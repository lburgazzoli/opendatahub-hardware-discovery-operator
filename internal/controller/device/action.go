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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/inventory"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	fwactions "github.com/opendatahub-io/odh-platform-utilities/framework/controller/actions"
	odherrors "github.com/opendatahub-io/odh-platform-utilities/framework/controller/actions/errors"
	fwconditions "github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	fwtypes "github.com/opendatahub-io/odh-platform-utilities/framework/controller/types"
)

// extensionReadyMessage carries the observation's success summary from
// the observation action to the post-status hook through the
// request-local extensions.
const extensionReadyMessage = ControllerName + "/ready-message"

// observeDeps holds the observation dependencies that do not come from
// the reconciliation request.
type observeDeps struct {
	// sources builds the ordered source registry for one singleton
	// configuration. Required.
	sources SourceFactory
}

// observeAction returns the device observation action: one bounded
// evaluation per reconciliation. Ineligibility and expected source
// failures surface as the Observed condition and keep a scheduled
// success, so Ready=False is derived from the unhealthy dependent
// instead of a backoff loop; transient pre-observation read failures
// preserve their %w chains, mark the evaluation failed and fall back to
// normal controller-runtime backoff.
func observeAction(deps observeDeps) fwactions.Fn {
	return func(ctx context.Context, rr *fwtypes.ReconciliationRequest) error {
		device, ok := rr.Instance.(*infrastructurev1alpha1.HardwareDevice)
		if !ok {
			return fmt.Errorf("device: instance %T is not a HardwareDevice", rr.Instance)
		}
		return observe(ctx, rr, device, deps)
	}
}

// observe runs one device evaluation against the reconciliation
// request's instance.
func observe(
	ctx context.Context,
	rr *fwtypes.ReconciliationRequest,
	device *infrastructurev1alpha1.HardwareDevice,
	deps observeDeps,
) error {
	generation := device.GetGeneration()

	single := &infrastructurev1alpha1.HardwareDiscovery{}
	if err := rr.Client.Get(ctx, client.ObjectKey{Name: inventory.SingletonName}, single); err != nil {
		if apierrors.IsNotFound(err) {
			// Reset path: the owner reference lets real garbage
			// collection remove the children, and the inventory
			// controller owns that lifecycle. Until it happens there is
			// no configuration to observe against — a diagnostic, not a
			// transient failure.
			return notObserved(rr, generation, reasonConfigurationMissing,
				fmt.Sprintf("hardware discovery singleton %q is missing", inventory.SingletonName),
				defaultMaxStale)
		}
		return evaluationFailed(rr, generation, classifyObserveError(err),
			fmt.Errorf("device: fetching singleton %q: %w", inventory.SingletonName, err))
	}

	cfg, err := evaluateConfig(single, deps.sources)
	if err != nil {
		// An invalid singleton configuration is not trustworthy: no
		// observation runs against it. The heartbeat bound survives —
		// evaluateConfig preserves a positive spec.maxStale even when
		// another field is invalid — and the config watch re-enqueues
		// every device once it is fixed.
		return notObserved(rr, generation, reasonInvalidConfiguration,
			fmt.Sprintf("invalid discovery configuration: %v", err), cfg.maxStale)
	}

	reason, message, err := ineligibility(ctx, rr.Client, device, cfg)
	if err != nil {
		return evaluationFailed(rr, generation, classifyObserveError(err), err)
	}
	if reason != "" {
		// A stale queued request must never freshen a deselected,
		// different-incarnation or uncovered device: no observation, no
		// fact refresh. The inventory controller owns the lifecycle;
		// watches re-enqueue this device when the situation changes,
		// and this requeue is the safety net.
		return notObserved(rr, generation, reason, message, cfg.maxStale)
	}

	source, identity, err := resolveIdentity(device, cfg)
	if err != nil {
		// A malformed spec is an observable failure plus a contextual
		// reconcile error, never a silent successful observation. The
		// CEL rules make it unreachable through the API; surfacing it
		// keeps it loud.
		return evaluationFailed(rr, generation, reasonInvalidConfiguration, err)
	}

	observation, err := source.Observe(ctx, identity)
	if err == nil {
		// A contract-violating observation is a failed observation,
		// never a trusted partial one.
		err = observation.Validate()
	}
	if err != nil {
		// Source failures retain every fact, timestamp and provenance
		// of the last successful observation: a failure never makes
		// stale facts look freshly collected. The condition, not
		// backoff, is the reporting channel (design doc §6.5); input
		// events drive faster retries.
		return notObserved(rr, generation, classifyObserveError(err), err.Error(), cfg.maxStale)
	}

	message, requeue := recordObservation(device, cfg, identity, observation)
	rr.Conditions.MarkTrue(fwapi.ConditionType(ConditionTypeObserved),
		fwconditions.WithReason(reasonObserved),
		fwconditions.WithMessage(message),
		fwconditions.WithObservedGeneration(generation),
	)
	// The request's extensions map is not pre-initialized by the
	// framework's apply loop.
	if rr.Extensions == nil {
		rr.Extensions = make(map[string]any)
	}
	rr.Extensions[extensionReadyMessage] = message
	return odherrors.NewRequeueAfterError(requeue)
}

// notObserved records a diagnostic Observed=False — the specific reason
// the current evaluation cannot produce a trustworthy observation — and
// schedules the next evaluation, returning a scheduled-success requeue
// rather than an error: the framework still completes the pipeline, so
// Ready aggregates the unhealthy dependent into Ready=False and phase
// Not Ready instead of retaining a misleading old Ready=True.
func notObserved(
	rr *fwtypes.ReconciliationRequest,
	generation int64,
	reason string,
	message string,
	requeue time.Duration,
) error {
	rr.Conditions.MarkFalse(fwapi.ConditionType(ConditionTypeObserved),
		fwconditions.WithReason(reason),
		fwconditions.WithMessage(message),
		fwconditions.WithObservedGeneration(generation),
	)
	return odherrors.NewRequeueAfterError(requeue)
}

// evaluationFailed marks the Observed condition false so a stopped
// value never stays stale, then returns the error for the framework's
// normal backoff (ProvisioningSucceeded=False; Ready=False).
func evaluationFailed(rr *fwtypes.ReconciliationRequest, generation int64, reason string, err error) error {
	rr.Conditions.MarkFalse(fwapi.ConditionType(ConditionTypeObserved),
		fwconditions.WithReason(reason),
		fwconditions.WithMessage(err.Error()),
		fwconditions.WithObservedGeneration(generation),
	)
	return err
}

// postStatus finalizes the device status after the framework computed
// conditions, phase and happiness but before the status apply. It
// stamps the current generation onto the derived Ready condition — the
// pinned aggregator does not carry it — and, only when the aggregation
// is already happy, gives the Ready condition its success reason and
// message. An unhealthy Ready value is never overridden.
func postStatus(_ context.Context, rr *fwtypes.ReconciliationRequest, isHappy bool) error {
	device, ok := rr.Instance.(*infrastructurev1alpha1.HardwareDevice)
	if !ok {
		return fmt.Errorf("device: instance %T is not a HardwareDevice", rr.Instance)
	}
	generation := device.GetGeneration()

	if isHappy {
		message, _ := rr.Extensions[extensionReadyMessage].(string)
		fwconditions.SetStatusCondition(device, fwapi.Condition{
			Type:               string(fwapi.ConditionTypeReady),
			Status:             metav1.ConditionTrue,
			Reason:             reasonObserved,
			Message:            message,
			ObservedGeneration: generation,
		})
		return nil
	}

	if ready := fwconditions.FindStatusCondition(device, string(fwapi.ConditionTypeReady)); ready != nil {
		ready.ObservedGeneration = generation
		fwconditions.SetStatusCondition(device, *ready)
	}
	return nil
}

// ineligibility explains why the device must not be observed right now:
// its spec does not validate against the current configuration and
// cluster state, so a stale queued request cannot freshen a deselected,
// different-incarnation or uncovered device. The empty reason means the
// device is eligible; a non-nil error means the evaluation itself
// failed transiently.
func ineligibility(
	ctx context.Context,
	cluster client.Client,
	device *infrastructurev1alpha1.HardwareDevice,
	cfg config,
) (string, string, error) {
	if device.Spec.Class != cfg.class {
		// Class coverage moved: not this controller's call to prune or
		// to keep observing a class the singleton no longer discovers.
		return reasonClassMismatch, fmt.Sprintf(
			"device class %q is not the singleton's discovered class %q",
			device.Spec.Class, cfg.class), nil
	}
	if sourceByID(cfg.sources, device.Spec.Source.Kind) == nil {
		// The device's source is disabled in the current
		// configuration: no trustworthy observation is possible.
		return reasonSourceDisabled, fmt.Sprintf(
			"source %q is disabled by the singleton configuration",
			device.Spec.Source.Kind), nil
	}

	node := &corev1.Node{}
	if err := cluster.Get(ctx, client.ObjectKey{Name: device.Spec.Host.Name}, node); err != nil {
		if apierrors.IsNotFound(err) {
			// Absent node: retention and grace belong to the inventory
			// controller; there is nothing here to observe.
			return reasonNodeNotFound, fmt.Sprintf(
				"host node %q is absent", device.Spec.Host.Name), nil
		}
		return "", "", fmt.Errorf("device: fetching node %q: %w", device.Spec.Host.Name, err)
	}
	if node.UID != device.Spec.Host.UID {
		// Different node incarnation under the same name: the spec
		// belongs to the old incarnation, and observations must never
		// freshen it or transfer facts to the new one.
		return reasonNodeUIDMismatch, fmt.Sprintf(
			"host node %q was recreated (spec uid %q, live uid %q)",
			device.Spec.Host.Name, device.Spec.Host.UID, node.UID), nil
	}
	if !cfg.selector.Matches(labels.Set(node.Labels)) {
		// Deliberately deselected: the inventory controller decides the
		// lifecycle; observation stands down.
		return reasonNodeDeselected, fmt.Sprintf(
			"host node %q is not selected by the singleton node selector",
			device.Spec.Host.Name), nil
	}
	return "", "", nil
}
