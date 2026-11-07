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
	"errors"
	"fmt"
	"slices"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	fwconditions "github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/dra"
)

// Condition types of the singleton status.
const (
	// ConditionTypeReady reports whether the desired inventory is
	// reconciled from trustworthy evidence.
	ConditionTypeReady = "Ready"
	// ConditionTypeDegraded reports whether the collector is impaired.
	ConditionTypeDegraded = "Degraded"
)

// Condition reasons (design doc §5.2). They describe the collector and
// its observations, never hardware health or free allocation capacity.
const (
	reasonReconciled           = "Reconciled"
	reasonSourcesUnavailable   = "SourcesUnavailable"
	reasonSourceForbidden      = "SourceForbidden"
	reasonPoolIncomplete       = "PoolIncomplete"
	reasonNameCollision        = "NameCollision"
	reasonIdentityAmbiguous    = "IdentityAmbiguous"
	reasonInvalidConfiguration = "InvalidConfiguration"
)

// Issue priorities: the most severe issue decides the condition reason.
const (
	priorityInvalidConfiguration = iota
	prioritySourceForbidden
	priorityPoolIncomplete
	prioritySourcesUnavailable
	priorityIdentityAmbiguous
	priorityNameCollision
)

// issue is one actionable reconciliation problem. Conditions describe the
// collector and its observations, never hardware health, and no
// zero-device success is reported when evidence is missing.
type issue struct {
	reason   string
	priority int
	message  string
}

// issueLog collects issues in a deterministic order.
type issueLog struct {
	issues []issue
}

// add records a formatted issue.
func (l *issueLog) add(reason string, priority int, format string, args ...any) {
	l.issues = append(l.issues, issue{reason: reason, priority: priority, message: fmt.Sprintf(format, args...)})
}

// addIssue records a pre-built issue.
func (l *issueLog) addIssue(i issue) {
	l.issues = append(l.issues, i)
}

// nodeIssue explains a node's Unknown selection, or returns nil when the
// unknown outcome is benign: every registered source evaluated the node
// successfully and none claims it, so the node's inventory is simply not
// covered (for example a node without GPU evidence). A node that no
// source could even evaluate — the DRA API is missing or forbidden — is
// not benign: the failure is the actionable cause.
func nodeIssue(res nodeResult) *issue {
	if res.selection.Outcome != discovery.OutcomeUnknown {
		return nil
	}
	if err := res.selection.Err; !errors.Is(err, discovery.ErrNoPlaneEvidence) {
		return new(classifyIssue(fmt.Errorf("node %q inventory is unknown: %w", res.node.Name, err)))
	}
	for _, id := range sortedEnumerationIDs(res.enumerated) {
		if enum := res.enumerated[id]; enum.Outcome == discovery.OutcomeUnknown {
			return new(classifyIssue(fmt.Errorf("node %q could not be evaluated by source %q: %w",
				res.node.Name, id, enum.Err)))
		}
	}
	return nil
}

// classifyIssue maps an error to the approved condition reason. Errors
// are classified with errors.Is and the API-status helpers, never by
// matching message text.
func classifyIssue(err error) issue {
	switch {
	case errors.Is(err, dra.ErrPoolIncomplete):
		return issue{reason: reasonPoolIncomplete, priority: priorityPoolIncomplete, message: err.Error()}
	case apierrors.IsForbidden(err):
		return issue{reason: reasonSourceForbidden, priority: prioritySourceForbidden, message: err.Error()}
	default:
		return issue{reason: reasonSourcesUnavailable, priority: prioritySourcesUnavailable, message: err.Error()}
	}
}

// sortedEnumerationIDs returns the enumerated source ids in order.
func sortedEnumerationIDs(enumerated map[string]discovery.Enumeration) []string {
	ids := make([]string, 0, len(enumerated))
	for id := range enumerated {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// writeStatus writes the singleton status when it changed. Conditions
// carry the observed generation; unchanged conditions keep their last
// transition time, so an unchanged status is never rewritten and cannot
// loop the watch.
func (r *Reconciler) writeStatus(
	ctx context.Context,
	single *infrastructurev1alpha1.HardwareDiscovery,
	selected []nodeResult,
	state *inventoryState,
	issues []issue,
) error {
	status := buildStatus(single.Generation, single.GetConditions(), selected, state, issues)
	if apiequality.Semantic.DeepEqual(single.Status, status) {
		return nil
	}
	single.Status = status
	if err := r.Status().Update(ctx, single); err != nil {
		return fmt.Errorf("inventory: updating singleton status: %w", err)
	}
	return nil
}

// buildStatus assembles the singleton status: one summary per selected
// node plus Ready/Degraded conditions. Existing conditions are the base
// of the update so unchanged ones keep their last transition time.
func buildStatus(
	generation int64,
	existing []fwapi.Condition,
	selected []nodeResult,
	state *inventoryState,
	issues []issue,
) infrastructurev1alpha1.HardwareDiscoveryStatus {
	status := infrastructurev1alpha1.HardwareDiscoveryStatus{
		Status: fwapi.Status{
			ObservedGeneration: generation,
		},
	}
	for _, res := range selected {
		summary := infrastructurev1alpha1.NodeSummary{
			Name:        res.node.Name,
			UID:         res.node.UID,
			Plane:       string(res.selection.Plane),
			DeviceCount: 0,
			Outcome:     string(res.selection.Outcome),
		}
		if state != nil {
			summary.DeviceCount = state.countByNode(res.node.Name)
		}
		status.NodeSummaries = append(status.NodeSummaries, summary)
	}

	status.SetConditions(existing)
	if top := topIssue(issues); top != nil {
		fwconditions.SetStatusCondition(&status, fwapi.Condition{
			Type:               ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             top.reason,
			Message:            top.message,
			ObservedGeneration: generation,
		})
		fwconditions.SetStatusCondition(&status, fwapi.Condition{
			Type:               ConditionTypeDegraded,
			Status:             metav1.ConditionTrue,
			Reason:             top.reason,
			Message:            top.message,
			ObservedGeneration: generation,
		})
	} else {
		message := fmt.Sprintf("reconciled %d selected nodes and %d owned hardware devices",
			len(selected), ownedCount(state))
		fwconditions.SetStatusCondition(&status, fwapi.Condition{
			Type:               ConditionTypeReady,
			Status:             metav1.ConditionTrue,
			Reason:             reasonReconciled,
			Message:            message,
			ObservedGeneration: generation,
		})
		fwconditions.SetStatusCondition(&status, fwapi.Condition{
			Type:               ConditionTypeDegraded,
			Status:             metav1.ConditionFalse,
			Reason:             reasonReconciled,
			Message:            message,
			ObservedGeneration: generation,
		})
	}
	return status
}

// topIssue returns the most severe issue, or nil when there is none.
// Ties keep the first recorded issue, so the result is deterministic.
func topIssue(issues []issue) *issue {
	var top *issue
	for i := range issues {
		if top == nil || issues[i].priority < top.priority {
			top = &issues[i]
		}
	}
	return top
}

// ownedCount returns the owned device count, tolerating a nil state.
func ownedCount(state *inventoryState) int {
	if state == nil {
		return 0
	}
	return len(state.ours)
}
