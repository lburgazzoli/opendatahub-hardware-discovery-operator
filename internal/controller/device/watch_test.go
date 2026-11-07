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
	"testing"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
)

const (
	// watchNodeA is the well-known node name of the watch tests.
	watchNodeA = "gpu-node-a"
	// watchNodeB is a second node of the watch tests.
	watchNodeB = "gpu-node-b"
	// watchGPUNames is the extended resource of classic-plane evidence.
	watchGPUNames = "nvidia.com/gpu"
)

// baseNode builds a Node carrying classic-plane evidence.
func baseNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   watchNodeA,
			UID:    "uid-a",
			Labels: map[string]string{"pool": "default"},
		},
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{watchGPUNames: apiresource.MustParse("8")},
			Allocatable: corev1.ResourceList{watchGPUNames: apiresource.MustParse("8")},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// TestNodeEvidencePredicate proves the Node watch passes discovery
// evidence and lifecycle changes while filtering routine status churn,
// so no relevant Node change is dropped and no heartbeat re-observes
// devices.
func TestNodeEvidencePredicate(t *testing.T) {
	g := NewWithT(t)
	predicate := nodeEvidencePredicate()

	cases := []struct {
		name string
		old  func() *corev1.Node
		new  func() *corev1.Node
		pass bool
	}{
		{
			name: "label change passes",
			old:  baseNode,
			new: func() *corev1.Node {
				node := baseNode()
				node.Labels["feature.node.kubernetes.io/nvidia-model"] = "Tesla-K80"
				return node
			},
			pass: true,
		},
		{
			name: "allocatable change passes",
			old:  baseNode,
			new: func() *corev1.Node {
				node := baseNode()
				node.Status.Allocatable = corev1.ResourceList{watchGPUNames: apiresource.MustParse("7")}
				return node
			},
			pass: true,
		},
		{
			name: "capacity change passes",
			old:  baseNode,
			new: func() *corev1.Node {
				node := baseNode()
				node.Status.Capacity = corev1.ResourceList{watchGPUNames: apiresource.MustParse("7")}
				return node
			},
			pass: true,
		},
		{
			name: "deletion timestamp passes",
			old:  baseNode,
			new: func() *corev1.Node {
				node := baseNode()
				node.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
				return node
			},
			pass: true,
		},
		{
			name: "condition churn is filtered",
			old:  baseNode,
			new: func() *corev1.Node {
				node := baseNode()
				node.Status.Conditions[0].LastHeartbeatTime = metav1.Now()
				node.Status.Conditions[0].Message = "kubelet is posting ready"
				return node
			},
			pass: false,
		},
		{
			name: "unchanged node is filtered",
			old:  baseNode,
			new:  baseNode,
			pass: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(predicate.Update(event.UpdateEvent{
				ObjectOld: tc.old(),
				ObjectNew: tc.new(),
			})).To(Equal(tc.pass))
		})
	}

	// Creation, deletion and generic events pass (nil funcs default
	// true): a new node incarnation must reach the devices.
	g.Expect(predicate.Create(event.CreateEvent{Object: baseNode()})).To(BeTrue())
	g.Expect(predicate.Delete(event.DeleteEvent{Object: baseNode()})).To(BeTrue())
	g.Expect(predicate.Generic(event.GenericEvent{Object: baseNode()})).To(BeTrue())
}

// watchScheme builds a scheme with the core and infrastructure types.
func watchScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := infrastructurev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// namedDevice builds a device labeled onto one node.
func namedDevice(name string, node string) *infrastructurev1alpha1.HardwareDevice {
	return &infrastructurev1alpha1.HardwareDevice{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: discovery.DeviceLabels("accelerator", node),
		},
	}
}

// TestAllDevicesMapper proves a singleton configuration change maps to
// every device: class, selector, source enablement and maxStale
// transitions must be re-evaluated everywhere.
func TestAllDevicesMapper(t *testing.T) {
	g := NewWithT(t)
	r := &Reconciler{Client: fake.NewClientBuilder().
		WithScheme(watchScheme(t)).
		WithObjects(namedDevice("gpu-a", watchNodeA), namedDevice("gpu-b", watchNodeB)).
		Build()}

	requests := r.allDevices(context.Background(), &infrastructurev1alpha1.HardwareDiscovery{})
	g.Expect(requests).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Name: "gpu-a"}},
		reconcile.Request{NamespacedName: types.NamespacedName{Name: "gpu-b"}},
	))
}

// TestDevicesOnNodeMapper proves a node evidence change maps to exactly
// the devices hosted on that node, through the service-written node
// label index.
func TestDevicesOnNodeMapper(t *testing.T) {
	g := NewWithT(t)
	r := &Reconciler{Client: fake.NewClientBuilder().
		WithScheme(watchScheme(t)).
		WithObjects(
			namedDevice("gpu-a-0", watchNodeA),
			namedDevice("gpu-a-1", watchNodeA),
			namedDevice("gpu-b-0", watchNodeB),
		).
		Build()}

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: watchNodeA}}
	requests := r.devicesOnNode(context.Background(), node)
	g.Expect(requests).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Name: "gpu-a-0"}},
		reconcile.Request{NamespacedName: types.NamespacedName{Name: "gpu-a-1"}},
	))
}

// TestDevicesOnNodeMapperUnlabelled proves devices without the node
// label (not managed by this service) never match a node event.
func TestDevicesOnNodeMapperUnlabelled(t *testing.T) {
	g := NewWithT(t)
	plain := &infrastructurev1alpha1.HardwareDevice{ObjectMeta: metav1.ObjectMeta{Name: "gpu-plain"}}
	r := &Reconciler{Client: fake.NewClientBuilder().
		WithScheme(watchScheme(t)).
		WithObjects(plain).
		Build()}

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: watchNodeA}}
	g.Expect(r.devicesOnNode(context.Background(), node)).To(BeEmpty())
}
