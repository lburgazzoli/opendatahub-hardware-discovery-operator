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

package inventory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/inventory"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/dra"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
)

// TestReconcileCreatesOwnedDevices covers the default all-node selector,
// device creation with ownership/labels/specs on both planes, the benign
// unknown of an unclaimed node, the singleton status, and — through the
// instrumented client — that a second, unchanged scan performs no write
// on children of any kind and leaves the persisted status unchanged.
func TestReconcileCreatesOwnedDevices(t *testing.T) {
	nodeA := testNode(nodeAName, nodeAUID, map[string]string{selectorKeyPool: "default"})
	nodeB := testNode(nodeBName, nodeBUID, nil)
	plain := testNode(plainName, plainUID, nil)
	single := testSingleton()

	draGPU0 := draIdentity(nodeAName, nodeAUID, deviceIDGPU, "workerpool")
	draGPU1 := draIdentity(nodeAName, nodeAUID, "gpu-def456", "workerpool")
	classic0 := classicIdentity(nodeBName, nodeBUID, 0)
	classic1 := classicIdentity(nodeBName, nodeBUID, 1)

	draSrc := &fakeSource{
		id:     sourceIDDRA,
		plane:  discovery.PlaneDRA,
		claims: map[string]bool{nodeAName: true},
		enums: map[string]discovery.Enumeration{
			nodeAName: successEnum(draGPU0, draGPU1),
		},
	}
	classicSrc := &fakeSource{
		id:     sourceIDClassic,
		plane:  discovery.PlaneClassic,
		claims: map[string]bool{nodeBName: true},
		enums: map[string]discovery.Enumeration{
			nodeBName: successEnum(classic0, classic1),
		},
	}

	f := newFixture(t, single, nodeA, nodeB, plain)
	g := NewWithT(t)
	f.sources = []discovery.Source{draSrc, classicSrc}

	result := f.mustReconcile(t)
	g.Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

	g.Expect(f.listDevices(t)).To(HaveLen(4))

	// DRA device: spec, ownership, labels, empty status.
	draDevice := f.getDevice(t, deviceIDGPU)
	g.Expect(draDevice.Spec).To(Equal(draDeviceSpec(deviceIDGPU, nodeAName, nodeAUID, "workerpool")))
	g.Expect(draDevice.Status).To(Equal(infrastructurev1alpha1.HardwareDeviceStatus{}),
		"the inventory controller never writes device status")
	ref := metav1.GetControllerOf(draDevice)
	g.Expect(ref).NotTo(BeNil())
	g.Expect(ref.Kind).To(Equal("HardwareDiscovery"))
	g.Expect(ref.Name).To(Equal(inventory.SingletonName))
	g.Expect(ref.UID).To(Equal(types.UID(singletonUID)))
	g.Expect(draDevice.OwnerReferences).To(HaveLen(1), "no Node owner reference is ever added")
	g.Expect(draDevice.Labels).To(Equal(map[string]string{
		deviceClassLabel: classTest,
		nodeLabel:        nodeAName,
	}))

	// Classic device: nodeIndex identity with a classic locator.
	classicDevice := f.getDevice(t, nodeBName+"-gpu-0")
	g.Expect(classicDevice.Spec).To(Equal(classicDeviceSpec(nodeBName, nodeBUID, 0)))

	// Singleton status: summaries, phase and conditions with the
	// generation. The framework contributes ProvisioningSucceeded; the
	// derived Ready carries the scan's success reason and message.
	updated := f.getSingleton(t)
	g.Expect(updated.Status.ObservedGeneration).To(Equal(int64(1)))
	g.Expect(updated.Status.Phase).To(Equal("Ready"))
	g.Expect(updated.Status.NodeSummaries).To(Equal([]infrastructurev1alpha1.NodeSummary{
		{Name: nodeAName, UID: types.UID(nodeAUID), Plane: sourceIDDRA, DeviceCount: 2, Outcome: outcomeSuccess},
		{Name: nodeBName, UID: types.UID(nodeBUID), Plane: sourceIDClassic, DeviceCount: 2, Outcome: outcomeSuccess},
		{Name: plainName, UID: types.UID(plainUID), DeviceCount: 0, Outcome: outcomeUnknown},
	}))
	ready := condition(g, updated, inventory.ConditionTypeReady)
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(ready.Reason).To(Equal("Reconciled"))
	g.Expect(ready.ObservedGeneration).To(Equal(int64(1)))
	degraded := condition(g, updated, inventory.ConditionTypeDegraded)
	g.Expect(degraded.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(degraded.Reason).To(Equal("Reconciled"))
	provisioned := condition(g, updated, "ProvisioningSucceeded")
	g.Expect(provisioned.Status).To(Equal(metav1.ConditionTrue))

	// A second, unchanged scan leaves the produced objects identical:
	// children are untouched — including their /status, which belongs
	// to the device controller — and the persisted singleton status
	// carries the same conditions (the framework's status apply may
	// run, but it carries identical content).
	devicesBefore := f.listDevices(t)
	singleBefore := f.getSingleton(t)
	f.mustReconcile(t)
	g.Expect(f.listDevices(t)).To(Equal(devicesBefore),
		"an unchanged scan never touches children, including their status")

	unchanged := f.getSingleton(t)
	g.Expect(unchanged.Status.ObservedGeneration).To(Equal(int64(1)))
	g.Expect(unchanged.Status.Phase).To(Equal("Ready"))
	g.Expect(unchanged.Status.NodeSummaries).To(Equal(singleBefore.Status.NodeSummaries))
	for _, conditionType := range []string{
		inventory.ConditionTypeReady, inventory.ConditionTypeDegraded, "ProvisioningSucceeded",
	} {
		again := condition(g, unchanged, conditionType)
		before := condition(g, singleBefore, conditionType)
		g.Expect(again).To(Equal(before), "condition %q is unchanged by a no-op scan", conditionType)
	}
}

// TestReconcileUpdatesSpecAndRetainsStatus proves a locator change is a
// primary-endpoint spec write only: the child's existing status — written
// by the device controller — survives every spec update, and no device
// /status call is ever made.
func TestReconcileUpdatesSpecAndRetainsStatus(t *testing.T) {
	nodeA := testNode(nodeAName, nodeAUID, nil)
	single := testSingleton()

	seededStatus := infrastructurev1alpha1.HardwareDeviceStatus{
		Accelerator: &infrastructurev1alpha1.AcceleratorDeviceStatus{
			Model:  modelTesla,
			Memory: nil,
		},
		Status:          fwapi.Status{ObservedGeneration: 3},
		AllocationPlane: sourceIDDRA,
		CollectedAt:     &metav1.Time{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		SourceRef:       "dra/gpu.nvidia.com/oldpool",
	}
	oldSpec := draDeviceSpec(deviceIDGPU, nodeAName, nodeAUID, "oldpool")
	device := seedDevice(t, testScheme(t), single, deviceIDGPU, oldSpec, seededStatus)

	f := newFixture(t, single, nodeA, device)
	g := NewWithT(t)
	f.sources = []discovery.Source{&fakeSource{
		id:     sourceIDDRA,
		plane:  discovery.PlaneDRA,
		claims: map[string]bool{nodeAName: true},
		enums: map[string]discovery.Enumeration{
			nodeAName: successEnum(draIdentity(nodeAName, nodeAUID, deviceIDGPU, "newpool")),
		},
	}}

	f.mustReconcile(t)

	// State carries the proof: exactly one device exists, carrying the
	// new spec while every seeded status field survives — the inventory
	// controller never touches device /status.
	g.Expect(f.listDevices(t)).To(HaveLen(1))

	updated := f.getDevice(t, deviceIDGPU)
	g.Expect(updated.Spec).To(Equal(draDeviceSpec(deviceIDGPU, nodeAName, nodeAUID, "newpool")))
	// The fake client re-materializes metav1.Time in the process time
	// zone, so CollectedAt compares by instant, not by location.
	g.Expect(updated.Status.Accelerator).To(Equal(seededStatus.Accelerator))
	g.Expect(updated.Status.AllocationPlane).To(Equal(seededStatus.AllocationPlane))
	g.Expect(updated.Status.SourceRef).To(Equal(seededStatus.SourceRef))
	g.Expect(updated.Status.ObservedGeneration).To(Equal(seededStatus.ObservedGeneration))
	g.Expect(updated.Status.CollectedAt.Time.Equal(seededStatus.CollectedAt.Time)).To(BeTrue(),
		"existing status survives every spec update")
}

// resourceSlicesGR is the DRA source's backing group resource, shared by
// the read-failure fixtures.
var resourceSlicesGR = schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceslices"}

// TestReconcilePruneDecisions pairs the authoritative-successful-empty
// deletion against unknown/incomplete retention on the same seeded owned
// child, and checks the condition classification of each unknown cause.
func TestReconcilePruneDecisions(t *testing.T) {
	forbidden := apierrors.NewForbidden(resourceSlicesGR, "", errors.New("rbac"))
	missingAPI := apierrors.NewNotFound(resourceSlicesGR, "")

	cases := []struct {
		name          string
		enum          discovery.Enumeration
		expectDeleted bool
		expectCount   int32
		expectReason  string
		expectOutcome infrastructurev1alpha1.DiscoveryOutcome
	}{
		{
			name:          "authoritative empty prunes the child",
			enum:          successEnum(),
			expectDeleted: true,
			expectCount:   0,
			expectOutcome: outcomeSuccess,
		},
		{
			name:          "incomplete pool retains the child",
			enum:          unknownEnum(errors.Join(dra.ErrPoolIncomplete)),
			expectDeleted: false,
			expectCount:   1,
			expectReason:  "PoolIncomplete",
			expectOutcome: outcomeUnknown,
		},
		{
			name:          "forbidden source retains the child",
			enum:          unknownEnum(forbidden),
			expectDeleted: false,
			expectCount:   1,
			expectReason:  "SourceForbidden",
			expectOutcome: outcomeUnknown,
		},
		{
			name:          "missing source api retains the child",
			enum:          unknownEnum(missingAPI),
			expectDeleted: false,
			expectCount:   1,
			expectReason:  "SourcesUnavailable",
			expectOutcome: outcomeUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			nodeA := testNode(nodeAName, nodeAUID, nil)
			single := testSingleton()
			spec := draDeviceSpec(deviceIDGPU, nodeAName, nodeAUID, "workerpool")
			status := infrastructurev1alpha1.HardwareDeviceStatus{
				Accelerator:     &infrastructurev1alpha1.AcceleratorDeviceStatus{Model: modelTesla},
				AllocationPlane: sourceIDDRA,
			}
			device := seedDevice(t, testScheme(t), single, deviceIDGPU, spec, status)

			f := newFixture(t, single, nodeA, device)
			f.sources = []discovery.Source{&fakeSource{
				id:     sourceIDDRA,
				plane:  discovery.PlaneDRA,
				claims: map[string]bool{nodeAName: true},
				enums:  map[string]discovery.Enumeration{nodeAName: tc.enum},
			}}

			f.mustReconcile(t)

			if tc.expectDeleted {
				f.deviceMissing(t, deviceIDGPU)
			} else {
				retained := f.getDevice(t, deviceIDGPU)
				g.Expect(retained.Spec).To(Equal(spec))
				g.Expect(retained.Status).To(Equal(status), "unknown evidence retains facts")
				g.Expect(retained.Annotations).NotTo(HaveKey(inventory.AnnotationNodeOrphanedAt),
					"a present node is never orphaned")
			}

			updated := f.getSingleton(t)
			ready := condition(g, updated, inventory.ConditionTypeReady)
			if tc.expectReason == "" {
				g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(ready.Reason).To(Equal("Reconciled"))
			} else {
				g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(ready.Reason).To(Equal(tc.expectReason))
				g.Expect(ready.ObservedGeneration).To(Equal(int64(1)))
			}
			g.Expect(updated.Status.NodeSummaries).To(Equal([]infrastructurev1alpha1.NodeSummary{{
				Name:        nodeAName,
				UID:         types.UID(nodeAUID),
				Plane:       sourceIDDRA,
				DeviceCount: tc.expectCount,
				Outcome:     tc.expectOutcome,
			}}))
		})
	}
}

// TestReconcileZeroAdvertisementPruning is the P1.1 regression: with the
// production source factory over a fake cluster and the real DRA source,
// deleting a node's last advertisement makes the source's own
// enumeration of that node a successful empty set. Even though the now
// unclaimed node folds to Unknown, the source-backed device is
// pruned as an authoritative disappearance — while a forbidden read and
// an incomplete snapshot retain it, and a never-claimed node stays
// Unknown with nothing fabricated or pruned.
func TestReconcileZeroAdvertisementPruning(t *testing.T) {
	const (
		gpuUUID   = "GPU-11111111-2222-3333-4444-555555555555"
		sliceName = "gpu-node-a-pool"
		poolName  = "gpu-node-a-pool"
	)
	nodeA := testNode(nodeAName, nodeAUID, nil)
	plain := testNode(plainName, plainUID, nil)
	single := testSingleton()
	disableClassic(single)
	slice := gpuSlice(sliceName, nodeAName, poolName, gpuUUID, modelTesla)
	deviceName := discovery.DeviceName(gpuUUID)

	f := newFixture(t, single, nodeA, plain, slice)
	g := NewWithT(t)
	f.useRealSources(f.inner)
	// Discover: the DRA-backed device exists; the never-claimed node
	// stays a benign Unknown.
	f.mustReconcile(t)
	discovered := f.getDevice(t, deviceName)
	g.Expect(discovered.Spec).To(Equal(draDeviceSpec(gpuUUID, nodeAName, nodeAUID, poolName)))
	g.Expect(f.getSingleton(t).Status.NodeSummaries).To(Equal([]infrastructurev1alpha1.NodeSummary{
		{Name: nodeAName, UID: types.UID(nodeAUID), Plane: sourceIDDRA, DeviceCount: 1, Outcome: outcomeSuccess},
		{Name: plainName, UID: types.UID(plainUID), DeviceCount: 0, Outcome: outcomeUnknown},
	}))

	t.Run("zero advertisement prunes the source-backed device", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(f.client.Delete(context.Background(), slice)).To(Succeed())
		f.mustReconcile(t)

		f.deviceMissing(t, deviceName)

		updated := f.getSingleton(t)
		g.Expect(updated.Status.NodeSummaries).To(Equal([]infrastructurev1alpha1.NodeSummary{
			{Name: nodeAName, UID: types.UID(nodeAUID), DeviceCount: 0, Outcome: outcomeUnknown},
			{Name: plainName, UID: types.UID(plainUID), DeviceCount: 0, Outcome: outcomeUnknown},
		}), "a zero-advertisement node folds to Unknown but is not an error")
		ready := condition(g, updated, inventory.ConditionTypeReady)
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue),
			"the benign unknown of a zero-advertisement node is not degraded")
	})

	t.Run("forbidden read retains the source-backed device", func(t *testing.T) {
		g := NewWithT(t)
		// Recreate the advertisement, rediscover, then fail the reads.
		// The fake client rejects Create with a resourceVersion set.
		slice.ResourceVersion = ""
		g.Expect(f.client.Create(context.Background(), slice)).To(Succeed())
		f.mustReconcile(t)
		g.Expect(f.getDevice(t, deviceName).Spec).To(Equal(draDeviceSpec(gpuUUID, nodeAName, nodeAUID, poolName)))

		f.useRealSources(interceptor.NewClient(f.inner, interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, isSlices := list.(*resourcev1.ResourceSliceList); isSlices {
					return apierrors.NewForbidden(resourceSlicesGR, "", errors.New("rbac"))
				}
				return c.List(ctx, list, opts...)
			},
		}))
		f.mustReconcile(t)

		retained := f.getDevice(t, deviceName)
		g.Expect(retained.Spec).To(Equal(draDeviceSpec(gpuUUID, nodeAName, nodeAUID, poolName)))
		ready := condition(g, f.getSingleton(t), inventory.ConditionTypeReady)
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(ready.Reason).To(Equal("SourceForbidden"))

		f.useRealSources(f.inner)
	})

	t.Run("incomplete snapshot retains the source-backed device", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(f.client.Delete(context.Background(), slice)).To(Succeed())
		incomplete := incompleteSlicePool(sliceName+"-incomplete", nodeAName, poolName, gpuUUID, modelTesla)
		g.Expect(f.client.Create(context.Background(), incomplete)).To(Succeed())
		f.mustReconcile(t)

		retained := f.getDevice(t, deviceName)
		g.Expect(retained.Spec).To(Equal(draDeviceSpec(gpuUUID, nodeAName, nodeAUID, poolName)))
		ready := condition(g, f.getSingleton(t), inventory.ConditionTypeReady)
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(ready.Reason).To(Equal("PoolIncomplete"))
	})
}

// TestReconcileSelectorPruning covers deliberate selector removal: a
// trustworthy evaluation prunes the deselected node's children
// immediately.
func TestReconcileSelectorPruning(t *testing.T) {
	nodeA := testNode(nodeAName, nodeAUID, map[string]string{selectorKeyPool: "default"})
	single := testSingleton()

	f := newFixture(t, single, nodeA)
	g := NewWithT(t)
	f.sources = []discovery.Source{&fakeSource{
		id:     sourceIDDRA,
		plane:  discovery.PlaneDRA,
		claims: map[string]bool{nodeAName: true},
		enums: map[string]discovery.Enumeration{
			nodeAName: successEnum(draIdentity(nodeAName, nodeAUID, deviceIDGPU, "workerpool")),
		},
	}}

	f.mustReconcile(t)
	g.Expect(f.listDevices(t)).To(HaveLen(1))

	// Narrow the selector so node A no longer matches.
	single = f.getSingleton(t)
	single.Spec.NodeSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{selectorKeyPool: "other"},
	}
	single.Generation = 2
	g.Expect(f.client.Update(context.Background(), single)).To(Succeed())

	f.mustReconcile(t)

	f.deviceMissing(t, deviceIDGPU)
	updated := f.getSingleton(t)
	g.Expect(updated.Status.NodeSummaries).To(BeEmpty())
	ready := condition(g, updated, inventory.ConditionTypeReady)
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
}

// TestReconcileCollisionGuard proves a derived name held by a foreign
// object is never adopted or touched.
func TestReconcileCollisionGuard(t *testing.T) {
	nodeA := testNode(nodeAName, nodeAUID, nil)
	single := testSingleton()

	foreignSpec := draDeviceSpec("GPU-other", "other-node", "other-uid", "foreignpool")
	foreignStatus := infrastructurev1alpha1.HardwareDeviceStatus{
		AllocationPlane: sourceIDDRA,
		SourceRef:       "dra/gpu.nvidia.com/foreignpool",
	}
	foreign := &infrastructurev1alpha1.HardwareDevice{
		ObjectMeta: metav1.ObjectMeta{Name: deviceIDGPU, UID: "foreign-uid"},
		Spec:       foreignSpec,
		Status:     foreignStatus,
	}

	f := newFixture(t, single, nodeA, foreign)
	g := NewWithT(t)
	f.sources = []discovery.Source{&fakeSource{
		id:     sourceIDDRA,
		plane:  discovery.PlaneDRA,
		claims: map[string]bool{nodeAName: true},
		enums: map[string]discovery.Enumeration{
			nodeAName: successEnum(draIdentity(nodeAName, nodeAUID, deviceIDGPU, "workerpool")),
		},
	}}

	f.mustReconcile(t)

	g.Expect(f.listDevices(t)).To(HaveLen(1), "only the foreign device exists")

	untouched := f.getDevice(t, deviceIDGPU)
	g.Expect(untouched.Spec).To(Equal(foreignSpec))
	g.Expect(untouched.Status).To(Equal(foreignStatus))
	g.Expect(untouched.OwnerReferences).To(BeEmpty())

	updated := f.getSingleton(t)
	ready := condition(g, updated, inventory.ConditionTypeReady)
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(ready.Reason).To(Equal("NameCollision"))
}

// TestReconcileNodeGraceLifecycle covers the persistent node-absence
// grace: the annotation stamps the first observation, a controller
// restart cannot reset it, and expiry prunes the device.
func TestReconcileNodeGraceLifecycle(t *testing.T) {
	nodeA := testNode(nodeAName, nodeAUID, nil)
	single := testSingleton()

	f := newFixture(t, single, nodeA)
	g := NewWithT(t)
	f.sources = []discovery.Source{&fakeSource{
		id:     sourceIDDRA,
		plane:  discovery.PlaneDRA,
		claims: map[string]bool{nodeAName: true},
		enums: map[string]discovery.Enumeration{
			nodeAName: successEnum(draIdentity(nodeAName, nodeAUID, deviceIDGPU, "workerpool")),
		},
	}}

	f.mustReconcile(t)
	g.Expect(f.listDevices(t)).To(HaveLen(1))

	// The node disappears: the device is retained and stamped.
	g.Expect(f.client.Delete(context.Background(), nodeA)).To(Succeed())
	f.clock.advance(time.Minute)
	f.mustReconcile(t)

	stamped := f.getDevice(t, deviceIDGPU)
	stamp, ok := stamped.Annotations[inventory.AnnotationNodeOrphanedAt]
	g.Expect(ok).To(BeTrue(), "orphaned devices carry the grace annotation")
	g.Expect(stamp).To(Equal(f.clock.now.UTC().Format(time.RFC3339)))

	// A restart (fresh reconciler, same cluster state) must not reset the
	// persisted deadline while the grace window is still running.
	f.clock.advance(10 * time.Minute)
	restarted := f.reconciler(t)
	_, err := restarted.Reconcile(context.Background(), request())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(f.getDevice(t, deviceIDGPU).Annotations[inventory.AnnotationNodeOrphanedAt]).
		To(Equal(stamp), "a restart cannot restamp the grace start")

	// Still inside the 15m window measured from the original stamp
	// (13 minutes elapsed).
	f.clock.advance(3 * time.Minute)
	f.mustReconcile(t)
	g.Expect(f.listDevices(t)).To(HaveLen(1))

	// Past the deadline (15 minutes and a second elapsed): the device is
	// pruned.
	f.clock.advance(2*time.Minute + time.Second)
	f.mustReconcile(t)
	f.deviceMissing(t, deviceIDGPU)
}

// TestReconcileNodeRecovery covers node recovery: the same node (same
// UID) coming back removes the grace annotation and keeps the device.
func TestReconcileNodeRecovery(t *testing.T) {
	nodeA := testNode(nodeAName, nodeAUID, nil)
	single := testSingleton()

	f := newFixture(t, single, nodeA)
	g := NewWithT(t)
	f.sources = []discovery.Source{&fakeSource{
		id:     sourceIDDRA,
		plane:  discovery.PlaneDRA,
		claims: map[string]bool{nodeAName: true},
		enums: map[string]discovery.Enumeration{
			nodeAName: successEnum(draIdentity(nodeAName, nodeAUID, deviceIDGPU, "workerpool")),
		},
	}}

	f.mustReconcile(t)

	// Orphan, then recover with the same node incarnation.
	g.Expect(f.client.Delete(context.Background(), nodeA)).To(Succeed())
	f.clock.advance(time.Minute)
	f.mustReconcile(t)
	g.Expect(f.getDevice(t, deviceIDGPU).Annotations).To(HaveKey(inventory.AnnotationNodeOrphanedAt))

	// The fake client rejects Create with a resourceVersion set; the
	// recovered node keeps its name and UID but is otherwise fresh.
	nodeA.ResourceVersion = ""
	g.Expect(f.client.Create(context.Background(), nodeA)).To(Succeed())
	f.clock.advance(time.Minute)
	f.mustReconcile(t)

	recovered := f.getDevice(t, deviceIDGPU)
	g.Expect(recovered.Annotations).NotTo(HaveKey(inventory.AnnotationNodeOrphanedAt),
		"recovery removes the grace annotation")
	g.Expect(recovered.Spec).To(Equal(draDeviceSpec(deviceIDGPU, nodeAName, nodeAUID, "workerpool")))
}

// TestReconcileNodeReplacement covers node name reuse: the same device id
// on a new Node UID is a new incarnation. The old object is orphaned and
// the fresh object waits for the grace prune — node reuse can neither
// freshen the old facts nor bind the stale locator to the new host.
func TestReconcileNodeReplacement(t *testing.T) {
	nodeA := testNode(nodeAName, nodeAUID, nil)
	single := testSingleton()

	f := newFixture(t, single, nodeA)
	g := NewWithT(t)
	f.sources = []discovery.Source{&fakeSource{
		id:     sourceIDDRA,
		plane:  discovery.PlaneDRA,
		claims: map[string]bool{nodeAName: true},
	}}

	enumerate := func(uid string) {
		f.sources[0].(*fakeSource).enums = map[string]discovery.Enumeration{
			nodeAName: successEnum(draIdentity(nodeAName, uid, deviceIDGPU, "workerpool")),
		}
	}
	enumerate(nodeAUID)

	// Initial incarnation with observed facts (written by the device
	// controller, not by this controller).
	f.mustReconcile(t)
	old := f.getDevice(t, deviceIDGPU)
	old.Status = infrastructurev1alpha1.HardwareDeviceStatus{
		Accelerator:     &infrastructurev1alpha1.AcceleratorDeviceStatus{Model: modelTesla},
		AllocationPlane: sourceIDDRA,
	}
	g.Expect(f.client.Status().Update(context.Background(), old)).To(Succeed())

	// Replace the node: same name, new UID.
	g.Expect(f.client.Delete(context.Background(), nodeA)).To(Succeed())
	replacement := testNode(nodeAName, "uid-new", nil)
	g.Expect(f.client.Create(context.Background(), replacement)).To(Succeed())
	enumerate("uid-new")

	// During grace the old object is retained and orphaned, and the new
	// incarnation waits for the deterministic name.
	f.mustReconcile(t)
	g.Expect(f.listDevices(t)).To(HaveLen(1), "the new incarnation defers while grace holds the name")
	orphaned := f.getDevice(t, deviceIDGPU)
	g.Expect(orphaned.Spec.Host.UID).To(Equal(types.UID(nodeAUID)))
	g.Expect(orphaned.Annotations).To(HaveKey(inventory.AnnotationNodeOrphanedAt))
	g.Expect(orphaned.Status.Accelerator.Model).To(Equal(modelTesla), "old facts are not refreshed")

	// After grace the old incarnation is pruned and a fresh object with
	// an empty status is created for the new node.
	f.clock.advance(16 * time.Minute)
	f.mustReconcile(t)
	g.Expect(f.listDevices(t)).To(HaveLen(1))
	fresh := f.getDevice(t, deviceIDGPU)
	g.Expect(fresh.Spec).To(Equal(draDeviceSpec(deviceIDGPU, nodeAName, "uid-new", "workerpool")))
	g.Expect(fresh.Status).To(Equal(infrastructurev1alpha1.HardwareDeviceStatus{}),
		"status is never transferred between incarnations")

	updated := f.getSingleton(t)
	g.Expect(updated.Status.NodeSummaries).To(ContainElement(infrastructurev1alpha1.NodeSummary{
		Name:        nodeAName,
		UID:         "uid-new",
		Plane:       sourceIDDRA,
		DeviceCount: 1,
		Outcome:     outcomeSuccess,
	}))
}

// TestReconcileCrossNodeAmbiguity covers the same id reported by two
// simultaneously live nodes: every incarnation is rejected.
func TestReconcileCrossNodeAmbiguity(t *testing.T) {
	nodeA := testNode(nodeAName, nodeAUID, nil)
	nodeB := testNode(nodeBName, nodeBUID, nil)
	single := testSingleton()

	f := newFixture(t, single, nodeA, nodeB)
	g := NewWithT(t)
	f.sources = []discovery.Source{&fakeSource{
		id:     sourceIDDRA,
		plane:  discovery.PlaneDRA,
		claims: map[string]bool{nodeAName: true, nodeBName: true},
		enums: map[string]discovery.Enumeration{
			nodeAName: successEnum(draIdentity(nodeAName, nodeAUID, deviceIDGPU, "workerpool")),
			nodeBName: successEnum(draIdentity(nodeBName, nodeBUID, deviceIDGPU, "workerpool")),
		},
	}}

	f.mustReconcile(t)

	g.Expect(f.listDevices(t)).To(BeEmpty())
	updated := f.getSingleton(t)
	ready := condition(g, updated, inventory.ConditionTypeReady)
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(ready.Reason).To(Equal("IdentityAmbiguous"))
}

// TestReconcileInvalidConfiguration covers actionable conditions for
// invalid configuration and the retention of existing children.
func TestReconcileInvalidConfiguration(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(single *infrastructurev1alpha1.HardwareDiscovery)
	}{
		{
			name: "invalid node selector",
			mutate: func(single *infrastructurev1alpha1.HardwareDiscovery) {
				single.Spec.NodeSelector = &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{Key: "bad key", Operator: "Exists"},
					},
				}
			},
		},
		{
			name: "unsupported class",
			mutate: func(single *infrastructurev1alpha1.HardwareDiscovery) {
				single.Spec.Class = "cpu"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			nodeA := testNode(nodeAName, nodeAUID, nil)
			single := testSingleton()
			tc.mutate(single)
			spec := draDeviceSpec(deviceIDGPU, nodeAName, nodeAUID, "workerpool")
			device := seedDevice(t, testScheme(t), single, deviceIDGPU, spec,
				infrastructurev1alpha1.HardwareDeviceStatus{})

			f := newFixture(t, single, nodeA, device)
			f.sources = []discovery.Source{&fakeSource{id: sourceIDDRA, plane: discovery.PlaneDRA}}

			result := f.mustReconcile(t)
			g.Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

			updated := f.getSingleton(t)
			ready := condition(g, updated, inventory.ConditionTypeReady)
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal("InvalidConfiguration"))
			g.Expect(ready.Message).To(ContainSubstring("invalid discovery configuration"))

			// An untrustworthy evaluation never prunes children.
			retained := f.getDevice(t, deviceIDGPU)
			g.Expect(retained.Spec).To(Equal(spec))
		})
	}
}

// TestReconcileTransientErrors proves failed reads return errors for
// backoff instead of being mistaken for authoritative absence. The node
// list is failed through a standard fake-client interceptor; the
// framework client converts typed lists to unstructured before they
// reach the client, so the failure is keyed on the NodeList GVK.
func TestReconcileTransientErrors(t *testing.T) {
	nodeA := testNode(nodeAName, nodeAUID, nil)
	single := testSingleton()

	f := newFixture(t, single, nodeA)
	g := NewWithT(t)
	f.sources = []discovery.Source{&fakeSource{id: sourceIDDRA, plane: discovery.PlaneDRA}}
	f.client = interceptor.NewClient(f.inner, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			gvk := list.GetObjectKind().GroupVersionKind()
			if gvk.Group == "" && gvk.Version == "v1" && gvk.Kind == "NodeList" {
				return errors.New("node list boom")
			}
			return c.List(ctx, list, opts...)
		},
	})

	_, err := f.reconciler(t).Reconcile(context.Background(), request())
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("listing nodes"))
}

// TestReconcileIgnoresAbsentSingleton covers the reset path and foreign
// request names: both are plain successes, and the cluster state proves
// the point — no singleton materializes and no device is fabricated.
func TestReconcileIgnoresAbsentSingleton(t *testing.T) {
	f := newFixture(t)
	g := NewWithT(t)
	f.sources = []discovery.Source{&fakeSource{id: sourceIDDRA, plane: discovery.PlaneDRA}}

	f.mustReconcile(t)

	single := &infrastructurev1alpha1.HardwareDiscovery{}
	err := f.client.Get(context.Background(), request().NamespacedName, single)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the singleton is never fabricated")
	g.Expect(f.listDevices(t)).To(BeEmpty())

	// A request for any other name is not ours to reconcile.
	result, err := f.reconciler(t).Reconcile(context.Background(),
		requestFor("something-else"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))
}

// request builds the singleton reconcile request.
func request() reconcile.Request {
	return requestFor(inventory.SingletonName)
}

// requestFor builds a reconcile request for the given object name.
func requestFor(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
}
