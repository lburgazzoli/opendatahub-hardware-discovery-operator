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

	"github.com/lburgazzoli/gomega-matchers/pkg/matchers/jq"
	"github.com/lburgazzoli/gomega-matchers/pkg/matchers/k8s/condition"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/classic"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/dra"
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

// TestReconcileObservesDevice proves the happy path: the controller
// resolves exactly the spec identity and locator, observes through the
// source, persists facts/plane/provenance with the collector-clock
// stamp and the Observed condition through /status — and never touches
// the primary endpoint.
func TestReconcileObservesDevice(t *testing.T) {
	single, node, device := observedCluster(t)
	f := newFixture(t, single, node, device)
	src := f.draSource()
	src.facts = acceleratorFacts(modelTesla, memoryK80)

	result := f.mustReconcile(deviceNameGPU)
	f.g.Expect(result.RequeueAfter).To(Equal(maxStaleDefault))

	// The observation resolved exactly the spec identity and locator.
	f.g.Expect(src.observed).To(HaveLen(1))
	identity := src.observed[0]
	f.g.Expect(identity.ID).To(Equal(deviceIDGPU))
	f.g.Expect(string(identity.Kind)).To(Equal(identityKindUUID))
	f.g.Expect(identity.NodeName).To(Equal(nodeAName))
	f.g.Expect(identity.NodeUID).To(Equal(types.UID(nodeAUID)))
	f.g.Expect(identity.Plane).To(Equal(discovery.PlaneDRA))
	f.g.Expect(identity.Locator).To(Equal(dra.Locator{
		Driver: driverNVIDIA, Pool: poolWorker, Device: draDeviceName,
	}))

	// Status: facts, plane, provenance, collector-clock stamp.
	found := f.getDevice(deviceNameGPU)
	assertStatusMatches(f.g, found, expectedStatus(f.clock.Now(), modelTesla))
	f.g.Expect(found.Status.Conditions).To(ContainElement(SatisfyAll(
		condition.Is(conditionTypeObserved, metav1.ConditionTrue),
		condition.HasReason(reasonObserved),
		condition.HasMessage(`observed facts from source "dra"`),
		HaveField("ObservedGeneration", int64(1)),
	)))

	// Spec untouched; exactly one status write; nothing else.
	f.g.Expect(found.Spec).To(Equal(draDeviceSpec(nodeAName, nodeAUID, poolWorker)))
	f.g.Expect(f.client.statusUpdates).To(HaveLen(1))
	f.client.expectNoPrimaryWrites(f.g)
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
	src := &fakeSource{
		id:    sourceIDClassic,
		plane: discovery.PlaneClassic,
		clock: f.clock,
		facts: acceleratorFacts(modelOther, memoryV100),
	}
	f.sources = append(f.sources, src)

	result := f.mustReconcile(name)
	f.g.Expect(result.RequeueAfter).To(Equal(maxStaleDefault))

	f.g.Expect(src.observed).To(HaveLen(1))
	identity := src.observed[0]
	f.g.Expect(string(identity.Kind)).To(Equal(identityKindNodeIndex))
	f.g.Expect(identity.Plane).To(Equal(discovery.PlaneClassic))
	f.g.Expect(identity.Locator).To(Equal(classic.Locator{Node: nodeBName, Index: 0}))

	found := f.getDevice(name)
	f.g.Expect(found).To(SatisfyAll(
		jq.Match(`.status.observedGeneration == 1`),
		jq.Match(fmt.Sprintf(`.status.allocationPlane == %q`, sourceIDClassic)),
		jq.Match(fmt.Sprintf(`.status.sourceRef == %q`, sourceIDClassic+"/"+nodeBName)),
		jq.Match(fmt.Sprintf(`.status.accelerator.model == %q and .status.accelerator.memory == %q`,
			modelOther, memoryV100)),
	))
	f.g.Expect(found.Status.CollectedAt.Time.Equal(f.clock.Now())).To(BeTrue())
	f.client.expectNoPrimaryWrites(f.g)
}

// TestReconcileFactsChangeVsHeartbeat proves the write policy: unchanged
// fresh facts write nothing (timestamp sampling alone is not a facts
// change) and requeue for the remaining bound; changed facts write while
// fresh; unchanged facts past maxStale write as the heartbeat.
func TestReconcileFactsChangeVsHeartbeat(t *testing.T) {
	single, node, device := observedCluster(t)
	f := newFixture(t, single, node, device)
	src := f.draSource()
	src.facts = acceleratorFacts(modelTesla, memoryK80)

	// Initial observation writes once.
	f.mustReconcile(deviceNameGPU)
	f.g.Expect(f.client.statusUpdates).To(HaveLen(1))
	first := f.getDevice(deviceNameGPU)

	// Unchanged fresh facts: no write at all, requeue for the remaining
	// heartbeat bound.
	f.client.reset()
	f.clock.advance(4 * time.Minute)
	result := f.mustReconcile(deviceNameGPU)
	f.g.Expect(result.RequeueAfter).To(Equal(6*time.Minute),
		"the no-op requeue covers exactly the remaining bound")
	f.client.expectNoPrimaryWrites(f.g)
	f.g.Expect(f.client.statusUpdates).To(BeEmpty(),
		"timestamp sampling alone is not a facts change")
	unchanged := f.getDevice(deviceNameGPU)
	assertStatusMatches(f.g, unchanged, expectedStatus(first.Status.CollectedAt.Time, modelTesla))

	// Changed facts write while still fresh.
	f.client.reset()
	f.clock.advance(time.Minute)
	src.facts = acceleratorFacts(modelOther, memoryK80)
	result = f.mustReconcile(deviceNameGPU)
	f.g.Expect(result.RequeueAfter).To(Equal(maxStaleDefault))
	f.g.Expect(f.client.statusUpdates).To(HaveLen(1))
	changed := f.getDevice(deviceNameGPU)
	assertStatusMatches(f.g, changed, expectedStatus(f.clock.Now(), modelOther))

	// Unchanged facts past maxStale: the heartbeat write.
	f.client.reset()
	f.clock.advance(maxStaleDefault + time.Minute)
	result = f.mustReconcile(deviceNameGPU)
	f.g.Expect(result.RequeueAfter).To(Equal(maxStaleDefault))
	f.g.Expect(f.client.statusUpdates).To(HaveLen(1))
	heartbeat := f.getDevice(deviceNameGPU)
	assertStatusMatches(f.g, heartbeat, expectedStatus(f.clock.Now(), modelOther))
}

// TestReconcileMaxStaleFromSingleton proves the heartbeat bound comes
// from the singleton spec, including the exact-boundary requeue.
func TestReconcileMaxStaleFromSingleton(t *testing.T) {
	single, node, device := observedCluster(t)
	single.Spec.MaxStale.Duration = 3 * time.Minute
	f := newFixture(t, single, node, device)
	src := f.draSource()
	src.facts = acceleratorFacts(modelTesla, memoryK80)

	// Initial write, then advance to exactly the bound: not yet stale
	// (the age is not older than maxStale), so no write — but the
	// remaining bound is zero and the requeue must not park the device.
	f.mustReconcile(deviceNameGPU)
	f.client.reset()
	f.clock.advance(3 * time.Minute)
	result := f.mustReconcile(deviceNameGPU)
	f.g.Expect(result.RequeueAfter).To(Equal(time.Second),
		"the exact-boundary no-op requeues promptly instead of parking")
	f.g.Expect(f.client.statusUpdates).To(BeEmpty())

	// Past the bound: the heartbeat writes.
	f.clock.advance(time.Minute)
	result = f.mustReconcile(deviceNameGPU)
	f.g.Expect(result.RequeueAfter).To(Equal(3 * time.Minute))
	f.g.Expect(f.client.statusUpdates).To(HaveLen(1))
	heartbeat := f.getDevice(deviceNameGPU)
	assertStatusMatches(f.g, heartbeat, expectedStatus(f.clock.Now(), modelTesla))
}

// TestReconcileObserveFailureRetains proves the failure contract: facts,
// collection time, provenance and observed generation of the last
// success are retained (a failure never looks freshly collected), the
// Observed condition flips to False with the classified reason, and an
// identical retry writes nothing.
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
			src := f.draSource()
			src.facts = acceleratorFacts(modelTesla, memoryK80)

			// A prior successful observation is the baseline.
			f.mustReconcile(deviceNameGPU)
			want := expectedStatus(f.clock.Now(), modelTesla)
			f.client.reset()

			// The next observation fails after the clock moved on.
			f.clock.advance(2 * time.Minute)
			src.err = tc.err
			result := f.mustReconcile(deviceNameGPU)
			f.g.Expect(result.RequeueAfter).To(Equal(maxStaleDefault))

			found := f.getDevice(deviceNameGPU)
			assertStatusMatches(f.g, found, want)
			f.g.Expect(found.Status.Conditions).To(ContainElement(SatisfyAll(
				condition.Is(conditionTypeObserved, metav1.ConditionFalse),
				condition.HasReason(tc.reason),
				condition.HasMessage(tc.err.Error()),
				HaveField("ObservedGeneration", int64(1)),
			)))
			failed := observedCondition(f.g, found)
			f.g.Expect(f.client.statusUpdates).To(HaveLen(1))
			f.client.expectNoPrimaryWrites(f.g)

			// An identical retry writes nothing: no condition churn.
			f.client.reset()
			f.clock.advance(time.Minute)
			result = f.mustReconcile(deviceNameGPU)
			f.g.Expect(result.RequeueAfter).To(Equal(maxStaleDefault))
			f.client.expectNoPrimaryWrites(f.g)
			f.g.Expect(f.client.statusUpdates).To(BeEmpty())
			after := f.getDevice(deviceNameGPU)
			assertStatusMatches(f.g, after, want)
			f.g.Expect(after.Status.Conditions).To(ContainElement(SatisfyAll(
				condition.Is(conditionTypeObserved, metav1.ConditionFalse),
				condition.HasReason(tc.reason),
				HaveField("LastTransitionTime", failed.LastTransitionTime),
			)), "an unchanged failure keeps its last transition time")
		})
	}
}

// TestReconcileStandDowns proves that a stale queued request cannot
// freshen a deselected or different-incarnation device: every stand-down
// is a plain success with zero writes and the prior status untouched.
func TestReconcileStandDowns(t *testing.T) {
	prior := seededStatus(fixtureStart)

	cases := []struct {
		name  string
		setup func(t *testing.T) *fixture
	}{
		{
			name: "node absent",
			setup: func(t *testing.T) *fixture {
				single := testSingleton()
				return newFixture(t, single, ownedDevice(t, single, prior))
			},
		},
		{
			name: "node replaced under the same name",
			setup: func(t *testing.T) *fixture {
				single := testSingleton()
				replaced := testNode(nodeAName, "uid-replaced", map[string]string{selectorKeyPool: selectorPoolDefault})
				return newFixture(t, single, replaced, ownedDevice(t, single, prior))
			},
		},
		{
			name: "node deselected",
			setup: func(t *testing.T) *fixture {
				single := testSingleton(withSelector(map[string]string{selectorKeyPool: selectorPoolDefault}))
				node := testNode(nodeAName, nodeAUID, nil)
				return newFixture(t, single, node, ownedDevice(t, single, prior))
			},
		},
		{
			name: "source disabled",
			setup: func(t *testing.T) *fixture {
				single := testSingleton(withDRADisabled())
				node := testNode(nodeAName, nodeAUID, map[string]string{selectorKeyPool: selectorPoolDefault})
				return newFixture(t, single, node, ownedDevice(t, single, prior))
			},
		},
		{
			name: "class coverage moved on",
			setup: func(t *testing.T) *fixture {
				single := testSingleton(func(s *infrastructurev1alpha1.HardwareDiscovery) {
					s.Spec.Class = "other"
				})
				node := testNode(nodeAName, nodeAUID, map[string]string{selectorKeyPool: selectorPoolDefault})
				return newFixture(t, single, node, ownedDevice(t, single, prior))
			},
		},
		{
			name: "singleton absent",
			setup: func(t *testing.T) *fixture {
				node := testNode(nodeAName, nodeAUID, map[string]string{selectorKeyPool: selectorPoolDefault})
				single := testSingleton()
				return newFixture(t, node, ownedDevice(t, single, prior))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.setup(t)
			f.draSource()

			result := f.mustReconcile(deviceNameGPU)
			f.g.Expect(result).To(Equal(ctrl.Result{}))
			f.client.expectNoPrimaryWrites(f.g)
			f.g.Expect(f.client.statusUpdates).To(BeEmpty(),
				"a stand-down never writes status")

			found := f.getDevice(deviceNameGPU)
			assertStatusMatches(f.g, found, prior)
			f.g.Expect(found.Status.Conditions).To(BeEmpty())
		})
	}
}

// TestReconcileDeletionRace proves deletion handling: a request for an
// already-deleted device is a plain success (never recreation), and a
// device under deletion is not observed.
func TestReconcileDeletionRace(t *testing.T) {
	single, node, _ := observedCluster(t)

	// The device vanished before the queued request ran.
	f := newFixture(t, single, node)
	f.draSource()
	result := f.mustReconcile("gone-device")
	f.g.Expect(result).To(Equal(ctrl.Result{}))
	f.client.expectNoPrimaryWrites(f.g)
	f.g.Expect(f.client.statusUpdates).To(BeEmpty())
	f.deviceMissing("gone-device")

	// The device is under deletion: no observation, no write.
	deleting := ownedDevice(t, single, seededStatus(fixtureStart))
	deleting.Finalizers = append(deleting.Finalizers, "test.opendatahub.io/retention")
	deleting.DeletionTimestamp = &metav1.Time{Time: fixtureStart.Add(time.Minute)}
	f2 := newFixture(t, single, node, deleting)
	f2.draSource()
	result = f2.mustReconcile(deviceNameGPU)
	f.g.Expect(result).To(Equal(ctrl.Result{}))
	f2.client.expectNoPrimaryWrites(f2.g)
	f2.g.Expect(f2.client.statusUpdates).To(BeEmpty())
	found := f2.getDevice(deviceNameGPU)
	assertStatusMatches(f2.g, found, seededStatus(fixtureStart))
}

// TestReconcileInvalidConfig proves an invalid singleton configuration
// stands down without touching device status: the inventory controller
// reports the condition, and the requeue is the safety net.
func TestReconcileInvalidConfig(t *testing.T) {
	single, node, device := observedCluster(t)
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
	f.draSource()

	result := f.mustReconcile(deviceNameGPU)
	f.g.Expect(result.RequeueAfter).To(Equal(maxStaleDefault))
	f.client.expectNoPrimaryWrites(f.g)
	f.g.Expect(f.client.statusUpdates).To(BeEmpty())
	found := f.getDevice(deviceNameGPU)
	assertStatusMatches(f.g, found, prior)
}

// TestReconcileMalformedSpec proves a spec that could only exist without
// CEL validation (source kind without its arm) surfaces as a reconcile
// error instead of a silent skip, and writes nothing.
func TestReconcileMalformedSpec(t *testing.T) {
	single, node, device := observedCluster(t)
	device.Spec.Source.DRA = nil
	f := newFixture(t, single, node, device)
	f.draSource()

	_, err := f.reconciler().Reconcile(context.Background(), request(deviceNameGPU))
	f.g.Expect(err).To(HaveOccurred())
	f.g.Expect(err.Error()).To(ContainSubstring("invalid source locator"))
	f.client.expectNoPrimaryWrites(f.g)
	f.g.Expect(f.client.statusUpdates).To(BeEmpty())
}

// TestReconcileIgnoresForeignSingletonName proves only the well-known
// singleton instance configures observation: another HardwareDiscovery
// object does not enable it.
func TestReconcileIgnoresForeignSingletonName(t *testing.T) {
	single, node, device := observedCluster(t)
	foreign := single.DeepCopy()
	foreign.Name = "not-the-singleton"
	foreign.UID = "foreign-uid"
	f := newFixture(t, foreign, node, device)
	f.draSource()

	result := f.mustReconcile(deviceNameGPU)
	f.g.Expect(result).To(Equal(ctrl.Result{}))
	f.client.expectNoPrimaryWrites(f.g)
	f.g.Expect(f.client.statusUpdates).To(BeEmpty())
}

// TestReconcileTransientErrors proves read failures surface as reconcile
// errors for normal backoff, with no partial writes.
func TestReconcileTransientErrors(t *testing.T) {
	single, node, device := observedCluster(t)
	f := newFixture(t, single, node, device)
	f.draSource()

	// A failing device read is a transient error.
	failing := &failingGetClient{Client: f.client.Client, name: deviceNameGPU}
	f.client.Client = failing
	_, err := f.reconciler().Reconcile(context.Background(), request(deviceNameGPU))
	f.g.Expect(err).To(HaveOccurred())
	f.client.expectNoPrimaryWrites(f.g)
	f.g.Expect(f.client.statusUpdates).To(BeEmpty())
}

// failingGetClient fails Get for one object name.
type failingGetClient struct {
	client.Client
	name string
}

// Get forwards every read except the failing name.
func (c *failingGetClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	if key.Name == c.name {
		return apierrors.NewServerTimeout(schema.GroupResource{
			Group:    "infrastructure.platform.opendatahub.io",
			Resource: "hardwaredevices",
		}, "get", 1)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}
