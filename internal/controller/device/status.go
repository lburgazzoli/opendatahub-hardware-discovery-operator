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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

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
		ObservedGeneration: device.Generation,
		AllocationPlane:    string(identity.Plane),
		CollectedAt:        &metav1.Time{Time: observation.CollectedAt},
		SourceRef:          sourceRef(device.Spec.Source.Kind, identity.Locator),
		Accelerator:        acceleratorStatus(observation.Facts),
	}
	// Copy before mutating: meta.SetStatusCondition updates in place,
	// and the old conditions must stay intact for change detection.
	conditions := append([]metav1.Condition(nil), device.Status.Conditions...)
	setObservedCondition(&conditions, device.Generation, metav1.ConditionTrue,
		reasonObserved, fmt.Sprintf("observed facts from source %q", device.Spec.Source.Kind))

	factsChanged := !apiequality.Semantic.DeepEqual(device.Status.Accelerator, status.Accelerator) ||
		device.Status.AllocationPlane != status.AllocationPlane ||
		device.Status.SourceRef != status.SourceRef ||
		device.Status.ObservedGeneration != status.ObservedGeneration
	conditionChanged := !equalConditions(device.Status.Conditions, conditions)

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

	status.Conditions = conditions
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
	// Copy before mutating: meta.SetStatusCondition updates in place,
	// and the old conditions must stay intact for change detection.
	conditions := append([]metav1.Condition(nil), device.Status.Conditions...)
	setObservedCondition(&conditions, device.Generation, metav1.ConditionFalse,
		classifyObserveError(observeErr), observeErr.Error())

	if !equalConditions(device.Status.Conditions, conditions) {
		device.Status.Conditions = conditions
		if err := r.Status().Update(ctx, device); err != nil {
			return ctrl.Result{}, fmt.Errorf(
				"device: updating status of hardware device %q: %w", device.Name, err)
		}
	}
	// Input events (node, config, spec) drive faster retries; this
	// requeue is the periodic safety net.
	return ctrl.Result{RequeueAfter: defaultMaxStale}, nil
}

// setObservedCondition records the observation condition on top of the
// existing conditions; unchanged conditions keep their last transition
// time.
func setObservedCondition(
	conditions *[]metav1.Condition,
	generation int64,
	conditionStatus metav1.ConditionStatus,
	reason, message string,
) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               ConditionTypeObserved,
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

// equalConditions compares condition lists for content equality,
// ignoring the last transition times (unchanged conditions keep them).
func equalConditions(oldConditions []metav1.Condition, newConditions []metav1.Condition) bool {
	if len(oldConditions) != len(newConditions) {
		return false
	}
	for i := range oldConditions {
		if oldConditions[i].Type != newConditions[i].Type ||
			oldConditions[i].Status != newConditions[i].Status ||
			oldConditions[i].Reason != newConditions[i].Reason ||
			oldConditions[i].Message != newConditions[i].Message ||
			oldConditions[i].ObservedGeneration != newConditions[i].ObservedGeneration {
			return false
		}
	}
	return true
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
