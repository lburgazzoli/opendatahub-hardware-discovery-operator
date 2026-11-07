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
	"testing"
	"time"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
)

// baseNode builds a Node carrying classic-plane evidence.
func baseNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gpu-node-a",
			UID:    "uid-a",
			Labels: map[string]string{"pool": "default"},
		},
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{"nvidia.com/gpu": apiresource.MustParse("8")},
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": apiresource.MustParse("8")},
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
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		t.Fatalf("fixture: converting %T to unstructured: %v", obj, err)
	}
	u := &unstructured.Unstructured{Object: content}
	u.SetGroupVersionKind(gvk)
	return u
}

// unstructuredNode converts a typed Node into the representation the
// Node watch delivers.
func unstructuredNode(t *testing.T, node *corev1.Node) *unstructured.Unstructured {
	t.Helper()
	return unstructuredOf(t, schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Node"}, node)
}

// TestNodePredicate proves the Node watch passes discovery evidence and
// lifecycle changes while filtering routine status churn, so no relevant
// Node change is dropped and no heartbeat triggers a scan. Every case is
// exercised through both the typed and the unstructured watch
// representation.
func TestNodePredicate(t *testing.T) {
	nodeFilter := nodePredicate()

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
				node.Labels["pool"] = "other"
				return node
			},
			pass: true,
		},
		{
			name: "label removal passes",
			old:  baseNode,
			new: func() *corev1.Node {
				node := baseNode()
				delete(node.Labels, "pool")
				return node
			},
			pass: true,
		},
		{
			name: "allocatable change passes",
			old:  baseNode,
			new: func() *corev1.Node {
				node := baseNode()
				node.Status.Allocatable["nvidia.com/gpu"] = apiresource.MustParse("7")
				return node
			},
			pass: true,
		},
		{
			name: "capacity change passes",
			old:  baseNode,
			new: func() *corev1.Node {
				node := baseNode()
				node.Status.Capacity["nvidia.com/gpu"] = apiresource.MustParse("7")
				return node
			},
			pass: true,
		},
		{
			name: "extended resource evidence appearing passes",
			old: func() *corev1.Node {
				node := baseNode()
				node.Status.Capacity = nil
				node.Status.Allocatable = nil
				return node
			},
			new:  baseNode,
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
				node.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(time.Now())
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
				var oldObj, newObj client.Object = tc.old(), tc.new()
				if representation == "unstructured" {
					oldObj = unstructuredNode(t, tc.old())
					newObj = unstructuredNode(t, tc.new())
				}
				got := nodeFilter.Update(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj})
				g.Expect(got).To(Equal(tc.pass))
			})
		}
	}
}

// TestNodePredicateNonNodeUpdates proves a non-Node update event (a
// misconfigured watch) is dropped rather than crashing the predicate,
// including unstructured objects of a foreign kind.
func TestNodePredicateNonNodeUpdates(t *testing.T) {
	g := NewWithT(t)
	nodeFilter := nodePredicate()

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

// TestNodePredicateLifecycleEvents proves create and delete events pass
// by default: a new node must be scanned and a deleted one re-evaluated.
func TestNodePredicateLifecycleEvents(t *testing.T) {
	g := NewWithT(t)
	nodeFilter := nodePredicate()

	g.Expect(nodeFilter.Create(event.CreateEvent{Object: baseNode()})).To(BeTrue())
	g.Expect(nodeFilter.Delete(event.DeleteEvent{Object: baseNode()})).To(BeTrue())
}

// TestNewWatchSet proves every registration watches its GVK as an
// unstructured object (the framework client's cache representation),
// maps child and Node events to the singleton request, and filters
// status-only churn through generation predicates.
func TestNewWatchSet(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(infrastructurev1alpha1.AddToScheme(scheme))

	watches, err := newWatchSet(scheme)
	g.Expect(err).NotTo(HaveOccurred())

	singletonGVK := schema.GroupVersionKind{
		Group: "infrastructure.platform.opendatahub.io", Version: "v1alpha1", Kind: "HardwareDiscovery",
	}
	deviceGVK := schema.GroupVersionKind{
		Group: "infrastructure.platform.opendatahub.io", Version: "v1alpha1", Kind: "HardwareDevice",
	}
	nodeGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Node"}

	registrations := []struct {
		name         string
		registration watchRegistration
		gvk          schema.GroupVersionKind
	}{
		{"singleton", watches.singleton, singletonGVK},
		{"devices", watches.devices, deviceGVK},
		{"nodes", watches.nodes, nodeGVK},
	}

	for _, registration := range registrations {
		t.Run(registration.name, func(t *testing.T) {
			g := NewWithT(t)
			u, isUnstructured := registration.registration.object.(*unstructured.Unstructured)
			g.Expect(isUnstructured).To(BeTrue(),
				"%s watch must use the unstructured cache representation", registration.name)
			g.Expect(u.GroupVersionKind()).To(Equal(registration.gvk))
		})

		// Mapped watches (children and Nodes) enqueue exactly one
		// request: the singleton.
		if registration.registration.handler == nil {
			continue
		}
		t.Run(registration.name+"/handler", func(t *testing.T) {
			g := NewWithT(t)
			var watched client.Object
			if registration.gvk == nodeGVK {
				watched = unstructuredNode(t, baseNode())
			} else {
				device := &unstructured.Unstructured{}
				device.SetGroupVersionKind(registration.gvk)
				device.SetName("gpu-abc123")
				watched = device
			}
			queue := workqueue.NewTypedRateLimitingQueue[reconcile.Request](
				workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
			registration.registration.handler.Create(context.Background(), event.CreateEvent{Object: watched}, queue)
			g.Expect(queue.Len()).To(Equal(1))
			item, _ := queue.Get()
			g.Expect(item).To(Equal(reconcile.Request{
				NamespacedName: types.NamespacedName{Name: SingletonName},
			}))
		})

		// Generation-predicated watches (the singleton and its children)
		// pass spec changes and filter status-only churn. The Node watch
		// has its own evidence predicate, covered above.
		if _, isGeneration := registration.registration.predicate.(predicate.GenerationChangedPredicate); !isGeneration {
			continue
		}
		t.Run(registration.name+"/generation", func(t *testing.T) {
			g := NewWithT(t)
			gvk := registration.registration.object.GetObjectKind().GroupVersionKind()

			stale := &unstructured.Unstructured{}
			stale.SetGroupVersionKind(gvk)
			stale.SetName("watched")
			stale.SetGeneration(1)

			fresh := stale.DeepCopy()
			fresh.SetGeneration(2)

			g.Expect(registration.registration.predicate.Update(event.UpdateEvent{
				ObjectOld: stale, ObjectNew: fresh,
			})).To(BeTrue(), "a spec change must trigger a scan")

			statusOnly := stale.DeepCopy()
			statusOnly.Object["status"] = map[string]any{"observed": true}
			g.Expect(registration.registration.predicate.Update(event.UpdateEvent{
				ObjectOld: stale, ObjectNew: statusOnly,
			})).To(BeFalse(), "a status-only change must not trigger a scan")
		})
	}
}
