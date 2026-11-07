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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
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
// of any kind.
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
	f.sources = []discovery.Source{draSrc, classicSrc}

	result := f.mustReconcile()
	f.g.Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

	f.g.Expect(f.listDevices()).To(HaveLen(4))

	// DRA device: spec, ownership, labels, empty status.
	draDevice := f.getDevice(deviceIDGPU)
	f.g.Expect(draDevice.Spec).To(Equal(draDeviceSpec(nodeAName, nodeAUID, "workerpool")))
	f.g.Expect(draDevice.Status).To(Equal(infrastructurev1alpha1.HardwareDeviceStatus{}),
		"the inventory controller never writes device status")
	ref := metav1.GetControllerOf(draDevice)
	f.g.Expect(ref).NotTo(BeNil())
	f.g.Expect(ref.Kind).To(Equal("HardwareDiscovery"))
	f.g.Expect(ref.Name).To(Equal(inventory.SingletonName))
	f.g.Expect(ref.UID).To(Equal(types.UID(singletonUID)))
	f.g.Expect(draDevice.OwnerReferences).To(HaveLen(1), "no Node owner reference is ever added")
	f.g.Expect(draDevice.Labels).To(Equal(map[string]string{
		deviceClassLabel: classTest,
		nodeLabel:        nodeAName,
	}))

	// Classic device: nodeIndex identity with a classic locator.
	classicDevice := f.getDevice(nodeBName + "-gpu-0")
	f.g.Expect(classicDevice.Spec).To(Equal(classicDeviceSpec(nodeBName, nodeBUID, 0)))

	// Singleton status: summaries and conditions with the generation.
	updated := f.getSingleton()
	f.g.Expect(updated.Status.ObservedGeneration).To(Equal(int64(1)))
	f.g.Expect(updated.Status.NodeSummaries).To(Equal([]infrastructurev1alpha1.NodeSummary{
		{Name: nodeAName, UID: types.UID(nodeAUID), Plane: sourceIDDRA, DeviceCount: 2, Outcome: outcomeSuccess},
		{Name: nodeBName, UID: types.UID(nodeBUID), Plane: sourceIDClassic, DeviceCount: 2, Outcome: outcomeSuccess},
		{Name: plainName, UID: types.UID(plainUID), DeviceCount: 0, Outcome: outcomeUnknown},
	}))
	ready := condition(f.g, updated, inventory.ConditionTypeReady)
	f.g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	f.g.Expect(ready.Reason).To(Equal("Reconciled"))
	f.g.Expect(ready.ObservedGeneration).To(Equal(int64(1)))
	degraded := condition(f.g, updated, inventory.ConditionTypeDegraded)
	f.g.Expect(degraded.Status).To(Equal(metav1.ConditionFalse))
	f.g.Expect(degraded.Reason).To(Equal("Reconciled"))

	// A second, unchanged scan writes nothing at all.
	f.client.reset()
	f.mustReconcile()
	f.g.Expect(f.client.creates).To(BeEmpty())
	f.g.Expect(f.client.updates).To(BeEmpty())
	f.g.Expect(f.client.deletes).To(BeEmpty())
	f.g.Expect(f.client.statusUpdates).To(BeEmpty())
}

// TestReconcileUpdatesSpecAndRetainsStatus proves a locator change is a
// primary-endpoint spec write only: the child's existing status — written
// by the device controller — survives every spec update, and no /status
// call is ever made.
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
	oldSpec := draDeviceSpec(nodeAName, nodeAUID, "oldpool")
	device := seedDevice(t, testScheme(t), single, deviceIDGPU, oldSpec, seededStatus)

	f := newFixture(t, single, nodeA, device)
	f.sources = []discovery.Source{&fakeSource{
		id:     sourceIDDRA,
		plane:  discovery.PlaneDRA,
		claims: map[string]bool{nodeAName: true},
		enums: map[string]discovery.Enumeration{
			nodeAName: successEnum(draIdentity(nodeAName, nodeAUID, deviceIDGPU, "newpool")),
		},
	}}

	f.mustReconcile()

	f.g.Expect(f.client.updates).To(HaveLen(1), "exactly one primary-endpoint spec write")
	f.g.Expect(f.client.creates).To(BeEmpty())
	f.g.Expect(f.client.deletes).To(BeEmpty())
	// The singleton's own status is this controller's to write; the
	// device's never is.
	f.g.Expect(f.client.statusUpdates).NotTo(ContainElement(ContainSubstring("HardwareDevice")),
		"the inventory controller never touches device /status")

	updated := f.getDevice(deviceIDGPU)
	f.g.Expect(updated.Spec).To(Equal(draDeviceSpec(nodeAName, nodeAUID, "newpool")))
	// The fake client re-materializes metav1.Time in the process time
	// zone, so CollectedAt compares by instant, not by location.
	f.g.Expect(updated.Status.Accelerator).To(Equal(seededStatus.Accelerator))
	f.g.Expect(updated.Status.AllocationPlane).To(Equal(seededStatus.AllocationPlane))
	f.g.Expect(updated.Status.SourceRef).To(Equal(seededStatus.SourceRef))
	f.g.Expect(updated.Status.ObservedGeneration).To(Equal(seededStatus.ObservedGeneration))
	f.g.Expect(updated.Status.CollectedAt.Time.Equal(seededStatus.CollectedAt.Time)).To(BeTrue(),
		"existing status survives every spec update")
}

// TestReconcilePruneDecisions pairs the authoritative-successful-empty
// deletion against unknown/incomplete retention on the same seeded owned
// child, and checks the condition classification of each unknown cause.
func TestReconcilePruneDecisions(t *testing.T) {
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceslices"},
		"", errors.New("rbac"))
	missingAPI := apierrors.NewNotFound(
		schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceslices"}, "")

	cases := []struct {
		name          string
		enum          discovery.Enumeration
		expectDeleted bool
		expectCount   int32
		expectReason  string
		expectOutcome string
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
			spec := draDeviceSpec(nodeAName, nodeAUID, "workerpool")
			status := infrastructurev1alpha1.HardwareDeviceStatus{
				Accelerator:     &infrastructurev1alpha1.AcceleratorDeviceStatus{Model: modelTesla},
				AllocationPlane: sourceIDDRA,
			}
			device := seedDevice(t, testScheme(t), single, deviceIDGPU, spec, status)

			f := newFixture(t, single, nodeA, device)
			f.g = g
			f.sources = []discovery.Source{&fakeSource{
				id:     sourceIDDRA,
				plane:  discovery.PlaneDRA,
				claims: map[string]bool{nodeAName: true},
				enums:  map[string]discovery.Enumeration{nodeAName: tc.enum},
			}}

			f.mustReconcile()

			if tc.expectDeleted {
				f.deviceMissing(deviceIDGPU)
			} else {
				retained := f.getDevice(deviceIDGPU)
				g.Expect(retained.Spec).To(Equal(spec))
				g.Expect(retained.Status).To(Equal(status), "unknown evidence retains facts")
				g.Expect(retained.Annotations).NotTo(HaveKey(inventory.AnnotationNodeOrphanedAt),
					"a present node is never orphaned")
			}

			updated := f.getSingleton()
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

// TestReconcileSelectorPruning covers deliberate selector removal: a
// trustworthy evaluation prunes the deselected node's children
// immediately.
func TestReconcileSelectorPruning(t *testing.T) {
	nodeA := testNode(nodeAName, nodeAUID, map[string]string{selectorKeyPool: "default"})
	single := testSingleton()

	f := newFixture(t, single, nodeA)
	f.sources = []discovery.Source{&fakeSource{
		id:     sourceIDDRA,
		plane:  discovery.PlaneDRA,
		claims: map[string]bool{nodeAName: true},
		enums: map[string]discovery.Enumeration{
			nodeAName: successEnum(draIdentity(nodeAName, nodeAUID, deviceIDGPU, "workerpool")),
		},
	}}

	f.mustReconcile()
	f.g.Expect(f.listDevices()).To(HaveLen(1))

	// Narrow the selector so node A no longer matches.
	single = f.getSingleton()
	single.Spec.NodeSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{selectorKeyPool: "other"},
	}
	single.Generation = 2
	f.g.Expect(f.client.Update(context.Background(), single)).To(Succeed())
	f.client.reset()

	f.mustReconcile()

	f.deviceMissing(deviceIDGPU)
	updated := f.getSingleton()
	f.g.Expect(updated.Status.NodeSummaries).To(BeEmpty())
	ready := condition(f.g, updated, inventory.ConditionTypeReady)
	f.g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
}

// TestReconcileCollisionGuard proves a derived name held by a foreign
// object is never adopted or touched.
func TestReconcileCollisionGuard(t *testing.T) {
	nodeA := testNode(nodeAName, nodeAUID, nil)
	single := testSingleton()

	foreignSpec := draDeviceSpec("other-node", "other-uid", "foreignpool")
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
	f.sources = []discovery.Source{&fakeSource{
		id:     sourceIDDRA,
		plane:  discovery.PlaneDRA,
		claims: map[string]bool{nodeAName: true},
		enums: map[string]discovery.Enumeration{
			nodeAName: successEnum(draIdentity(nodeAName, nodeAUID, deviceIDGPU, "workerpool")),
		},
	}}

	f.mustReconcile()

	f.g.Expect(f.client.creates).To(BeEmpty(), "a colliding name is never created over")
	f.g.Expect(f.client.updates).To(BeEmpty(), "a foreign object is never touched")
	f.g.Expect(f.client.deletes).To(BeEmpty())
	f.g.Expect(f.listDevices()).To(HaveLen(1), "only the foreign device exists")

	untouched := f.getDevice(deviceIDGPU)
	f.g.Expect(untouched.Spec).To(Equal(foreignSpec))
	f.g.Expect(untouched.Status).To(Equal(foreignStatus))
	f.g.Expect(untouched.OwnerReferences).To(BeEmpty())

	updated := f.getSingleton()
	ready := condition(f.g, updated, inventory.ConditionTypeReady)
	f.g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	f.g.Expect(ready.Reason).To(Equal("NameCollision"))
}

// TestReconcileNodeGraceLifecycle covers the persistent node-absence
// grace: the annotation stamps the first observation, a controller
// restart cannot reset it, and expiry prunes the device.
func TestReconcileNodeGraceLifecycle(t *testing.T) {
	nodeA := testNode(nodeAName, nodeAUID, nil)
	single := testSingleton()

	f := newFixture(t, single, nodeA)
	f.sources = []discovery.Source{&fakeSource{
		id:     sourceIDDRA,
		plane:  discovery.PlaneDRA,
		claims: map[string]bool{nodeAName: true},
		enums: map[string]discovery.Enumeration{
			nodeAName: successEnum(draIdentity(nodeAName, nodeAUID, deviceIDGPU, "workerpool")),
		},
	}}

	f.mustReconcile()
	f.g.Expect(f.listDevices()).To(HaveLen(1))

	// The node disappears: the device is retained and stamped.
	f.g.Expect(f.client.Delete(context.Background(), nodeA)).To(Succeed())
	f.clock.advance(time.Minute)
	f.mustReconcile()

	stamped := f.getDevice(deviceIDGPU)
	stamp, ok := stamped.Annotations[inventory.AnnotationNodeOrphanedAt]
	f.g.Expect(ok).To(BeTrue(), "orphaned devices carry the grace annotation")
	f.g.Expect(stamp).To(Equal(f.clock.now.UTC().Format(time.RFC3339)))

	// A restart (fresh reconciler, same cluster state) must not reset the
	// persisted deadline while the grace window is still running.
	f.clock.advance(10 * time.Minute)
	restarted := f.reconciler()
	_, err := restarted.Reconcile(context.Background(), request())
	f.g.Expect(err).NotTo(HaveOccurred())
	f.g.Expect(f.getDevice(deviceIDGPU).Annotations[inventory.AnnotationNodeOrphanedAt]).
		To(Equal(stamp), "a restart cannot restamp the grace start")

	// Still inside the 15m window measured from the original stamp
	// (13 minutes elapsed).
	f.clock.advance(3 * time.Minute)
	f.mustReconcile()
	f.g.Expect(f.listDevices()).To(HaveLen(1))

	// Past the deadline (15 minutes and a second elapsed): the device is
	// pruned.
	f.clock.advance(2*time.Minute + time.Second)
	f.client.reset()
	f.mustReconcile()
	f.deviceMissing(deviceIDGPU)
	f.g.Expect(f.client.deletes).To(HaveLen(1))
}

// TestReconcileNodeRecovery covers node recovery: the same node (same
// UID) coming back removes the grace annotation and keeps the device.
func TestReconcileNodeRecovery(t *testing.T) {
	nodeA := testNode(nodeAName, nodeAUID, nil)
	single := testSingleton()

	f := newFixture(t, single, nodeA)
	f.sources = []discovery.Source{&fakeSource{
		id:     sourceIDDRA,
		plane:  discovery.PlaneDRA,
		claims: map[string]bool{nodeAName: true},
		enums: map[string]discovery.Enumeration{
			nodeAName: successEnum(draIdentity(nodeAName, nodeAUID, deviceIDGPU, "workerpool")),
		},
	}}

	f.mustReconcile()

	// Orphan, then recover with the same node incarnation.
	f.g.Expect(f.client.Delete(context.Background(), nodeA)).To(Succeed())
	f.clock.advance(time.Minute)
	f.mustReconcile()
	f.g.Expect(f.getDevice(deviceIDGPU).Annotations).To(HaveKey(inventory.AnnotationNodeOrphanedAt))

	// The fake client rejects Create with a resourceVersion set; the
	// recovered node keeps its name and UID but is otherwise fresh.
	nodeA.ResourceVersion = ""
	f.g.Expect(f.client.Create(context.Background(), nodeA)).To(Succeed())
	f.clock.advance(time.Minute)
	f.mustReconcile()

	recovered := f.getDevice(deviceIDGPU)
	f.g.Expect(recovered.Annotations).NotTo(HaveKey(inventory.AnnotationNodeOrphanedAt),
		"recovery removes the grace annotation")
	f.g.Expect(recovered.Spec).To(Equal(draDeviceSpec(nodeAName, nodeAUID, "workerpool")))
}

// TestReconcileNodeReplacement covers node name reuse: the same device id
// on a new Node UID is a new incarnation. The old object is orphaned and
// the fresh object waits for the grace prune — node reuse can neither
// freshen the old facts nor bind the stale locator to the new host.
func TestReconcileNodeReplacement(t *testing.T) {
	nodeA := testNode(nodeAName, nodeAUID, nil)
	single := testSingleton()

	f := newFixture(t, single, nodeA)
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
	f.mustReconcile()
	old := f.getDevice(deviceIDGPU)
	old.Status = infrastructurev1alpha1.HardwareDeviceStatus{
		Accelerator:     &infrastructurev1alpha1.AcceleratorDeviceStatus{Model: modelTesla},
		AllocationPlane: sourceIDDRA,
	}
	f.g.Expect(f.client.Status().Update(context.Background(), old)).To(Succeed())

	// Replace the node: same name, new UID.
	f.g.Expect(f.client.Delete(context.Background(), nodeA)).To(Succeed())
	replacement := testNode(nodeAName, "uid-new", nil)
	f.g.Expect(f.client.Create(context.Background(), replacement)).To(Succeed())
	enumerate("uid-new")

	// During grace the old object is retained and orphaned, and the new
	// incarnation waits for the deterministic name.
	f.mustReconcile()
	f.g.Expect(f.listDevices()).To(HaveLen(1), "the new incarnation defers while grace holds the name")
	orphaned := f.getDevice(deviceIDGPU)
	f.g.Expect(orphaned.Spec.Host.UID).To(Equal(types.UID(nodeAUID)))
	f.g.Expect(orphaned.Annotations).To(HaveKey(inventory.AnnotationNodeOrphanedAt))
	f.g.Expect(orphaned.Status.Accelerator.Model).To(Equal(modelTesla), "old facts are not refreshed")

	// After grace the old incarnation is pruned and a fresh object with
	// an empty status is created for the new node.
	f.clock.advance(16 * time.Minute)
	f.client.reset()
	f.mustReconcile()
	f.g.Expect(f.listDevices()).To(HaveLen(1))
	f.g.Expect(f.client.deletes).To(HaveLen(1), "the prior incarnation is pruned")
	f.g.Expect(f.client.creates).To(HaveLen(1), "the fresh incarnation is created")
	fresh := f.getDevice(deviceIDGPU)
	f.g.Expect(fresh.Spec).To(Equal(draDeviceSpec(nodeAName, "uid-new", "workerpool")))
	f.g.Expect(fresh.Status).To(Equal(infrastructurev1alpha1.HardwareDeviceStatus{}),
		"status is never transferred between incarnations")

	updated := f.getSingleton()
	f.g.Expect(updated.Status.NodeSummaries).To(ContainElement(infrastructurev1alpha1.NodeSummary{
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
	f.sources = []discovery.Source{&fakeSource{
		id:     sourceIDDRA,
		plane:  discovery.PlaneDRA,
		claims: map[string]bool{nodeAName: true, nodeBName: true},
		enums: map[string]discovery.Enumeration{
			nodeAName: successEnum(draIdentity(nodeAName, nodeAUID, deviceIDGPU, "workerpool")),
			nodeBName: successEnum(draIdentity(nodeBName, nodeBUID, deviceIDGPU, "workerpool")),
		},
	}}

	f.mustReconcile()

	f.g.Expect(f.client.creates).To(BeEmpty())
	f.g.Expect(f.listDevices()).To(BeEmpty())
	updated := f.getSingleton()
	ready := condition(f.g, updated, inventory.ConditionTypeReady)
	f.g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	f.g.Expect(ready.Reason).To(Equal("IdentityAmbiguous"))
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
			spec := draDeviceSpec(nodeAName, nodeAUID, "workerpool")
			device := seedDevice(t, testScheme(t), single, deviceIDGPU, spec,
				infrastructurev1alpha1.HardwareDeviceStatus{})

			f := newFixture(t, single, nodeA, device)
			f.g = g
			f.sources = []discovery.Source{&fakeSource{id: sourceIDDRA, plane: discovery.PlaneDRA}}

			result := f.mustReconcile()
			g.Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

			updated := f.getSingleton()
			ready := condition(g, updated, inventory.ConditionTypeReady)
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal("InvalidConfiguration"))
			g.Expect(ready.Message).To(ContainSubstring("invalid discovery configuration"))

			// An untrustworthy evaluation never prunes children.
			retained := f.getDevice(deviceIDGPU)
			g.Expect(retained.Spec).To(Equal(spec))
			g.Expect(f.client.deletes).To(BeEmpty())
		})
	}
}

// TestReconcileTransientErrors proves failed reads return errors for
// backoff instead of being mistaken for authoritative absence.
func TestReconcileTransientErrors(t *testing.T) {
	nodeA := testNode(nodeAName, nodeAUID, nil)
	single := testSingleton()

	f := newFixture(t, single, nodeA)
	f.sources = []discovery.Source{&fakeSource{id: sourceIDDRA, plane: discovery.PlaneDRA}}
	f.client.listNodesErr = errors.New("node list boom")

	_, err := f.reconciler().Reconcile(context.Background(), request())
	f.g.Expect(err).To(HaveOccurred())
	f.g.Expect(err.Error()).To(ContainSubstring("listing nodes"))
}

// TestReconcileIgnoresAbsentSingleton covers the reset path and foreign
// request names: both are plain successes with no writes.
func TestReconcileIgnoresAbsentSingleton(t *testing.T) {
	f := newFixture(t)
	f.sources = []discovery.Source{&fakeSource{id: sourceIDDRA, plane: discovery.PlaneDRA}}

	f.mustReconcile()
	f.g.Expect(f.client.updates).To(BeEmpty())
	f.g.Expect(f.client.statusUpdates).To(BeEmpty())

	// A request for any other name is not ours to reconcile.
	result, err := f.reconciler().Reconcile(context.Background(),
		requestFor("something-else"))
	f.g.Expect(err).NotTo(HaveOccurred())
	f.g.Expect(result).To(Equal(ctrl.Result{}))
}

// request builds the singleton reconcile request.
func request() reconcile.Request {
	return requestFor(inventory.SingletonName)
}

// requestFor builds a reconcile request for the given object name.
func requestFor(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
}
