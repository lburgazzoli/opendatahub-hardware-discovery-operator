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

package device_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	// The blank import registers the Kubernetes object converters (the
	// typed client.Object → unstructured mapping) that the jq matchers
	// rely on.
	"github.com/lburgazzoli/gomega-matchers/pkg/matchers/jq"
	_ "github.com/lburgazzoli/gomega-matchers/pkg/matchers/k8s"
	"github.com/lburgazzoli/gomega-matchers/pkg/matchers/k8s/condition"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/classic"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/dra"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	fwconditions "github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
)

// ownedDevice builds a singleton-owned device with the well-known DRA
// spec and the given status, ready to be seeded into a fixture.
func ownedDevice(
	t *testing.T,
	single *infrastructurev1alpha1.HardwareDiscovery,
	status infrastructurev1alpha1.HardwareDeviceStatus,
) *infrastructurev1alpha1.HardwareDevice {
	t.Helper()
	device := seedDevice(t, testScheme(t), single, deviceNameGPU,
		draDeviceSpec(nodeAName, nodeAUID, poolWorker))
	device.Status = status
	return device
}

// seededStatus is a prior successful observation, recorded before the
// scenario clock starts moving.
func seededStatus(collectedAt time.Time) infrastructurev1alpha1.HardwareDeviceStatus {
	return expectedStatus(collectedAt, modelTesla)
}

// observedCluster is the well-known discoverable cluster: a matching
// node and an owned device with no status yet.
func observedCluster(t *testing.T) (*infrastructurev1alpha1.HardwareDiscovery, *corev1.Node, *infrastructurev1alpha1.HardwareDevice) {
	t.Helper()
	single := testSingleton()
	node := testNode(nodeAName, nodeAUID, map[string]string{selectorKeyPool: selectorPoolDefault})
	device := ownedDevice(t, single, infrastructurev1alpha1.HardwareDeviceStatus{})
	return single, node, device
}

// assertStatusMatches compares a device status against the expected
// document: the persisted facts are asserted as a JSON document through
// jq, and collectedAt compares by instant because the fake client
// re-materializes metav1.Time in the process time zone.
func assertStatusMatches(
	g *WithT,
	found *infrastructurev1alpha1.HardwareDevice,
	want infrastructurev1alpha1.HardwareDeviceStatus,
) {
	g.Expect(found).To(SatisfyAll(
		jq.Match(fmt.Sprintf(`.status.observedGeneration == %d`, want.ObservedGeneration)),
		jq.Match(fmt.Sprintf(`.status.allocationPlane == %q`, want.AllocationPlane)),
		jq.Match(fmt.Sprintf(`.status.sourceRef == %q`, want.SourceRef)),
		jq.Match(acceleratorExpr(want.Accelerator)),
	))
	if want.CollectedAt == nil {
		g.Expect(found.Status.CollectedAt).To(BeNil())
		return
	}
	g.Expect(found.Status.CollectedAt).NotTo(BeNil())
	g.Expect(found.Status.CollectedAt.Time.Equal(want.CollectedAt.Time)).To(BeTrue(),
		"collectedAt must match by instant")
}

// acceleratorExpr renders the jq comparison of the accelerator status
// document: optional facts the source does not publish are absent from
// the document (null), never zero or defaulted.
func acceleratorExpr(want *infrastructurev1alpha1.AcceleratorDeviceStatus) string {
	if want == nil {
		return `.status.accelerator == null`
	}
	expr := fmt.Sprintf(`.status.accelerator.model == %q`, want.Model)
	if want.Vendor != "" {
		expr += fmt.Sprintf(` and .status.accelerator.vendor == %q`, want.Vendor)
	} else {
		expr += ` and .status.accelerator.vendor == null`
	}
	if want.Memory != nil {
		expr += fmt.Sprintf(` and .status.accelerator.memory == %q`, want.Memory.String())
	} else {
		expr += ` and .status.accelerator.memory == null`
	}
	return expr
}

// assertHappyReadiness asserts the framework-derived success shape:
// Observed, ProvisioningSucceeded and Ready all True, phase Ready, and
// the top-level observed generation stamped.
func assertHappyReadiness(g *WithT, found *infrastructurev1alpha1.HardwareDevice, generation int64) {
	g.Expect(found.Status.Phase).To(Equal(phaseReady))
	g.Expect(found.Status.ObservedGeneration).To(Equal(generation))
	for _, conditionType := range []string{conditionTypeObserved, conditionTypeProv, conditionTypeReady} {
		cond := findCondition(g, found, conditionType)
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
			"condition %q must be True", conditionType)
	}
	ready := findCondition(g, found, conditionTypeReady)
	g.Expect(ready.Reason).To(Equal(reasonObserved))
	g.Expect(ready.ObservedGeneration).To(Equal(generation))
}

// assertUnhappyReadiness asserts the framework-derived failure shape:
// Ready False, phase Not Ready, and the top-level observed generation
// left at its prior value (the framework only stamps it when happy).
func assertUnhappyReadiness(g *WithT, found *infrastructurev1alpha1.HardwareDevice) {
	g.Expect(found.Status.Phase).To(Equal(phaseNotReady))
	ready := findCondition(g, found, conditionTypeReady)
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
}

// findCondition returns one condition and asserts its presence.
func findCondition(g *WithT, found *infrastructurev1alpha1.HardwareDevice, conditionType string) fwapi.Condition {
	cond := fwconditions.FindStatusCondition(found, conditionType)
	g.Expect(cond).NotTo(BeNil(), "condition %q must exist", conditionType)
	return *cond
}

// TestReconcileObservesDevice proves the happy path through the
// production ReconcilerFor/Build composition: the controller resolves
// exactly the spec identity and locator, observes through the source,
// and the framework persists facts/plane/provenance, the
// collector-clock stamp and the derived conditions through the status
// subresource — never the primary endpoint, with spec and metadata
// intact.
func TestReconcileObservesDevice(t *testing.T) {
	single, node, device := observedCluster(t)
	f := newFixture(t, single, node, device)
	g := NewWithT(t)
	src := f.draSource()
	src.facts = acceleratorFacts(modelTesla, memoryK80)

	result := f.mustReconcile(t, deviceNameGPU)
	g.Expect(result.RequeueAfter).To(Equal(maxStaleDefault))

	// The observation resolved exactly the spec identity and locator.
	g.Expect(src.observed).To(HaveLen(1))
	identity := src.observed[0]
	g.Expect(identity.ID).To(Equal(deviceIDGPU))
	g.Expect(string(identity.Kind)).To(Equal(identityKindUUID))
	g.Expect(identity.NodeName).To(Equal(nodeAName))
	g.Expect(identity.NodeUID).To(Equal(types.UID(nodeAUID)))
	g.Expect(identity.Plane).To(Equal(discovery.PlaneDRA))
	g.Expect(identity.Locator).To(Equal(dra.Locator{
		Driver: driverNVIDIA, Pool: poolWorker, Device: draDeviceName,
	}))

	// Status: facts, plane, provenance, collector-clock stamp, and the
	// aggregated readiness.
	found := f.getDevice(t, deviceNameGPU)
	assertStatusMatches(g, found, expectedStatus(f.clock.Now(), modelTesla))
	assertHappyReadiness(g, found, 1)
	g.Expect(found.Status.GetConditions()).To(ContainElement(SatisfyAll(
		condition.Is(conditionTypeObserved, metav1.ConditionTrue),
		condition.HasReason(reasonObserved),
		condition.HasMessage(`observed facts from source "dra"`),
		HaveField("ObservedGeneration", int64(1)),
	)))

	// Spec and metadata untouched; exactly one status apply; nothing
	// else.
	g.Expect(found.Spec).To(Equal(draDeviceSpec(nodeAName, nodeAUID, poolWorker)))
	g.Expect(found.Labels).To(Equal(device.Labels))
	g.Expect(found.OwnerReferences).To(Equal(device.OwnerReferences))
	g.Expect(found.Finalizers).To(BeEmpty())
	g.Expect(f.writes.statusApplies).To(HaveLen(1))
	f.writes.expectNoPrimaryWrites(t)
}

// TestReconcileObservesClassicDevice proves the classic locator arm:
// node-index identity, classic plane and the classic sourceRef format.
func TestReconcileObservesClassicDevice(t *testing.T) {
	single := testSingleton()
	node := testNode(nodeBName, nodeBUID, nil)
	name := nodeBName + "-gpu-0"
	device := seedDevice(t, testScheme(t), single, name,
		classicDeviceSpec(nodeBName, nodeBUID, 0, name))
	f := newFixture(t, single, node, device)
	g := NewWithT(t)
	src := f.classicSource()
	src.facts = acceleratorFacts(modelOther, memoryV100)

	result := f.mustReconcile(t, name)
	g.Expect(result.RequeueAfter).To(Equal(maxStaleDefault))

	g.Expect(src.observed).To(HaveLen(1))
	identity := src.observed[0]
	g.Expect(string(identity.Kind)).To(Equal(identityKindNodeIndex))
	g.Expect(identity.Plane).To(Equal(discovery.PlaneClassic))
	g.Expect(identity.Locator).To(Equal(classic.Locator{Node: nodeBName, Index: 0}))

	found := f.getDevice(t, name)
	g.Expect(found).To(SatisfyAll(
		jq.Match(`.status.observedGeneration == 1`),
		jq.Match(fmt.Sprintf(`.status.allocationPlane == %q`, sourceIDClassic)),
		jq.Match(fmt.Sprintf(`.status.sourceRef == %q`, sourceIDClassic+"/"+nodeBName)),
		jq.Match(fmt.Sprintf(`.status.accelerator.model == %q and .status.accelerator.memory == %q`,
			modelOther, memoryV100)),
	))
	g.Expect(found.Status.CollectedAt.Time.Equal(f.clock.Now())).To(BeTrue())
	assertHappyReadiness(g, found, 1)
	f.writes.expectNoPrimaryWrites(t)
}

// TestReconcileFactsChangeVsHeartbeat proves the collectedAt policy
// through the framework's always-apply status writer: unchanged fresh
// facts keep the persisted stamp (a repeated status SSA never resamples
// it) and requeue for the remaining bound; changed facts refresh while
// fresh; unchanged facts past maxStale refresh as the heartbeat.
func TestReconcileFactsChangeVsHeartbeat(t *testing.T) {
	single, node, device := observedCluster(t)
	f := newFixture(t, single, node, device)
	g := NewWithT(t)
	src := f.draSource()
	src.facts = acceleratorFacts(modelTesla, memoryK80)

	// Initial observation applies status once.
	f.mustReconcile(t, deviceNameGPU)
	g.Expect(f.writes.statusApplies).To(HaveLen(1))
	first := f.getDevice(t, deviceNameGPU)

	// Unchanged fresh facts: the stamp is kept — a repeated identical
	// status SSA does not resample collectedAt — and the requeue covers
	// exactly the remaining bound.
	f.writes.reset()
	f.clock.advance(4 * time.Minute)
	result := f.mustReconcile(t, deviceNameGPU)
	g.Expect(result.RequeueAfter).To(Equal(6*time.Minute),
		"the no-op requeue covers exactly the remaining bound")
	f.writes.expectNoPrimaryWrites(t)
	unchanged := f.getDevice(t, deviceNameGPU)
	assertStatusMatches(g, unchanged, expectedStatus(first.Status.CollectedAt.Time, modelTesla))
	g.Expect(f.writes.statusApplies).To(HaveLen(1),
		"the framework re-applies identical status; the stamp proves it is a no-op")

	// Changed facts refresh while still fresh.
	f.writes.reset()
	f.clock.advance(time.Minute)
	src.facts = acceleratorFacts(modelOther, memoryK80)
	result = f.mustReconcile(t, deviceNameGPU)
	g.Expect(result.RequeueAfter).To(Equal(maxStaleDefault))
	changed := f.getDevice(t, deviceNameGPU)
	assertStatusMatches(g, changed, expectedStatus(f.clock.Now(), modelOther))

	// Unchanged facts past maxStale: the heartbeat refresh.
	f.writes.reset()
	f.clock.advance(maxStaleDefault + time.Minute)
	result = f.mustReconcile(t, deviceNameGPU)
	g.Expect(result.RequeueAfter).To(Equal(maxStaleDefault))
	heartbeat := f.getDevice(t, deviceNameGPU)
	assertStatusMatches(g, heartbeat, expectedStatus(f.clock.Now(), modelOther))
	f.writes.expectNoPrimaryWrites(t)
}

// TestReconcileMaxStaleFromSingleton proves the heartbeat bound comes
// from the singleton spec, including the exact-boundary requeue.
func TestReconcileMaxStaleFromSingleton(t *testing.T) {
	single, node, device := observedCluster(t)
	single.Spec.MaxStale.Duration = 3 * time.Minute
	f := newFixture(t, single, node, device)
	g := NewWithT(t)
	src := f.draSource()
	src.facts = acceleratorFacts(modelTesla, memoryK80)

	// Initial write, then advance to exactly the bound: not stale (the
	// age is not strictly older than maxStale), so the stamp is kept —
	// but the remaining bound is zero and the requeue must not park the
	// device.
	f.mustReconcile(t, deviceNameGPU)
	first := f.getDevice(t, deviceNameGPU)
	f.writes.reset()
	f.clock.advance(3 * time.Minute)
	result := f.mustReconcile(t, deviceNameGPU)
	g.Expect(result.RequeueAfter).To(Equal(time.Second),
		"the exact-boundary no-op requeues promptly instead of parking")
	unchanged := f.getDevice(t, deviceNameGPU)
	assertStatusMatches(g, unchanged, expectedStatus(first.Status.CollectedAt.Time, modelTesla))

	// Past the bound: the heartbeat refreshes.
	f.clock.advance(time.Minute)
	result = f.mustReconcile(t, deviceNameGPU)
	g.Expect(result.RequeueAfter).To(Equal(3 * time.Minute))
	heartbeat := f.getDevice(t, deviceNameGPU)
	assertStatusMatches(g, heartbeat, expectedStatus(f.clock.Now(), modelTesla))
}

// TestReconcileObserveFailureRetains proves the failure contract:
// facts, collection time and provenance of the last success are
// retained (a failure never looks freshly collected), the Observed
// condition flips to False with the classified reason while
// ProvisioningSucceeded stays True (a scheduled success), Ready
// aggregates to False with phase Not Ready, and an identical retry
// keeps the last transition time.
func TestReconcileObserveFailureRetains(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		reason string
	}{
		{
			name:   "forbidden source read",
			err:    apierrors.NewForbidden(schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceslices"}, "slice-0", errors.New("denied")),
			reason: "SourceForbidden",
		},
		{
			name:   "incomplete dra pool",
			err:    fmt.Errorf("observing device: %w", dra.ErrPoolIncomplete),
			reason: "PoolIncomplete",
		},
		{
			name:   "generic source failure",
			err:    errors.New("connection refused"),
			reason: "SourceUnavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			single, node, device := observedCluster(t)
			f := newFixture(t, single, node, device)
			g := NewWithT(t)
			src := f.draSource()
			src.facts = acceleratorFacts(modelTesla, memoryK80)

			// A prior successful observation is the baseline.
			f.mustReconcile(t, deviceNameGPU)
			want := expectedStatus(f.clock.Now(), modelTesla)
			want.ObservedGeneration = 1
			f.writes.reset()

			// The next observation fails after the clock moved on.
			f.clock.advance(2 * time.Minute)
			src.err = tc.err
			result := f.mustReconcile(t, deviceNameGPU)
			g.Expect(result.RequeueAfter).To(Equal(maxStaleDefault),
				"an expected failure requeues on the full bound")

			found := f.getDevice(t, deviceNameGPU)
			assertStatusMatches(g, found, want)
			assertUnhappyReadiness(g, found)
			g.Expect(found.Status.GetConditions()).To(ContainElement(SatisfyAll(
				condition.Is(conditionTypeObserved, metav1.ConditionFalse),
				condition.HasReason(tc.reason),
				condition.HasMessage(tc.err.Error()),
				HaveField("ObservedGeneration", int64(1)),
			)))
			failed := observedCondition(g, found)
			f.writes.expectNoPrimaryWrites(t)

			// An identical retry keeps the last transition time: no
			// condition churn.
			f.writes.reset()
			f.clock.advance(time.Minute)
			result = f.mustReconcile(t, deviceNameGPU)
			g.Expect(result.RequeueAfter).To(Equal(maxStaleDefault))
			f.writes.expectNoPrimaryWrites(t)
			after := f.getDevice(t, deviceNameGPU)
			assertStatusMatches(g, after, want)
			g.Expect(after.Status.GetConditions()).To(ContainElement(SatisfyAll(
				condition.Is(conditionTypeObserved, metav1.ConditionFalse),
				condition.HasReason(tc.reason),
				HaveField("LastTransitionTime", failed.LastTransitionTime),
			)), "an unchanged failure keeps its last transition time")
		})
	}
}

// TestReconcileSuccessFailureRecovery proves the full cycle: a
// successful observation, a failing one that retains every fact, and a
// recovery whose condition-only success does not refresh the retained
// stamp.
func TestReconcileSuccessFailureRecovery(t *testing.T) {
	single, node, device := observedCluster(t)
	f := newFixture(t, single, node, device)
	g := NewWithT(t)
	src := f.draSource()
	src.facts = acceleratorFacts(modelTesla, memoryK80)

	// Success.
	f.mustReconcile(t, deviceNameGPU)
	happy := f.getDevice(t, deviceNameGPU)
	assertHappyReadiness(g, happy, 1)

	// Failure retains.
	f.clock.advance(2 * time.Minute)
	src.err = errors.New("connection refused")
	f.mustReconcile(t, deviceNameGPU)
	failed := f.getDevice(t, deviceNameGPU)
	assertStatusMatches(g, failed, expectedStatus(happy.Status.CollectedAt.Time, modelTesla))
	assertUnhappyReadiness(g, failed)
	g.Expect(observedCondition(g, failed).Reason).To(Equal("SourceUnavailable"))

	// Recovery: the condition flips back, and the unchanged-fresh
	// observation keeps the retained collection time.
	f.clock.advance(time.Minute)
	src.err = nil
	result := f.mustReconcile(t, deviceNameGPU)
	g.Expect(result.RequeueAfter).To(Equal(7*time.Minute),
		"the recovery requeue covers the remaining bound of the retained stamp")
	recovered := f.getDevice(t, deviceNameGPU)
	assertStatusMatches(g, recovered, expectedStatus(happy.Status.CollectedAt.Time, modelTesla))
	assertHappyReadiness(g, recovered, 1)
	f.writes.expectNoPrimaryWrites(t)
}

// TestReconcileIneligibility proves every ineligibility reason: the
// prior facts are retained, the Observed condition carries the specific
// reason, Ready aggregates to False with phase Not Ready (never a
// misleading old Ready=True), and the requeue uses the effective
// heartbeat bound.
func TestReconcileIneligibility(t *testing.T) {
	prior := seededStatus(fixtureStart)

	cases := []struct {
		name   string
		reason string
		setup  func(t *testing.T) *fixture
	}{
		{
			name:   "class coverage moved on",
			reason: "ClassMismatch",
			setup: func(t *testing.T) *fixture {
				single := testSingleton(func(s *infrastructurev1alpha1.HardwareDiscovery) {
					s.Spec.Class = "other"
				})
				node := testNode(nodeAName, nodeAUID, map[string]string{selectorKeyPool: selectorPoolDefault})
				return newFixture(t, single, node, ownedDevice(t, single, prior))
			},
		},
		{
			name:   "source disabled",
			reason: "SourceDisabled",
			setup: func(t *testing.T) *fixture {
				single := testSingleton(withDRADisabled())
				node := testNode(nodeAName, nodeAUID, map[string]string{selectorKeyPool: selectorPoolDefault})
				return newFixture(t, single, node, ownedDevice(t, single, prior))
			},
		},
		{
			name:   "node absent",
			reason: "NodeNotFound",
			setup: func(t *testing.T) *fixture {
				single := testSingleton()
				return newFixture(t, single, ownedDevice(t, single, prior))
			},
		},
		{
			name:   "node replaced under the same name",
			reason: "NodeUIDMismatch",
			setup: func(t *testing.T) *fixture {
				single := testSingleton()
				replaced := testNode(nodeAName, "uid-replaced", map[string]string{selectorKeyPool: selectorPoolDefault})
				return newFixture(t, single, replaced, ownedDevice(t, single, prior))
			},
		},
		{
			name:   "node deselected",
			reason: "NodeDeselected",
			setup: func(t *testing.T) *fixture {
				single := testSingleton(withSelector(map[string]string{selectorKeyPool: selectorPoolDefault}))
				node := testNode(nodeAName, nodeAUID, nil)
				return newFixture(t, single, node, ownedDevice(t, single, prior))
			},
		},
		{
			name:   "singleton absent",
			reason: "ConfigurationMissing",
			setup: func(t *testing.T) *fixture {
				node := testNode(nodeAName, nodeAUID, map[string]string{selectorKeyPool: selectorPoolDefault})
				single := testSingleton()
				return newFixture(t, node, ownedDevice(t, single, prior))
			},
		},
		{
			name:   "foreign singleton name does not configure observation",
			reason: "ConfigurationMissing",
			setup: func(t *testing.T) *fixture {
				single := testSingleton()
				foreign := single.DeepCopy()
				foreign.Name = "not-the-singleton"
				foreign.UID = "foreign-uid"
				node := testNode(nodeAName, nodeAUID, map[string]string{selectorKeyPool: selectorPoolDefault})
				return newFixture(t, foreign, node, ownedDevice(t, single, prior))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.setup(t)
			g := NewWithT(t)
			src := f.draSource()
			src.facts = acceleratorFacts(modelTesla, memoryK80)

			result := f.mustReconcile(t, deviceNameGPU)
			g.Expect(result.RequeueAfter).To(Equal(maxStaleDefault),
				"ineligibility requeues on the effective heartbeat bound")

			found := f.getDevice(t, deviceNameGPU)
			assertStatusMatches(g, found, prior)
			assertUnhappyReadiness(g, found)
			g.Expect(found.Status.GetConditions()).To(ContainElement(SatisfyAll(
				condition.Is(conditionTypeObserved, metav1.ConditionFalse),
				condition.HasReason(tc.reason),
				HaveField("ObservedGeneration", int64(1)),
			)))
			g.Expect(src.observed).To(BeEmpty(),
				"an ineligible device never reaches its source")
			f.writes.expectNoPrimaryWrites(t)
		})
	}
}

// TestReconcileInvalidConfigTiming proves the effective heartbeat bound
// of an invalid configuration: a positive spec.maxStale survives another
// invalid field, while an omitted or non-positive one falls back to the
// design default.
func TestReconcileInvalidConfigTiming(t *testing.T) {
	cases := []struct {
		name     string
		maxStale time.Duration
		requeue  time.Duration
	}{
		{
			name:     "positive spec timing survives another invalid field",
			maxStale: 3 * time.Minute,
			requeue:  3 * time.Minute,
		},
		{
			name:     "omitted timing falls back to the default",
			maxStale: 0,
			requeue:  maxStaleDefault,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			single, node, device := observedCluster(t)
			single.Spec.MaxStale.Duration = tc.maxStale
			single.Spec.NodeSelector = &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "pool",
					Operator: "NotAnOperator",
					Values:   []string{"default"},
				}},
			}
			prior := seededStatus(fixtureStart)
			device.Status = prior
			f := newFixture(t, single, node, device)
			g := NewWithT(t)
			f.draSource()

			result := f.mustReconcile(t, deviceNameGPU)
			g.Expect(result.RequeueAfter).To(Equal(tc.requeue))

			found := f.getDevice(t, deviceNameGPU)
			assertStatusMatches(g, found, prior)
			assertUnhappyReadiness(g, found)
			g.Expect(observedCondition(g, found).Reason).To(Equal("InvalidConfiguration"))
			f.writes.expectNoPrimaryWrites(t)
		})
	}
}

// TestReconcileDeletionRace proves deletion handling: a request for an
// already-deleted device is a plain success (never recreation), and a
// device under deletion is an early framework no-op — no observation,
// no status write.
func TestReconcileDeletionRace(t *testing.T) {
	single, node, _ := observedCluster(t)
	g := NewWithT(t)

	// The device vanished before the queued request ran.
	f := newFixture(t, single, node)
	f.draSource()
	result := f.mustReconcile(t, "gone-device")
	g.Expect(result.RequeueAfter).To(BeZero())
	f.writes.expectNoPrimaryWrites(t)
	g.Expect(f.writes.statusApplies).To(BeEmpty())
	f.deviceMissing(t, "gone-device")

	// The device is under deletion: no observation, no write.
	deleting := ownedDevice(t, single, seededStatus(fixtureStart))
	deleting.Finalizers = append(deleting.Finalizers, "test.opendatahub.io/retention")
	deleting.DeletionTimestamp = &metav1.Time{Time: fixtureStart.Add(time.Minute)}
	f2 := newFixture(t, single, node, deleting)
	src := f2.draSource()
	result = f2.mustReconcile(t, deviceNameGPU)
	g.Expect(result.RequeueAfter).To(BeZero())
	f2.writes.expectNoPrimaryWrites(t)
	g.Expect(f2.writes.statusApplies).To(BeEmpty())
	g.Expect(src.observed).To(BeEmpty())
	found := f2.getDevice(t, deviceNameGPU)
	assertStatusMatches(g, found, seededStatus(fixtureStart))
}

// TestReconcileMalformedSpec proves a spec that could only exist without
// CEL validation (source kind without its arm) surfaces as a contextual
// reconcile error with a diagnostic Observed=False — never a silent
// successful observation.
func TestReconcileMalformedSpec(t *testing.T) {
	single, node, device := observedCluster(t)
	device.Spec.Source.DRA = nil
	f := newFixture(t, single, node, device)
	g := NewWithT(t)
	f.draSource()

	_, err := f.reconciler(t).Reconcile(context.Background(), request(deviceNameGPU))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("invalid source locator"))

	found := f.getDevice(t, deviceNameGPU)
	assertUnhappyReadiness(g, found)
	g.Expect(observedCondition(g, found).Reason).To(Equal("InvalidConfiguration"))
	f.writes.expectNoPrimaryWrites(t)
}

// TestReconcileTransientErrors proves pre-observation read failures
// return contextual errors for the framework's normal backoff while
// marking the evaluation failed: Observed=False, ProvisioningSucceeded
// False (a real error, not a scheduled success), Ready=False — and the
// retained facts never look freshly collected.
func TestReconcileTransientErrors(t *testing.T) {
	cases := []struct {
		name    string
		failing string
		wrapped string
	}{
		{
			name:    "singleton read fails",
			failing: "cluster",
			wrapped: "fetching singleton",
		},
		{
			name:    "node read fails",
			failing: nodeAName,
			wrapped: "fetching node",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			single, node, device := observedCluster(t)
			prior := seededStatus(fixtureStart)
			device.Status = prior
			f := newFixture(t, single, node, device)
			g := NewWithT(t)
			f.draSource()

			f.hooks.get = func(
				ctx context.Context,
				c client.WithWatch,
				key client.ObjectKey,
				obj client.Object,
				opts ...client.GetOption,
			) error {
				if key.Name == tc.failing {
					return apierrors.NewServerTimeout(schema.GroupResource{
						Group:    "infrastructure.platform.opendatahub.io",
						Resource: "hardwaredevices",
					}, "get", 1)
				}
				return c.Get(ctx, key, obj, opts...)
			}

			_, err := f.reconciler(t).Reconcile(context.Background(), request(deviceNameGPU))
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wrapped))

			found := f.getDevice(t, deviceNameGPU)
			assertStatusMatches(g, found, prior)
			assertUnhappyReadiness(g, found)
			observed := observedCondition(g, found)
			g.Expect(observed.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(observed.Reason).To(Equal("SourceUnavailable"))
			g.Expect(observed.Message).To(ContainSubstring(tc.wrapped))
			prov := findCondition(g, found, conditionTypeProv)
			g.Expect(prov.Status).To(Equal(metav1.ConditionFalse),
				"a transient failure is a real error, not a scheduled success")
			f.writes.expectNoPrimaryWrites(t)
		})
	}
}

// TestReconcileGenerationSemantics proves generation changes never
// refresh facts: the conditions and the top-level observed generation
// follow the new generation while the retained collection time stays.
func TestReconcileGenerationSemantics(t *testing.T) {
	single, node, device := observedCluster(t)
	f := newFixture(t, single, node, device)
	g := NewWithT(t)
	src := f.draSource()
	src.facts = acceleratorFacts(modelTesla, memoryK80)

	f.mustReconcile(t, deviceNameGPU)
	first := f.getDevice(t, deviceNameGPU)

	// The inventory controller rewrote the spec: generation bump, same
	// identity.
	bumped := first.DeepCopy()
	bumped.Generation = 2
	f.updateDevice(t, bumped)

	f.clock.advance(time.Minute)
	result := f.mustReconcile(t, deviceNameGPU)
	g.Expect(result.RequeueAfter).To(Equal(9*time.Minute),
		"a generation change alone does not refresh the heartbeat bound")

	found := f.getDevice(t, deviceNameGPU)
	want := expectedStatus(first.Status.CollectedAt.Time, modelTesla)
	want.ObservedGeneration = 2
	assertStatusMatches(g, found, want)
	assertHappyReadiness(g, found, 2)
	g.Expect(observedCondition(g, found).ObservedGeneration).To(Equal(int64(2)))
	f.writes.expectNoPrimaryWrites(t)
}

// TestReconcileIncarnationRace is the unit-level interceptor scenario of
// the accepted status-write incarnation semantics: a completed
// observation of deleted incarnation A lands on same-name replacement B
// (the framework's status apply carries no optimistic-concurrency
// precondition and the payload's uid is not a guard), and B's creation
// reconcile overwrites the transferred status with its own observation
// within one pass — eventual consistency, not atomicity. The deployed
// Kind e2e repeats this race against a real API server.
func TestReconcileIncarnationRace(t *testing.T) {
	single, node, device := observedCluster(t)
	f := newFixture(t, single, node, device)
	g := NewWithT(t)
	src := f.draSource()
	src.facts = acceleratorFacts(modelTesla, memoryK80)
	scheme := testScheme(t)

	// Incarnation A observes successfully.
	f.mustReconcile(t, deviceNameGPU)
	incarnationA := f.getDevice(t, deviceNameGPU)
	assertHappyReadiness(g, incarnationA, 1)

	// A's next observation completes while the inventory controller
	// deletes A and recreates the same name as incarnation B: the swap
	// happens between the reconciler's read and its status apply.
	f.clock.advance(5 * time.Minute)
	src.facts = acceleratorFacts(modelOther, memoryK80)
	swapped := false
	f.hooks.subResourceApply = func(
		ctx context.Context,
		c client.Client,
		subResourceName string,
		obj runtime.ApplyConfiguration,
		opts ...client.SubResourceApplyOption,
	) error {
		if !swapped {
			swapped = true
			g.Expect(c.Delete(ctx, &infrastructurev1alpha1.HardwareDevice{
				ObjectMeta: metav1.ObjectMeta{Name: deviceNameGPU},
			})).To(Succeed())
			reincarnation := seedDevice(t, scheme, single, deviceNameGPU,
				draDeviceSpec(nodeAName, nodeAUID, poolWorker))
			reincarnation.UID = types.UID("device-reincarnated")
			g.Expect(c.Create(ctx, reincarnation)).To(Succeed())
		}
		return c.SubResource(subResourceName).Apply(ctx, obj, opts...)
	}
	f.mustReconcile(t, deviceNameGPU)

	// The stale A facts landed on B — the accepted transfer — and the
	// outgoing payload still identified A while the live object was B.
	transferred := f.getDevice(t, deviceNameGPU)
	g.Expect(transferred.UID).To(Equal(types.UID("device-reincarnated")))
	assertStatusMatches(g, transferred, expectedStatus(f.clock.Now(), modelOther))
	g.Expect(f.writes.appliedUIDs).To(HaveLen(2))
	g.Expect(f.writes.appliedUIDs[1]).To(Equal(incarnationA.UID),
		"the status apply carries the deleted incarnation's uid; it is not a guard")

	// B's creation reconcile overwrites the transferred status with its
	// own observation: convergence, one pass.
	f.hooks.subResourceApply = nil
	src.facts = acceleratorFacts(modelTesla, memoryK80)
	f.mustReconcile(t, deviceNameGPU)
	converged := f.getDevice(t, deviceNameGPU)
	g.Expect(converged.UID).To(Equal(types.UID("device-reincarnated")))
	assertStatusMatches(g, converged, expectedStatus(f.clock.Now(), modelTesla))
	assertHappyReadiness(g, converged, 1)
	f.writes.expectNoPrimaryWrites(t)
}
