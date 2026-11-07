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
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"

	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/dra"
)

// scanner executes one inventory scan: it owns the singleton's device
// children through the request's framework client and reports problems
// through the request's condition manager. The framework reconciler
// persists the singleton status; this type never writes it directly.
type scanner struct {
	client   client.Client
	scheme   *runtime.Scheme
	recorder events.EventRecorder
	sources  SourceFactory
	clock    func() time.Time
}

// now returns the collector clock, defaulting to the system clock.
func (s *scanner) now() time.Time {
	if s.clock == nil {
		return time.Now()
	}
	return s.clock()
}

// config is the validated singleton configuration of one scan.
type config struct {
	class        infrastructurev1alpha1.DeviceClass
	selector     labels.Selector
	sources      discovery.Sources
	rescanPeriod time.Duration
	gracePeriod  time.Duration
}

// evaluateConfig validates the singleton spec and builds the scan
// configuration. Node selection defaults to every node; a nil or empty
// LabelSelector matches all eligible nodes. Durations fall back to the
// approved defaults when omitted or non-positive.
func (s *scanner) evaluateConfig(single *infrastructurev1alpha1.HardwareDiscovery) (config, error) {
	cfg := config{
		rescanPeriod: durationOr(single.Spec.RescanPeriod.Duration, defaultRescanPeriod),
		gracePeriod:  durationOr(single.Spec.NodeGracePeriod.Duration, defaultNodeGracePeriod),
	}
	if single.Spec.Class != infrastructurev1alpha1.DeviceClassAccelerator {
		return cfg, fmt.Errorf("spec.class %q is not the enabled device class %q",
			single.Spec.Class, infrastructurev1alpha1.DeviceClassAccelerator)
	}
	cfg.class = single.Spec.Class

	selector, err := nodeSelector(single.Spec.NodeSelector)
	if err != nil {
		return cfg, fmt.Errorf("spec.nodeSelector is invalid: %w", err)
	}
	cfg.selector = selector

	sources, err := s.sources(single.Spec.Sources)
	if err != nil {
		return cfg, err
	}
	cfg.sources = sources
	return cfg, nil
}

// nodeSelector resolves the spec selector. An omitted or empty selector
// matches all nodes; metav1.LabelSelectorAsSelector alone would turn an
// explicit empty selector into "nothing", which is not the approved
// default.
func nodeSelector(spec *metav1.LabelSelector) (labels.Selector, error) {
	if spec == nil || len(spec.MatchLabels) == 0 && len(spec.MatchExpressions) == 0 {
		return labels.Everything(), nil
	}
	return metav1.LabelSelectorAsSelector(spec)
}

// durationOr returns d when positive, otherwise the fallback.
func durationOr(d time.Duration, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

// deviceKey is the matching key of one device incarnation: the device id,
// the identity kind and the hosting Node UID. Existing devices are matched
// by the full key, so a kind change or a node replacement is never an
// in-place update of an owned object.
type deviceKey struct {
	id      string
	kind    discovery.IdentityKind
	nodeUID types.UID
}

// deviceKeyOf returns the matching key of a discovered identity.
func deviceKeyOf(identity discovery.Identity) deviceKey {
	return deviceKey{id: identity.ID, kind: identity.Kind, nodeUID: identity.NodeUID}
}

// deviceKeyOfSpec returns the matching key of an existing device spec.
func deviceKeyOfSpec(spec infrastructurev1alpha1.HardwareDeviceSpec) deviceKey {
	return deviceKey{
		id:      spec.Identity.ID,
		kind:    discovery.IdentityKind(spec.Identity.Kind),
		nodeUID: spec.Host.UID,
	}
}

// nodeResult is the discovery decision of one selected node.
type nodeResult struct {
	node       *corev1.Node
	selection  discovery.NodeSelection
	enumerated map[string]discovery.Enumeration
}

// inventoryState is the working set of HardwareDevice objects. ours holds
// the devices controlled by this singleton instance; everything else is
// foreign and never touched.
type inventoryState struct {
	singleton *infrastructurev1alpha1.HardwareDiscovery
	all       map[string]*infrastructurev1alpha1.HardwareDevice
	ours      map[string]*infrastructurev1alpha1.HardwareDevice
}

// newState partitions the listed devices into this singleton's children
// and foreign objects. A controller reference with a stale singleton UID
// (the singleton was deleted and recreated) is foreign: real garbage
// collection owns those objects, and this controller never adopts them.
func newState(single *infrastructurev1alpha1.HardwareDiscovery, items []infrastructurev1alpha1.HardwareDevice) *inventoryState {
	state := &inventoryState{
		singleton: single,
		all:       make(map[string]*infrastructurev1alpha1.HardwareDevice, len(items)),
		ours:      make(map[string]*infrastructurev1alpha1.HardwareDevice),
	}
	for i := range items {
		device := &items[i]
		state.all[device.Name] = device
		if isControlledBy(device, single) {
			state.ours[device.Name] = device
		}
	}
	return state
}

// isControlledBy reports whether the device's controller reference points
// at exactly this singleton instance.
func isControlledBy(device *infrastructurev1alpha1.HardwareDevice, single *infrastructurev1alpha1.HardwareDiscovery) bool {
	ref := metav1.GetControllerOf(device)
	return ref != nil &&
		ref.APIVersion == infrastructurev1alpha1.GroupVersion.String() &&
		ref.Kind == "HardwareDiscovery" &&
		ref.Name == single.Name &&
		ref.UID == single.UID
}

// add registers a newly created owned device.
func (s *inventoryState) add(device *infrastructurev1alpha1.HardwareDevice) {
	s.all[device.Name] = device
	s.ours[device.Name] = device
}

// remove forgets a pruned device.
func (s *inventoryState) remove(name string) {
	delete(s.all, name)
	delete(s.ours, name)
}

// ownedByKey finds the owned device carrying the exact device key. Lookups
// are derived from the working set, so they always reflect creations and
// deletions of the current pass.
func (s *inventoryState) ownedByKey(key deviceKey) *infrastructurev1alpha1.HardwareDevice {
	for _, device := range s.ours {
		if deviceKeyOfSpec(device.Spec) == key {
			return device
		}
	}
	return nil
}

// takenNames returns the object names held by owned devices, mapped to
// the identity id of the holder; it seeds the deterministic name
// resolution of new identities.
func (s *inventoryState) takenNames() map[string]string {
	taken := make(map[string]string, len(s.ours))
	for name, device := range s.ours {
		taken[name] = device.Spec.Identity.ID
	}
	return taken
}

// countByNode counts the owned devices of one node.
func (s *inventoryState) countByNode(nodeName string) int32 {
	var count int32
	for _, device := range s.ours {
		if device.Spec.Host.Name == nodeName {
			count++
		}
	}
	return count
}

// selectNodes resolves the selection of every node matching the
// configured selector, in node name order. Each source is enumerated per
// node (per-node outcomes drive pruning and the node summaries), and the
// SelectNode fold decides the authoritative enumeration. Unknown outcomes
// are classified into actionable issues; a node that no source claims
// while every source evaluated it successfully is unknown but benign.
func (s *scanner) selectNodes(
	ctx context.Context,
	cfg config,
	items []corev1.Node,
	issues *issueLog,
) []nodeResult {
	slices.SortFunc(items, func(a, b corev1.Node) int {
		return strings.Compare(a.Name, b.Name)
	})

	selected := make([]nodeResult, 0)
	for i := range items {
		node := &items[i]
		if !cfg.selector.Matches(labels.Set(node.Labels)) {
			continue
		}
		res := s.selectNode(ctx, node, cfg.sources)
		if res.selection.Outcome == discovery.OutcomeUnknown {
			if issue := nodeIssue(res); issue != nil {
				issues.addIssue(*issue)
			}
		}
		selected = append(selected, res)
	}
	return selected
}

// selectNode enumerates every registered source for one node and folds
// their claims with SelectNode. Sources that fail to even classify their
// enumeration (invalid arguments, context cancellation) contribute an
// Unknown outcome; a contract-violating enumeration is downgraded to
// Unknown rather than trusted.
func (s *scanner) selectNode(
	ctx context.Context,
	node *corev1.Node,
	sources discovery.Sources,
) nodeResult {
	info := discovery.NodeInfo{Name: node.Name, UID: node.UID}
	enumerated := make(map[string]discovery.Enumeration, sources.Len())
	for _, src := range sources.List() {
		enum, err := src.Enumerate(ctx, []discovery.NodeInfo{info})
		if err != nil {
			// Reserved for invalid arguments and context cancellation:
			// the outcome is unknown, never authoritative.
			enum = discovery.Enumeration{Outcome: discovery.OutcomeUnknown, Err: err}
		} else if err := enum.Validate(); err != nil {
			// A contract-violating enumeration is downgraded to Unknown
			// rather than trusted.
			enum = discovery.Enumeration{Outcome: discovery.OutcomeUnknown, Err: err}
		}
		enumerated[src.ID()] = enum
	}
	return nodeResult{
		node:       node,
		selection:  discovery.SelectNode(ctx, info, sources, enumerated),
		enumerated: enumerated,
	}
}

// desiredIdentities folds the authoritative selections into the desired
// device set and reports the ids that are ambiguous across nodes. Unknown
// selections contribute nothing (a partially visible snapshot is never a
// partial hardware set), identities that fail their contract validation
// are reported as issues, and an id reported by more than one node is
// rejected everywhere: one incarnation is never silently picked.
func desiredIdentities(selected []nodeResult, issues *issueLog) (map[deviceKey]discovery.Identity, map[string]struct{}) {
	idCounts := make(map[string]int)
	for _, res := range selected {
		if res.selection.Outcome != discovery.OutcomeSuccess {
			continue
		}
		for _, identity := range res.selection.Identities {
			idCounts[identity.ID]++
		}
	}

	ambiguous := make(map[string]struct{})
	for id, count := range idCounts {
		if count > 1 {
			ambiguous[id] = struct{}{}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(ambiguous)) {
		issues.add(reasonIdentityAmbiguous, priorityIdentityAmbiguous,
			"device identity %q is reported by %d nodes; every incarnation is rejected", id, idCounts[id])
	}

	desired := make(map[deviceKey]discovery.Identity)
	for _, res := range selected {
		if res.selection.Outcome != discovery.OutcomeSuccess {
			continue
		}
		identities := slices.Clone(res.selection.Identities)
		slices.SortFunc(identities, func(a, b discovery.Identity) int {
			return strings.Compare(a.ID, b.ID)
		})
		for _, identity := range identities {
			if _, rejected := ambiguous[identity.ID]; rejected {
				continue
			}
			if err := identity.Validate(); err != nil {
				issues.add(reasonSourcesUnavailable, prioritySourcesUnavailable,
					"node %q produced an invalid identity: %v", res.node.Name, err)
				continue
			}
			desired[deviceKeyOf(identity)] = identity
		}
	}
	return desired, ambiguous
}

// prune deletes the owned devices that authoritative evidence no longer
// covers and stamps or expires the node grace of orphaned devices. It
// returns the requeue delay: the rescan period, or the earliest grace
// deadline when it comes first.
func (s *scanner) prune(
	ctx context.Context,
	cfg config,
	state *inventoryState,
	selected []nodeResult,
	desired map[deviceKey]discovery.Identity,
	nodes []corev1.Node,
) (time.Duration, error) {
	nodeByName := make(map[string]*corev1.Node, len(nodes))
	for i := range nodes {
		nodeByName[nodes[i].Name] = &nodes[i]
	}
	resultByName := make(map[string]nodeResult, len(selected))
	for _, res := range selected {
		resultByName[res.node.Name] = res
	}

	type pruneAction struct {
		device *infrastructurev1alpha1.HardwareDevice
		cause  string
	}
	var prunes []pruneAction
	var stamps []*infrastructurev1alpha1.HardwareDevice
	requeue := cfg.rescanPeriod
	now := s.now()

	for _, name := range slices.Sorted(maps.Keys(state.ours)) {
		device := state.ours[name]
		node, exists := nodeByName[device.Spec.Host.Name]
		switch {
		case !exists || node.UID != device.Spec.Host.UID:
			// The hosting node is absent or was recreated: the device is
			// orphaned. Grace bookkeeping persists in the annotation so a
			// restart cannot reset it (gate decision D5).
			deadline, ok := orphanDeadline(device)
			switch {
			case !ok:
				stamps = append(stamps, device)
				if remaining := cfg.gracePeriod; remaining < requeue {
					requeue = remaining
				}
			case !now.Before(deadline.Add(cfg.gracePeriod)):
				prunes = append(prunes, pruneAction{device, "node grace expired"})
			default:
				if remaining := deadline.Add(cfg.gracePeriod).Sub(now); remaining < requeue {
					requeue = remaining
				}
			}
		case !cfg.selector.Matches(labels.Set(node.Labels)):
			// Deliberate selector removal with a trustworthy node
			// evaluation: prune immediately.
			prunes = append(prunes, pruneAction{device, "node no longer selected"})
		default:
			res, isResult := resultByName[node.Name]
			if !isResult || res.selection.Outcome != discovery.OutcomeSuccess {
				// Unknown inventory: retain, and never annotate — the
				// node is present. One narrow exception, the no-claim
				// zero-advertisement case: when no registered source
				// claims the node, a source whose own successful read
				// found zero of its devices proves those devices gone.
				// A selected source's Unknown outcome takes precedence
				// over this exception — loss of evidence never prunes —
				// and an unclaimed node's empty slice is never
				// disappearance proof for source kinds whose claim rule
				// differs from their zero-proof (classic).
				if isResult && sourceAuthoritativelyEmpty(res, string(device.Spec.Source.Kind)) {
					prunes = append(prunes, pruneAction{device, "authoritative disappearance"})
				}
				continue
			}
			if _, wanted := desired[deviceKeyOfSpec(device.Spec)]; !wanted {
				prunes = append(prunes, pruneAction{device, "authoritative disappearance"})
			}
		}
	}

	for _, action := range prunes {
		if err := s.client.Delete(ctx, action.device); err != nil && !apierrors.IsNotFound(err) {
			return requeue, fmt.Errorf("inventory: pruning hardware device %q: %w", action.device.Name, err)
		}
		state.remove(action.device.Name)
		s.recorder.Eventf(action.device, nil, corev1.EventTypeNormal, "DevicePruned", "Deleted",
			"deleted hardware device: %s", action.cause)
	}

	for _, device := range stamps {
		stampAnnotation(device, now)
		if err := s.client.Update(ctx, device); err != nil {
			return requeue, fmt.Errorf("inventory: stamping node grace on hardware device %q: %w", device.Name, err)
		}
		s.recorder.Eventf(device, nil, corev1.EventTypeNormal, "NodeGraceStarted", "Retained",
			"hosting node is absent or was recreated; device is retained for %s", cfg.gracePeriod)
	}

	return requeue, nil
}

// sourceAuthoritativelyEmpty reports whether the device's backing source
// proved its devices gone on a node no registered source claims. Beyond
// the source's own successful empty enumeration of the node, two
// conditions hold. First, no registered source claims the node
// (ErrNoPlaneEvidence): a selected source's Unknown outcome — an
// incomplete DRA pool, a forbidden read — takes precedence over every
// empty-source exception, so loss of evidence never prunes. Second, a
// successful empty read must be authoritative disappearance evidence for
// the source kind: the DRA source's claim rule (advertisements exist) is
// separate from its zero-proof (a successful read that finds none), so
// its devices are pruned, while the classic source's empty enumeration
// of a node it does not claim — missing or malformed gpu.count, missing
// extended-resource evidence — is loss of evidence, never a proven zero,
// so classic devices are retained. A trustworthy classic zero (evidence
// plus a valid count of zero) claims the node and prunes through the
// ordinary authoritative-disappearance path instead. The exception never
// turns a never-claimed node into a zero-device claim: the node's
// selection stays Unknown and no identities are fabricated.
func sourceAuthoritativelyEmpty(res nodeResult, sourceKind string) bool {
	if !errors.Is(res.selection.Err, discovery.ErrNoPlaneEvidence) {
		return false
	}
	enum, ok := res.enumerated[sourceKind]
	if !ok || enum.Outcome != discovery.OutcomeSuccess || len(enum.Identities) != 0 {
		return false
	}
	return sourceKind == dra.SourceID
}

// orphanDeadline parses the persisted grace start of a device. A missing
// or malformed annotation reports ok=false: the caller stamps it fresh.
func orphanDeadline(device *infrastructurev1alpha1.HardwareDevice) (time.Time, bool) {
	raw, ok := device.Annotations[AnnotationNodeOrphanedAt]
	if !ok {
		return time.Time{}, false
	}
	deadline, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return deadline, true
}

// stampAnnotation records the grace start on a device.
func stampAnnotation(device *infrastructurev1alpha1.HardwareDevice, now time.Time) {
	if device.Annotations == nil {
		device.Annotations = make(map[string]string, 1)
	}
	device.Annotations[AnnotationNodeOrphanedAt] = now.UTC().Format(time.RFC3339)
}
