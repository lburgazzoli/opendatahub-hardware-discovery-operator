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
	corev1 "k8s.io/api/core/v1"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	fwresources "github.com/opendatahub-io/odh-platform-utilities/framework/resources"
)

// nodeEvidencePredicate passes Node events that can change a device
// observation: creation, deletion, and updates to the labels (node
// selection and the GFD-style facts of classic devices), the extended
// resources in status.allocatable/capacity (classic-plane evidence), or
// the deletion timestamp. Routine Node status churn — conditions,
// heartbeats — is filtered so observation requeues stay bounded. There
// is deliberately no generation predicate: Node updates do not bump a
// spec generation. The filtering mirrors the inventory controller's
// Node watch: node changes map to the singleton and
// affected devices.
func nodeEvidencePredicate(scheme *runtime.Scheme) predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNode, oldOK := asNode(scheme, e.ObjectOld)
			newNode, newOK := asNode(scheme, e.ObjectNew)
			if !oldOK || !newOK {
				return false
			}
			return nodeEvidenceChanged(oldNode, newNode)
		},
	}
}

// asNode converts a watched object to a typed Node. The Node watch is
// registered as unstructured — the framework client's cache
// representation — so the common path decodes an unstructured object with
// the core Node kind; a typed Node is accepted unchanged. Anything else,
// or an undecodable object, is not a Node event for this controller. The
// manager's scheme drives the framework conversion helper, which also
// re-checks that the scheme knows the core Node kind.
func asNode(scheme *runtime.Scheme, obj client.Object) (*corev1.Node, bool) {
	switch typed := obj.(type) {
	case *corev1.Node:
		return typed, true
	case *unstructured.Unstructured:
		gvk := typed.GroupVersionKind()
		if gvk.Group != "" || gvk.Kind != "Node" {
			return nil, false
		}
		node := &corev1.Node{}
		if err := fwresources.ObjectFromUnstructured(scheme, typed, node); err != nil {
			return nil, false
		}
		return node, true
	default:
		return nil, false
	}
}

// nodeEvidenceChanged reports whether a Node update touches discovery
// evidence or lifecycle.
func nodeEvidenceChanged(oldNode *corev1.Node, newNode *corev1.Node) bool {
	if !apiequality.Semantic.DeepEqual(oldNode.Labels, newNode.Labels) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(oldNode.Status.Allocatable, newNode.Status.Allocatable) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(oldNode.Status.Capacity, newNode.Status.Capacity) {
		return true
	}
	return newNode.GetDeletionTimestamp() != nil
}
