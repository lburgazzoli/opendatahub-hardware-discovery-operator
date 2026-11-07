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

package classic_test

import (
	"context"
	"fmt"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/classic"
)

// Named fixtures shared by the classic source tests. Expected values are
// computed here from the contract, independent of the implementation.

const (
	nodeName     = "gpu-node-a"
	otherNode    = "gpu-node-b"
	draSourceID  = "dra"
	teslaK80     = "Tesla-K80"
	gpuMemoryMiB = "11441"
)

var (
	nodeUID   = types.UID("11111111-1111-1111-1111-111111111111")
	otherUID  = types.UID("22222222-2222-2222-2222-222222222222")
	fixedNow  = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	nodeGVR   = schema.GroupResource{Group: "", Resource: "nodes"}
	customGPU = corev1.ResourceName("example.com/gpu")
)

// stubReader is an in-memory client.Reader serving Nodes by name. It
// proves the source reads only Nodes through the injected reader; List is
// deliberately unsupported so any use fails the test.
type stubReader struct {
	nodes map[string]*corev1.Node
	err   error
}

func (r *stubReader) Get(
	_ context.Context,
	key client.ObjectKey,
	obj client.Object,
	_ ...client.GetOption,
) error {
	if r.err != nil {
		return r.err
	}
	node, ok := r.nodes[key.Name]
	if !ok {
		return apierrors.NewNotFound(nodeGVR, key.Name)
	}
	target, ok := obj.(*corev1.Node)
	if !ok {
		return fmt.Errorf("stub reader: unexpected object type %T", obj)
	}
	*target = *node
	return nil
}

func (r *stubReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return fmt.Errorf("stub reader: List must not be used by the classic source")
}

// nodeBuilder assembles Node fixtures: GFD-style labels plus, optionally,
// classic extended-resource evidence.
type nodeBuilder struct {
	node *corev1.Node
}

func newNode(name string, uid types.UID) *nodeBuilder {
	return &nodeBuilder{node: &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid, Labels: map[string]string{}},
	}}
}

func (b *nodeBuilder) labels(labels map[string]string) *nodeBuilder {
	maps.Copy(b.node.Labels, labels)
	return b
}

func (b *nodeBuilder) evidence(name corev1.ResourceName) *nodeBuilder {
	gpus := resource.MustParse("2")
	b.node.Status.Allocatable = corev1.ResourceList{name: gpus}
	b.node.Status.Capacity = corev1.ResourceList{name: gpus}
	return b
}

func (b *nodeBuilder) build() *corev1.Node {
	return b.node
}

// fullGPUNode is a fully claimed classic node: nvidia.com/gpu evidence
// plus complete GFD-style labels.
func fullGPUNode() *corev1.Node {
	return newNode(nodeName, nodeUID).
		labels(map[string]string{
			classic.LabelGPUProduct: teslaK80,
			classic.LabelGPUMemory:  gpuMemoryMiB,
			classic.LabelGPUCount:   "2",
		}).
		evidence(corev1.ResourceName(classic.DefaultResourceName)).
		build()
}

// fullGPUNodes returns the standard single-node fixture map plus a second
// node with a different UID for incarnation tests.
func fullGPUNodes() map[string]*corev1.Node {
	return map[string]*corev1.Node{nodeName: fullGPUNode()}
}

// newSource builds the source over a reader with the fixed test clock.
func newSource(reader client.Reader, opts ...classic.Option) *classic.Source {
	opts = append([]classic.Option{classic.WithClock(func() time.Time { return fixedNow })}, opts...)
	source, err := classic.New(reader, opts...)
	if err != nil {
		panic(fmt.Sprintf("fixture source: %v", err))
	}
	return source
}

// nodeInfo is the NodeInfo of the standard fixture node.
func nodeInfo() discovery.NodeInfo {
	return discovery.NodeInfo{Name: nodeName, UID: nodeUID}
}
