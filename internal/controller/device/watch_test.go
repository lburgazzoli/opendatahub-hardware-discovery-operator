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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"

	fwclient "github.com/opendatahub-io/odh-platform-utilities/framework/client"
	fwresources "github.com/opendatahub-io/odh-platform-utilities/framework/resources"
)

const (
	// watchNodeA is the well-known node name of the watch tests.
	watchNodeA = "gpu-node-a"
	// watchNodeB is a second node of the watch tests.
	watchNodeB = "gpu-node-b"
	// watchGPUNames is the extended resource of classic-plane evidence.
	watchGPUNames = "nvidia.com/gpu"
	// watchGFDModel is a GFD-style node label of classic devices.
	watchGFDModel = "feature.node.kubernetes.io/nvidia-model"
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

// unstructuredOf converts a typed object into the unstructured
// representation its watch delivers, carrying the given GVK.
func unstructuredOf(t *testing.T, gvk schema.GroupVersionKind, obj runtime.Object) *unstructured.Unstructured {
	t.Helper()
	u, err := fwresources.ToUnstructured(obj)
	if err != nil {
		t.Fatalf("fixture: converting %T to unstructured: %v", obj, err)
	}
	u.SetGroupVersionKind(gvk)
	return u
}

// unstructuredNode converts a typed Node into the representation the
// Node watch delivers.
func unstructuredNode(t *testing.T, node *corev1.Node) *unstructured.Unstructured {
	t.Helper()
	return unstructuredOf(t, schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Node"}, node)
}

// watchScheme builds a scheme with the core and infrastructure types.
func watchScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(infrastructurev1alpha1.AddToScheme(scheme))
	return scheme
}

// mapperClient builds the framework-coherent client the production
// mappers list through, over a fake cluster seeded with the objects.
func mapperClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fwclient.New(fake.NewClientBuilder().
		WithScheme(watchScheme(t)).
		WithObjects(objs...).
		Build())
}

// TestNodeEvidencePredicate proves the Node watch passes discovery
// evidence and lifecycle changes while filtering routine status churn,
// so no relevant Node change is dropped and no heartbeat re-observes
// devices. Every case is exercised through both the typed and the
// unstructured watch representation.
func TestNodeEvidencePredicate(t *testing.T) {
	nodeFilter := nodeEvidencePredicate(watchScheme(t))

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
				node.Labels[watchGFDModel] = "Tesla-K80"
				return node
			},
			pass: true,
		},
		{
			name: "selector label change passes",
			old:  baseNode,
			new: func() *corev1.Node {
				node := baseNode()
				node.Labels["pool"] = "other"
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
				now := metav1.Now()
				node.DeletionTimestamp = &now
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
		for _, representation := range []string{"typed", "unstructured"} {
			t.Run(tc.name+"/"+representation, func(t *testing.T) {
				g := NewWithT(t)
				var oldObj client.Object = tc.old()
				var newObj client.Object = tc.new()
				if representation == "unstructured" {
					oldObj = unstructuredNode(t, tc.old())
					newObj = unstructuredNode(t, tc.new())
				}
				g.Expect(nodeFilter.Update(event.UpdateEvent{
					ObjectOld: oldObj,
					ObjectNew: newObj,
				})).To(Equal(tc.pass))
			})
		}
	}
}

// TestNodeEvidencePredicateNonNodeUpdates proves a non-Node update event
// (a misconfigured watch) is dropped rather than crashing the predicate,
// including unstructured objects of a foreign kind.
func TestNodeEvidencePredicateNonNodeUpdates(t *testing.T) {
	g := NewWithT(t)
	nodeFilter := nodeEvidencePredicate(watchScheme(t))

	g.Expect(nodeFilter.Update(event.UpdateEvent{
		ObjectOld: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm"}},
		ObjectNew: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm"}},
	})).To(BeFalse())

	foreignGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	g.Expect(nodeFilter.Update(event.UpdateEvent{
		ObjectOld: unstructuredOf(t, foreignGVK, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm"}}),
		ObjectNew: unstructuredOf(t, foreignGVK, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm"}}),
	})).To(BeFalse())
}

// TestNodeEvidencePredicateLifecycleEvents proves create and delete
// events pass by default: a new node incarnation must reach the devices
// and a deleted one re-evaluate them.
func TestNodeEvidencePredicateLifecycleEvents(t *testing.T) {
	g := NewWithT(t)
	nodeFilter := nodeEvidencePredicate(watchScheme(t))

	g.Expect(nodeFilter.Create(event.CreateEvent{Object: baseNode()})).To(BeTrue())
	g.Expect(nodeFilter.Delete(event.DeleteEvent{Object: baseNode()})).To(BeTrue())
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
	mapper := allDevices(mapperClient(t,
		namedDevice("gpu-a", watchNodeA),
		namedDevice("gpu-b", watchNodeB),
	))

	requests := mapper(context.Background(), &infrastructurev1alpha1.HardwareDiscovery{})
	g.Expect(requests).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Name: "gpu-a"}},
		reconcile.Request{NamespacedName: types.NamespacedName{Name: "gpu-b"}},
	))
}

// TestDevicesOnNodeMapper proves a node evidence change maps to exactly
// the devices hosted on that node, through the service-written node
// label index. The watched object is the unstructured cache
// representation, but only its name may be read.
func TestDevicesOnNodeMapper(t *testing.T) {
	g := NewWithT(t)
	mapper := devicesOnNode(mapperClient(t,
		namedDevice("gpu-a-0", watchNodeA),
		namedDevice("gpu-a-1", watchNodeA),
		namedDevice("gpu-b-0", watchNodeB),
	))

	requests := mapper(context.Background(), unstructuredNode(t, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: watchNodeA},
	}))
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
	mapper := devicesOnNode(mapperClient(t, plain))

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: watchNodeA}}
	g.Expect(mapper(context.Background(), node)).To(BeEmpty())
}

// TestMappersListFailure proves a failed device list — an unavailable
// cache — maps to no requests instead of panicking: the watch is
// dropped, and the next event or reconcile recovers.
func TestMappersListFailure(t *testing.T) {
	g := NewWithT(t)
	failing := interceptor.NewClient(fake.NewClientBuilder().
		WithScheme(watchScheme(t)).
		WithObjects(namedDevice("gpu-a", watchNodeA)).
		Build(), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return apierrors.NewServerTimeout(schema.GroupResource{
				Group:    "infrastructure.platform.opendatahub.io",
				Resource: "hardwaredevices",
			}, "list", 1)
		},
	})

	single := allDevices(failing)
	g.Expect(single(context.Background(), &infrastructurev1alpha1.HardwareDiscovery{})).To(BeEmpty())

	node := devicesOnNode(failing)
	g.Expect(node(context.Background(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: watchNodeA},
	})).To(BeEmpty())
}
