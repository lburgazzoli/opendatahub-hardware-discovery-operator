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
	"testing"
	"time"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
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

// TestNodePredicate proves the Node watch passes discovery evidence and
// lifecycle changes while filtering routine status churn, so no relevant
// Node change is dropped and no heartbeat triggers a scan.
func TestNodePredicate(t *testing.T) {
	predicate := nodePredicate()

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
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			got := predicate.Update(event.UpdateEvent{ObjectOld: tc.old(), ObjectNew: tc.new()})
			g.Expect(got).To(Equal(tc.pass))
		})
	}
}

// TestNodePredicateNonNodeUpdates proves a non-Node update event (a
// misconfigured watch) is dropped rather than crashing the predicate.
func TestNodePredicateNonNodeUpdates(t *testing.T) {
	g := NewWithT(t)
	predicate := nodePredicate()

	got := predicate.Update(event.UpdateEvent{
		ObjectOld: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm"}},
		ObjectNew: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm"}},
	})
	g.Expect(got).To(BeFalse())
}

// TestNodePredicateLifecycleEvents proves create and delete events pass
// by default: a new node must be scanned and a deleted one re-evaluated.
func TestNodePredicateLifecycleEvents(t *testing.T) {
	g := NewWithT(t)
	predicate := nodePredicate()

	g.Expect(predicate.Create(event.CreateEvent{Object: baseNode()})).To(BeTrue())
	g.Expect(predicate.Delete(event.DeleteEvent{Object: baseNode()})).To(BeTrue())
}
