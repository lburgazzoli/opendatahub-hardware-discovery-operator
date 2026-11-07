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

package inventory

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	fwactions "github.com/opendatahub-io/odh-platform-utilities/framework/controller/actions"
	odherrors "github.com/opendatahub-io/odh-platform-utilities/framework/controller/actions/errors"
	fwconditions "github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	fwtypes "github.com/opendatahub-io/odh-platform-utilities/framework/controller/types"
)

// extensionReadyMessage carries the scan's success summary from the scan
// action to the post-status hook through the request-local extensions.
const extensionReadyMessage = ControllerName + "/ready-message"

// scanDeps holds the scan dependencies that do not come from the
// reconciliation request.
type scanDeps struct {
	// recorder emits lifecycle events on children and configuration
	// issues on the singleton (events.k8s.io/v1 semantics; the action is
	// the UpperCamelCase verb of what this controller did to the
	// regarding object). Required.
	recorder events.EventRecorder
	// sources builds the ordered source registry for one singleton
	// configuration. Required.
	sources SourceFactory
	// clock is the collector clock used for grace bookkeeping. Defaults
	// to time.Now; tests inject a fake.
	clock func() time.Time
}

// scanAction returns the inventory scan action: one bounded scan per
// reconciliation. Expected configuration and evidence problems surface
// as the Degraded condition and keep a scheduled success; transient
// read/write failures preserve their %w chains, mark Degraded unhealthy
// and fall back to normal controller-runtime backoff.
func scanAction(deps scanDeps) fwactions.Fn {
	return func(ctx context.Context, rr *fwtypes.ReconciliationRequest) error {
		scan := &scanner{
			client:   rr.Client,
			scheme:   rr.Client.Scheme(),
			recorder: deps.recorder,
			sources:  deps.sources,
			clock:    deps.clock,
		}
		return scan.run(ctx, rr)
	}
}

// run executes one scan against the reconciliation request's instance.
func (s *scanner) run(ctx context.Context, rr *fwtypes.ReconciliationRequest) error {
	single, ok := rr.Instance.(*infrastructurev1alpha1.HardwareDiscovery)
	if !ok {
		return fmt.Errorf("inventory: instance %T is not a HardwareDiscovery", rr.Instance)
	}
	generation := single.GetGeneration()

	cfg, err := s.evaluateConfig(single)
	if err != nil {
		// Invalid configuration never prunes children: the evaluation is
		// not trustworthy. Surface an actionable condition instead and
		// keep scanning on the configured period.
		classified := issue{
			reason:   reasonInvalidConfiguration,
			priority: priorityInvalidConfiguration,
			message:  fmt.Sprintf("invalid discovery configuration: %v", err),
		}
		markDegraded(rr.Conditions, generation, classified)
		single.Status.NodeSummaries = nil
		s.recorder.Eventf(single, nil, corev1.EventTypeWarning, reasonInvalidConfiguration, "Reconciled",
			"invalid discovery configuration: %v", err)
		return odherrors.NewRequeueAfterError(cfg.rescanPeriod)
	}

	nodes := &corev1.NodeList{}
	if err := rr.Client.List(ctx, nodes); err != nil {
		return s.failScan(rr, generation, fmt.Errorf("inventory: listing nodes: %w", err))
	}
	devices := &infrastructurev1alpha1.HardwareDeviceList{}
	if err := rr.Client.List(ctx, devices); err != nil {
		return s.failScan(rr, generation, fmt.Errorf("inventory: listing hardware devices: %w", err))
	}

	state := newState(single, devices.Items)
	issues := issueLog{}
	selected := s.selectNodes(ctx, cfg, nodes.Items, &issues)
	if err := ctx.Err(); err != nil {
		// A cancelled scan must never be written as an authoritative
		// partial evaluation.
		return s.failScan(rr, generation, fmt.Errorf("inventory: scan cancelled: %w", err))
	}
	// The ambiguous id set feeds issue reporting only: ambiguous
	// identities are already rejected from the desired set.
	desired, _ := desiredIdentities(selected, &issues)

	// Deletion before creation: names freed by pruning become available
	// to new incarnations in the same pass.
	requeue, err := s.prune(ctx, cfg, state, selected, desired, nodes.Items)
	if err != nil {
		return s.failScan(rr, generation, err)
	}
	if err := s.materialize(ctx, single, cfg, state, desired, &issues); err != nil {
		return s.failScan(rr, generation, err)
	}

	single.Status.NodeSummaries = nodeSummaries(selected, state)
	if top := topIssue(issues.issues); top != nil {
		markDegraded(rr.Conditions, generation, *top)
	} else {
		message := fmt.Sprintf("reconciled %d selected nodes and %d owned hardware devices",
			len(selected), len(state.ours))
		// The request's extensions map is not pre-initialized by the
		// framework's apply loop.
		if rr.Extensions == nil {
			rr.Extensions = make(map[string]any)
		}
		rr.Extensions[extensionReadyMessage] = message
		rr.Conditions.MarkFalse(fwapi.ConditionType(ConditionTypeDegraded),
			fwconditions.WithReason(reasonReconciled),
			fwconditions.WithMessage(message),
			fwconditions.WithObservedGeneration(generation),
		)
	}

	return odherrors.NewRequeueAfterError(requeue)
}

// failScan marks the scan's transient failure on Degraded so a stopped
// value never stays stale, then returns the error for normal backoff.
func (s *scanner) failScan(rr *fwtypes.ReconciliationRequest, generation int64, err error) error {
	markDegraded(rr.Conditions, generation, classifyIssue(err))
	return err
}

// markDegraded records an issue as the unhealthy Degraded condition: the
// framework aggregator carries its deterministic reason and message onto
// the derived Ready condition.
func markDegraded(manager *fwconditions.Manager, generation int64, issue issue) {
	manager.MarkTrue(fwapi.ConditionType(ConditionTypeDegraded),
		fwconditions.WithReason(issue.reason),
		fwconditions.WithMessage(issue.message),
		fwconditions.WithObservedGeneration(generation),
	)
}

// postStatus finalizes the singleton status after the framework computed
// conditions, phase and happiness but before the status apply. It stamps
// the current generation onto the derived Ready condition — the v0.4.0
// aggregator does not carry it — and, only when the aggregation is
// already happy, gives the Ready condition its success reason and
// message. An unhealthy Ready value is never overridden.
func postStatus(_ context.Context, rr *fwtypes.ReconciliationRequest, isHappy bool) error {
	single, ok := rr.Instance.(*infrastructurev1alpha1.HardwareDiscovery)
	if !ok {
		return fmt.Errorf("inventory: instance %T is not a HardwareDiscovery", rr.Instance)
	}
	generation := single.GetGeneration()

	if isHappy {
		message, _ := rr.Extensions[extensionReadyMessage].(string)
		fwconditions.SetStatusCondition(single, fwapi.Condition{
			Type:               ConditionTypeReady,
			Status:             metav1.ConditionTrue,
			Reason:             reasonReconciled,
			Message:            message,
			ObservedGeneration: generation,
		})
		return nil
	}

	if ready := fwconditions.FindStatusCondition(single, ConditionTypeReady); ready != nil {
		ready.ObservedGeneration = generation
		fwconditions.SetStatusCondition(single, *ready)
	}
	return nil
}
