/*
Copyright 2026 lburgazzoli.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package device

import (
	"context"
	"errors"
	"fmt"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	fwconditions "github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/dra"
)

// writeObservation persists a successful observation. Facts, plane,
// provenance and the observed generation are written through /status
// only when they changed, or as the maxStale heartbeat when the
// persisted collection time fell out of bounds; an unchanged fresh
// observation writes nothing at all.
func (r *Reconciler) writeObservation(
	ctx context.Context,
	device *infrastructurev1alpha1.HardwareDevice,
	cfg config,
	identity discovery.Identity,
	observation discovery.Observation,
) (ctrl.Result, error) {
	status := infrastructurev1alpha1.HardwareDeviceStatus{
		Status: fwapi.Status{
			ObservedGeneration: device.Generation,
		},
		AllocationPlane: string(identity.Plane),
		CollectedAt:     &metav1.Time{Time: observation.CollectedAt},
		SourceRef:       sourceRef(device.Spec.Source.Kind, identity.Locator),
		Accelerator:     acceleratorStatus(observation.Facts),
	}
	// Existing conditions are the base of the update so unchanged ones
	// keep their last transition time; the helper upserts on the copy and
	// reports whether the condition content changed.
	status.SetConditions(device.Status.GetConditions())
	conditionChanged := fwconditions.SetStatusCondition(&status, fwapi.Condition{
		Type:               ConditionTypeObserved,
		Status:             metav1.ConditionTrue,
		Reason:             reasonObserved,
		Message:            fmt.Sprintf("observed facts from source %q", device.Spec.Source.Kind),
		ObservedGeneration: device.Generation,
	})

	factsChanged := !apiequality.Semantic.DeepEqual(device.Status.Accelerator, status.Accelerator) ||
		device.Status.AllocationPlane != status.AllocationPlane ||
		device.Status.SourceRef != status.SourceRef ||
		device.Status.ObservedGeneration != status.ObservedGeneration

	// The heartbeat bound is evaluated on the collector clock: the new
	// observation stamp versus the persisted one. Timestamp sampling
	// alone is not a facts change — only exceeding maxStale rewrites.
	stale := device.Status.CollectedAt == nil ||
		observation.CollectedAt.Sub(device.Status.CollectedAt.Time) > cfg.maxStale

	if !factsChanged && !conditionChanged && !stale {
		// Unchanged fresh facts: no write. Requeue for the heartbeat
		// exactly when the bound is next exceeded. Not stale means the
		// age is within the bound, so the remaining time is positive —
		// except at the exact boundary, where a zero requeue would park
		// the device until an input event; requeue promptly there.
		remaining := cfg.maxStale - observation.CollectedAt.Sub(device.Status.CollectedAt.Time)
		if remaining <= 0 {
			remaining = time.Second
		}
		return ctrl.Result{RequeueAfter: remaining}, nil
	}

	device.Status = status
	if err := r.Status().Update(ctx, device); err != nil {
		return ctrl.Result{}, fmt.Errorf("device: updating status of hardware device %q: %w", device.Name, err)
	}
	return ctrl.Result{RequeueAfter: cfg.maxStale}, nil
}

// reportObservationFailure records a failed observation as the Observed
// condition. Facts, collection time, provenance and the observed
// generation of the last successful observation are all retained — a
// failure never makes stale facts look freshly collected — and an
// unchanged failure condition is not rewritten, so retries cannot loop
// on writes.
func (r *Reconciler) reportObservationFailure(
	ctx context.Context,
	device *infrastructurev1alpha1.HardwareDevice,
	observeErr error,
) (ctrl.Result, error) {
	// Copy before mutating: the helper upserts through the accessor, and
	// the persisted conditions must stay intact for change detection.
	status := device.Status
	status.SetConditions(device.Status.GetConditions())
	conditionChanged := fwconditions.SetStatusCondition(&status, fwapi.Condition{
		Type:               ConditionTypeObserved,
		Status:             metav1.ConditionFalse,
		Reason:             classifyObserveError(observeErr),
		Message:            observeErr.Error(),
		ObservedGeneration: device.Generation,
	})

	if conditionChanged {
		device.Status = status
		if err := r.Status().Update(ctx, device); err != nil {
			return ctrl.Result{}, fmt.Errorf(
				"device: updating status of hardware device %q: %w", device.Name, err)
		}
	}
	// Input events (node, config, spec) drive faster retries; this
	// requeue is the periodic safety net.
	return ctrl.Result{RequeueAfter: defaultMaxStale}, nil
}

// classifyObserveError maps an observation failure to the condition
// reason (design doc §6.5). Classification uses errors.Is and the API
// status helpers, never message text.
func classifyObserveError(err error) string {
	switch {
	case apierrors.IsForbidden(err):
		return reasonSourceForbidden
	case errors.Is(err, dra.ErrPoolIncomplete):
		return reasonPoolIncomplete
	default:
		return reasonSourceUnavailable
	}
}

// acceleratorStatus maps observed facts to the accelerator status
// struct. Facts are replaced wholesale — absent facts are omitted,
// never zero or defaulted — and the struct is nil when no accelerator
// facts were observed.
func acceleratorStatus(facts discovery.Facts) *infrastructurev1alpha1.AcceleratorDeviceStatus {
	if facts.Accelerator == nil {
		return nil
	}
	status := &infrastructurev1alpha1.AcceleratorDeviceStatus{
		Vendor: facts.Accelerator.Vendor,
		Model:  facts.Accelerator.Model,
	}
	if facts.Accelerator.Memory != nil {
		memory := *facts.Accelerator.Memory
		status.Memory = &memory
	}
	return status
}
