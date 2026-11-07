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
	"maps"
	"slices"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/inventory"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/classic"
)

// classicNodeLabels builds the GFD-style label set of a classic GPU node
// with the given gpu.count.
func classicNodeLabels(count string) map[string]string {
	return map[string]string{
		classic.LabelGPUProduct: modelTesla,
		classic.LabelGPUMemory:  "11441",
		classic.LabelGPUCount:   count,
	}
}

// stampClassicEvidence stamps a node with the full classic-plane
// evidence contract: the GFD labels plus the default extended resource
// in allocatable and capacity.
func stampClassicEvidence(node *corev1.Node, count string) {
	if node.Labels == nil {
		node.Labels = make(map[string]string, 3)
	}
	maps.Copy(node.Labels, classicNodeLabels(count))
	gpu := corev1.ResourceName(classic.DefaultResourceName)
	quantity := apiresource.MustParse(count)
	if node.Status.Allocatable == nil {
		node.Status.Allocatable = corev1.ResourceList{}
	}
	if node.Status.Capacity == nil {
		node.Status.Capacity = corev1.ResourceList{}
	}
	node.Status.Allocatable[gpu] = quantity
	node.Status.Capacity[gpu] = quantity
}

// classicEvidenceNode builds a node carrying the full classic evidence
// contract with the given gpu.count.
func classicEvidenceNode(name string, uid string, count string) *corev1.Node {
	node := testNode(name, uid, nil)
	stampClassicEvidence(node, count)
	return node
}

// updateNode fetches a fresh copy of the named node, applies mutate and
// persists the result. The fake client shields a node's status on plain
// Update — even without a registered status subresource, it also
// restores the tracked status on the passed object — while the status
// endpoint accepts the write. Labels/meta and status evidence are
// therefore persisted as two writes, the status one on a freshly
// fetched copy carrying the current resourceVersion and the mutated
// status captured before the plain update.
//
// nolint:unparam // every retention scenario mutates the evidence node A; the name parameter keeps the helper honest for future scenarios
func (f *fixture) updateNode(t *testing.T, name string, mutate func(node *corev1.Node)) {
	t.Helper()
	g := NewWithT(t)
	node := &corev1.Node{}
	g.ExpectWithOffset(2, f.client.Get(context.Background(),
		client.ObjectKey{Name: name}, node)).To(Succeed())
	mutate(node)
	status := node.Status.DeepCopy()
	g.ExpectWithOffset(2, f.client.Update(context.Background(), node)).To(Succeed())

	fresh := &corev1.Node{}
	g.ExpectWithOffset(2, f.client.Get(context.Background(),
		client.ObjectKey{Name: name}, fresh)).To(Succeed())
	fresh.Status = *status
	g.ExpectWithOffset(2, f.client.Status().Update(context.Background(), fresh)).To(Succeed())
}

// devicesByName returns the cluster's devices as a name-sorted slice,
// so retention is proven by deep-equal state snapshots.
func (f *fixture) devicesByName(t *testing.T) []infrastructurev1alpha1.HardwareDevice {
	t.Helper()
	devices := slices.Clone(f.listDevices(t))
	slices.SortFunc(devices, func(a infrastructurev1alpha1.HardwareDevice, b infrastructurev1alpha1.HardwareDevice) int {
		return strings.Compare(a.Name, b.Name)
	})
	return devices
}

// seedDeviceStatus stamps device-controller-written status on the named
// device, so retention proves the whole object — spec, labels and
// status — is preserved untouched.
func (f *fixture) seedDeviceStatus(t *testing.T, name string, plane infrastructurev1alpha1.AllocationPlane) {
	t.Helper()
	device := f.getDevice(t, name)
	device.Status = infrastructurev1alpha1.HardwareDeviceStatus{
		Accelerator:     &infrastructurev1alpha1.AcceleratorDeviceStatus{Model: modelTesla},
		AllocationPlane: plane,
	}
	NewWithT(t).ExpectWithOffset(2, f.client.Status().Update(context.Background(), device)).To(Succeed())
}

// TestReconcileClassicEvidenceRetention is the regression for
// the classic plane: with the production source factory over a fake
// cluster and the real classic source, losing classic evidence — a
// missing or malformed gpu.count label, or missing extended-resource
// evidence — leaves the node unclaimed, and the source's successful
// empty enumeration of a node it does not claim is loss of evidence,
// never a proven zero: the classic devices are retained untouched. A
// trustworthy classic zero (evidence kept, valid count of zero) claims
// the node and prunes through the ordinary desired-set path.
func TestReconcileClassicEvidenceRetention(t *testing.T) {
	nodeA := classicEvidenceNode(nodeAName, nodeAUID, "2")
	plain := testNode(plainName, plainUID, nil)
	single := testSingleton()

	f := newFixture(t, single, nodeA, plain)
	g := NewWithT(t)
	f.useRealSources(f.inner)

	f.mustReconcile(t)
	device0 := discovery.DeviceName(classicIdentity(nodeAName, nodeAUID, 0).ID)
	device1 := discovery.DeviceName(classicIdentity(nodeAName, nodeAUID, 1).ID)
	g.Expect(f.getDevice(t, device0).Spec).To(Equal(classicDeviceSpec(nodeAName, nodeAUID, 0)))
	g.Expect(f.getDevice(t, device1).Spec).To(Equal(classicDeviceSpec(nodeAName, nodeAUID, 1)))

	// Device status belongs to the device controller; retention must
	// preserve it with the rest of the object.
	f.seedDeviceStatus(t, device0, sourceIDClassic)
	f.seedDeviceStatus(t, device1, sourceIDClassic)
	f.mustReconcile(t)
	retained := f.devicesByName(t)
	g.Expect(retained).To(HaveLen(2))

	retains := func(t *testing.T) {
		t.Helper()
		g := NewWithT(t)
		g.Expect(f.devicesByName(t)).To(Equal(retained),
			"loss of classic evidence retains the inventory untouched")
		g.Expect(f.getDevice(t, device0).Annotations).To(BeEmpty(),
			"a present node is never orphan-annotated")
		updated := f.getSingleton(t)
		g.Expect(updated.Status.NodeSummaries).To(Equal([]infrastructurev1alpha1.NodeSummary{
			{Name: nodeAName, UID: types.UID(nodeAUID), DeviceCount: 2, Outcome: outcomeUnknown},
			{Name: plainName, UID: types.UID(plainUID), DeviceCount: 0, Outcome: outcomeUnknown},
		}), "an unclaimed node reports unknown, never zero")
		ready := condition(g, updated, inventory.ConditionTypeReady)
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue),
			"the benign unknown of an unclaimed node is not degraded")
	}

	restore := func(t *testing.T) {
		t.Helper()
		f.updateNode(t, nodeAName, func(node *corev1.Node) {
			stampClassicEvidence(node, "2")
		})
		f.mustReconcile(t)
	}

	t.Run("missing gpu count retains the classic devices", func(t *testing.T) {
		f.updateNode(t, nodeAName, func(node *corev1.Node) {
			delete(node.Labels, classic.LabelGPUCount)
		})
		f.mustReconcile(t)
		retains(t)
		restore(t)
	})

	t.Run("malformed gpu count retains the classic devices", func(t *testing.T) {
		f.updateNode(t, nodeAName, func(node *corev1.Node) {
			node.Labels[classic.LabelGPUCount] = "two"
		})
		f.mustReconcile(t)
		retains(t)
		restore(t)
	})

	t.Run("missing extended resource evidence retains the classic devices", func(t *testing.T) {
		gpu := corev1.ResourceName(classic.DefaultResourceName)
		f.updateNode(t, nodeAName, func(node *corev1.Node) {
			delete(node.Status.Allocatable, gpu)
			delete(node.Status.Capacity, gpu)
		})
		f.mustReconcile(t)
		retains(t)
		restore(t)
	})

	t.Run("trustworthy zero count prunes through the desired set", func(t *testing.T) {
		g := NewWithT(t)
		f.updateNode(t, nodeAName, func(node *corev1.Node) {
			stampClassicEvidence(node, "0")
		})
		f.mustReconcile(t)

		f.deviceMissing(t, device0)
		f.deviceMissing(t, device1)
		updated := f.getSingleton(t)
		g.Expect(updated.Status.NodeSummaries).To(Equal([]infrastructurev1alpha1.NodeSummary{
			{Name: nodeAName, UID: types.UID(nodeAUID), Plane: sourceIDClassic, DeviceCount: 0, Outcome: outcomeSuccess},
			{Name: plainName, UID: types.UID(plainUID), DeviceCount: 0, Outcome: outcomeUnknown},
		}), "a claim-qualified zero is an authoritative empty snapshot")
		ready := condition(g, updated, inventory.ConditionTypeReady)
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	})
}

// TestReconcileSelectedUnknownRetainsClassic is the regression
// for the mixed plane: when the selected source's evaluation is Unknown —
// a DRA-incomplete pool on a node that also carries classic evidence —
// the classic devices are retained even when the classic source would
// claim the node with zero devices: a selected source's Unknown outcome
// takes precedence over every empty-source exception. Restoring complete
// DRA evidence switches the node's plane and prunes the classic device
// through the ordinary desired-set path.
func TestReconcileSelectedUnknownRetainsClassic(t *testing.T) {
	const (
		gpuUUID        = "GPU-11111111-2222-3333-4444-555555555555"
		incompleteName = "gpu-node-a-incomplete"
		incompletePool = "gpu-node-a-incomplete-pool"
		completeName   = "gpu-node-a-pool"
		completePool   = "gpu-node-a-pool"
	)
	nodeA := classicEvidenceNode(nodeAName, nodeAUID, "1")
	single := testSingleton()

	f := newFixture(t, single, nodeA)
	g := NewWithT(t)
	f.useRealSources(f.inner)

	f.mustReconcile(t)
	classicName := discovery.DeviceName(classicIdentity(nodeAName, nodeAUID, 0).ID)
	g.Expect(f.getDevice(t, classicName).Spec).To(Equal(classicDeviceSpec(nodeAName, nodeAUID, 0)))
	f.seedDeviceStatus(t, classicName, sourceIDClassic)

	// A DRA advertisement appears but its pool snapshot is incomplete:
	// DRA claims the node (advertisements exist, even incomplete ones)
	// and the fold selects DRA, whose Unknown outcome governs.
	incomplete := incompleteSlicePool(incompleteName, nodeAName, incompletePool, gpuUUID, modelTesla)
	g.Expect(f.client.Create(context.Background(), incomplete)).To(Succeed())
	f.mustReconcile(t)
	retained := f.devicesByName(t)
	g.Expect(retained).To(HaveLen(1),
		"no partial DRA inventory is fabricated from an incomplete pool")

	t.Run("selected unknown retains the classic device", func(t *testing.T) {
		g := NewWithT(t)
		f.mustReconcile(t)

		g.Expect(f.devicesByName(t)).To(Equal(retained))
		updated := f.getSingleton(t)
		g.Expect(updated.Status.NodeSummaries).To(Equal([]infrastructurev1alpha1.NodeSummary{
			{Name: nodeAName, UID: types.UID(nodeAUID), Plane: sourceIDDRA, DeviceCount: 1, Outcome: outcomeUnknown},
		}))
		ready := condition(g, updated, inventory.ConditionTypeReady)
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(ready.Reason).To(Equal("PoolIncomplete"))
	})

	t.Run("absent classic count under selected unknown still retains", func(t *testing.T) {
		g := NewWithT(t)
		// The classic source no longer claims the node and enumerates
		// it empty; the selected source's Unknown still governs, so
		// the empty lower-priority enumeration never prunes.
		f.updateNode(t, nodeAName, func(node *corev1.Node) {
			delete(node.Labels, classic.LabelGPUCount)
		})
		f.mustReconcile(t)

		g.Expect(f.devicesByName(t)).To(Equal(retained))
		ready := condition(g, f.getSingleton(t), inventory.ConditionTypeReady)
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(ready.Reason).To(Equal("PoolIncomplete"))

		f.updateNode(t, nodeAName, func(node *corev1.Node) {
			stampClassicEvidence(node, "1")
		})
		f.mustReconcile(t)
	})

	t.Run("classic zero claim under selected unknown still retains", func(t *testing.T) {
		g := NewWithT(t)
		// The classic source now claims the node with zero devices;
		// the selected source's Unknown still governs, so nothing
		// prunes and no classic zero is fabricated.
		f.updateNode(t, nodeAName, func(node *corev1.Node) {
			stampClassicEvidence(node, "0")
		})
		f.mustReconcile(t)

		g.Expect(f.devicesByName(t)).To(Equal(retained))
		ready := condition(g, f.getSingleton(t), inventory.ConditionTypeReady)
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(ready.Reason).To(Equal("PoolIncomplete"))

		f.updateNode(t, nodeAName, func(node *corev1.Node) {
			stampClassicEvidence(node, "1")
		})
		f.mustReconcile(t)
	})

	t.Run("complete DRA evidence switches the plane", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(f.client.Delete(context.Background(), incomplete)).To(Succeed())
		complete := gpuSlice(completeName, nodeAName, completePool, gpuUUID, modelTesla)
		g.Expect(f.client.Create(context.Background(), complete)).To(Succeed())
		f.mustReconcile(t)

		draName := discovery.DeviceName(gpuUUID)
		g.Expect(f.getDevice(t, draName).Spec).To(Equal(draDeviceSpec(gpuUUID, nodeAName, nodeAUID, completePool)))
		f.deviceMissing(t, classicName)
		updated := f.getSingleton(t)
		g.Expect(updated.Status.NodeSummaries).To(Equal([]infrastructurev1alpha1.NodeSummary{
			{Name: nodeAName, UID: types.UID(nodeAUID), Plane: sourceIDDRA, DeviceCount: 1, Outcome: outcomeSuccess},
		}))
		ready := condition(g, updated, inventory.ConditionTypeReady)
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	})
}
