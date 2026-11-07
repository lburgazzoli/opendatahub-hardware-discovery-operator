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
	"errors"
	"fmt"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/classic"
)

func TestSourceIdentity(t *testing.T) {
	g := NewWithT(t)

	source := newSource(&stubReader{})
	g.Expect(source.ID()).To(Equal("classic"))
	g.Expect(source.Plane()).To(Equal(discovery.PlaneClassic))
}

func TestNewValidation(t *testing.T) {
	g := NewWithT(t)

	_, err := classic.New(nil)
	g.Expect(err).To(HaveOccurred())

	_, err = classic.New(&stubReader{}, classic.WithResourceName(""))
	g.Expect(err).To(HaveOccurred())

	_, err = classic.New(&stubReader{}, classic.WithClock(nil))
	g.Expect(err).To(HaveOccurred())
}

func TestClaimsNode(t *testing.T) {
	cases := []struct {
		name  string
		node  *corev1.Node
		claim bool
	}{
		{
			"evidence plus labels",
			fullGPUNode(),
			true,
		},
		{
			// count=0 is still a claimed node: its authoritative empty
			// snapshot may prune.
			"zero count",
			newNode(nodeName, nodeUID).
				labels(map[string]string{classic.LabelGPUCount: "0"}).
				evidence(corev1.ResourceName(classic.DefaultResourceName)).
				build(),
			true,
		},
		{
			// Extended-resource evidence alone claims nothing: without
			// labels the inventory is unknown, never zero.
			"evidence without labels",
			newNode(nodeName, nodeUID).
				evidence(corev1.ResourceName(classic.DefaultResourceName)).
				build(),
			false,
		},
		{
			"labels without evidence",
			newNode(nodeName, nodeUID).
				labels(map[string]string{classic.LabelGPUCount: "2"}).
				build(),
			false,
		},
		{
			// A malformed count label is not usable evidence: the node is
			// unclaimed rather than zeroed.
			"malformed count",
			newNode(nodeName, nodeUID).
				labels(map[string]string{classic.LabelGPUCount: "two"}).
				evidence(corev1.ResourceName(classic.DefaultResourceName)).
				build(),
			false,
		},
		{
			"negative count",
			newNode(nodeName, nodeUID).
				labels(map[string]string{classic.LabelGPUCount: "-1"}).
				evidence(corev1.ResourceName(classic.DefaultResourceName)).
				build(),
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			source := newSource(&stubReader{nodes: map[string]*corev1.Node{tc.node.Name: tc.node}})
			g.Expect(source.ClaimsNode(context.Background(), nodeInfo())).To(Equal(tc.claim))
		})
	}

	t.Run("missing node", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{nodes: map[string]*corev1.Node{}})
		g.Expect(source.ClaimsNode(context.Background(), nodeInfo())).To(BeFalse())
	})

	t.Run("stale uid", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{nodes: map[string]*corev1.Node{
			nodeName: atUID(fullGPUNode(), otherUID),
		}})
		g.Expect(source.ClaimsNode(context.Background(), nodeInfo())).To(BeFalse())
	})

	t.Run("custom resource name", func(t *testing.T) {
		g := NewWithT(t)
		node := newNode(nodeName, nodeUID).
			labels(map[string]string{classic.LabelGPUCount: "2"}).
			evidence(customGPU).
			build()
		source := newSource(&stubReader{nodes: map[string]*corev1.Node{nodeName: node}},
			classic.WithResourceName(string(customGPU)))
		g.Expect(source.ClaimsNode(context.Background(), nodeInfo())).To(BeTrue())

		// The default resource name does not claim a node that only
		// publishes the custom one.
		defaultSource := newSource(&stubReader{nodes: map[string]*corev1.Node{nodeName: node}})
		g.Expect(defaultSource.ClaimsNode(context.Background(), nodeInfo())).To(BeFalse())
	})
}

func TestEnumerate(t *testing.T) {
	t.Run("count identities with node-scoped ids", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{nodes: fullGPUNodes()})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Validate()).To(Succeed())
		g.Expect(enumeration.Authoritative()).To(BeTrue())

		// Two same-model GPUs with distinct indices: homogeneous facts
		// come from Observe; enumeration only establishes identity.
		g.Expect(enumeration.Identities).To(HaveLen(2))
		for index, identity := range enumeration.Identities {
			g.Expect(identity.ID).To(Equal(fmt.Sprintf("%s-gpu-%d", nodeName, index)))
			g.Expect(identity.Kind).To(Equal(discovery.IdentityKindNodeIndex))
			g.Expect(identity.NodeName).To(Equal(nodeName))
			g.Expect(identity.NodeUID).To(Equal(nodeUID))
			g.Expect(identity.Plane).To(Equal(discovery.PlaneClassic))
			g.Expect(identity.Locator).To(Equal(classic.Locator{Node: nodeName, Index: int32(index)}))
			g.Expect(identity.Validate()).To(Succeed())
		}

		// Deterministic across repeated enumerations of the same node
		// incarnation.
		again, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(again.Identities).To(Equal(enumeration.Identities))
	})

	t.Run("zero count is an authoritative empty snapshot", func(t *testing.T) {
		g := NewWithT(t)
		node := newNode(nodeName, nodeUID).
			labels(map[string]string{classic.LabelGPUCount: "0"}).
			evidence(corev1.ResourceName(classic.DefaultResourceName)).
			build()
		source := newSource(&stubReader{nodes: map[string]*corev1.Node{nodeName: node}})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeTrue())
		g.Expect(enumeration.Identities).To(BeEmpty())
	})

	t.Run("unclaimed nodes are skipped, not zeroed", func(t *testing.T) {
		g := NewWithT(t)
		nodes := map[string]*corev1.Node{
			nodeName:  fullGPUNode(),
			otherNode: newNode(otherNode, otherUID).build(), // no evidence, no labels
		}
		source := newSource(&stubReader{nodes: nodes})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{
			nodeInfo(),
			{Name: otherNode, UID: otherUID},
		})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeTrue())
		g.Expect(enumeration.Identities).To(HaveLen(2))
		for _, identity := range enumeration.Identities {
			g.Expect(identity.NodeName).To(Equal(nodeName))
		}
	})

	t.Run("read failures are unknown, never zero", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{err: apierrors.NewForbidden(nodeGVR, nodeName, errors.New("rbac"))})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeFalse())
		g.Expect(enumeration.Identities).To(BeEmpty())
		g.Expect(enumeration.Err).To(HaveOccurred())
	})

	t.Run("stale uid is unknown", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{nodes: map[string]*corev1.Node{
			nodeName: atUID(fullGPUNode(), otherUID),
		}})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeFalse())
		g.Expect(errors.Is(enumeration.Err, classic.ErrNodeUIDMismatch)).To(BeTrue())
	})

	t.Run("invalid node info is a hard error", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{nodes: fullGPUNodes()})

		_, err := source.Enumerate(context.Background(), []discovery.NodeInfo{{UID: nodeUID}})
		g.Expect(err).To(HaveOccurred())
		g.Expect(errors.Is(err, discovery.ErrMissingNode)).To(BeTrue())
	})

	t.Run("cancelled context is a hard error", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{nodes: fullGPUNodes()})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := source.Enumerate(ctx, []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).To(MatchError(context.Canceled))
	})

	t.Run("long node names derive legal ids", func(t *testing.T) {
		g := NewWithT(t)
		longNode := strings.Repeat("n", 200)
		node := newNode(longNode, nodeUID).
			labels(map[string]string{classic.LabelGPUCount: "3"}).
			evidence(corev1.ResourceName(classic.DefaultResourceName)).
			build()
		source := newSource(&stubReader{nodes: map[string]*corev1.Node{longNode: node}})

		enumeration, err := source.Enumerate(context.Background(),
			[]discovery.NodeInfo{{Name: longNode, UID: nodeUID}})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Identities).To(HaveLen(3))
		seen := map[string]struct{}{}
		for _, identity := range enumeration.Identities {
			g.Expect(len(identity.ID)).To(BeNumerically("<=", classic.MaxIDLength))
			g.Expect(identity.ID).To(HavePrefix("n"))
			seen[identity.ID] = struct{}{}
		}
		g.Expect(seen).To(HaveLen(3))
	})
}

func TestObserve(t *testing.T) {
	t.Run("homogeneous label facts with collector clock", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{nodes: fullGPUNodes()})

		enumeration, _ := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		observation, err := source.Observe(context.Background(), enumeration.Identities[0])
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(observation.Validate()).To(Succeed())
		g.Expect(observation.CollectedAt).To(Equal(fixedNow))

		// The expected memory is computed from the contract's MiB →
		// bytes rule, independently of the implementation.
		expectedBytes := int64(11441) * 1024 * 1024
		expected := resource.NewQuantity(expectedBytes, resource.BinarySI)
		g.Expect(observation.Facts.Accelerator).NotTo(BeNil())
		g.Expect(observation.Facts.Accelerator.Model).To(Equal(teslaK80))
		g.Expect(observation.Facts.Accelerator.Memory).To(Equal(expected))
		// GFD does not publish vendor: omitted, never defaulted.
		g.Expect(observation.Facts.Accelerator.Vendor).To(BeEmpty())
	})

	t.Run("absent facts are omitted, never zero", func(t *testing.T) {
		g := NewWithT(t)
		node := newNode(nodeName, nodeUID).
			labels(map[string]string{classic.LabelGPUCount: "1"}).
			evidence(corev1.ResourceName(classic.DefaultResourceName)).
			build()
		source := newSource(&stubReader{nodes: map[string]*corev1.Node{nodeName: node}})

		observation, err := source.Observe(context.Background(), discovery.Identity{
			ID:       classic.DeviceID(nodeName, 0),
			Kind:     discovery.IdentityKindNodeIndex,
			NodeName: nodeName,
			NodeUID:  nodeUID,
			Plane:    discovery.PlaneClassic,
			Locator:  classic.Locator{Node: nodeName, Index: 0},
		})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(observation.Validate()).To(Succeed())
		g.Expect(observation.Facts.Accelerator).To(BeNil())
	})

	t.Run("model only", func(t *testing.T) {
		g := NewWithT(t)
		node := newNode(nodeName, nodeUID).
			labels(map[string]string{
				classic.LabelGPUProduct: teslaK80,
				classic.LabelGPUCount:   "1",
			}).
			evidence(corev1.ResourceName(classic.DefaultResourceName)).
			build()
		source := newSource(&stubReader{nodes: map[string]*corev1.Node{nodeName: node}})

		observation, err := source.Observe(context.Background(), discovery.Identity{
			ID:       classic.DeviceID(nodeName, 0),
			Kind:     discovery.IdentityKindNodeIndex,
			NodeName: nodeName,
			NodeUID:  nodeUID,
			Plane:    discovery.PlaneClassic,
			Locator:  classic.Locator{Node: nodeName, Index: 0},
		})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(observation.Facts.Accelerator.Model).To(Equal(teslaK80))
		g.Expect(observation.Facts.Accelerator.Memory).To(BeNil())
	})

	t.Run("malformed memory label is unknown, not zero", func(t *testing.T) {
		g := NewWithT(t)
		node := newNode(nodeName, nodeUID).
			labels(map[string]string{
				classic.LabelGPUMemory: "lots",
				classic.LabelGPUCount:  "1",
			}).
			evidence(corev1.ResourceName(classic.DefaultResourceName)).
			build()
		source := newSource(&stubReader{nodes: map[string]*corev1.Node{nodeName: node}})

		_, err := source.Observe(context.Background(), discovery.Identity{
			ID:       classic.DeviceID(nodeName, 0),
			Kind:     discovery.IdentityKindNodeIndex,
			NodeName: nodeName,
			NodeUID:  nodeUID,
			Plane:    discovery.PlaneClassic,
			Locator:  classic.Locator{Node: nodeName, Index: 0},
		})
		g.Expect(errors.Is(err, classic.ErrMalformedLabel)).To(BeTrue())
	})

	t.Run("node uid change never retargets a device", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{nodes: map[string]*corev1.Node{
			nodeName: atUID(fullGPUNode(), otherUID),
		}})

		_, err := source.Observe(context.Background(), discovery.Identity{
			ID:       classic.DeviceID(nodeName, 0),
			Kind:     discovery.IdentityKindNodeIndex,
			NodeName: nodeName,
			NodeUID:  nodeUID,
			Plane:    discovery.PlaneClassic,
			Locator:  classic.Locator{Node: nodeName, Index: 0},
		})
		g.Expect(errors.Is(err, classic.ErrNodeUIDMismatch)).To(BeTrue())
	})

	t.Run("node no longer claimed", func(t *testing.T) {
		g := NewWithT(t)
		node := fullGPUNode()
		node.Status.Allocatable = corev1.ResourceList{}
		node.Status.Capacity = corev1.ResourceList{}
		source := newSource(&stubReader{nodes: map[string]*corev1.Node{nodeName: node}})

		_, err := source.Observe(context.Background(), discovery.Identity{
			ID:       classic.DeviceID(nodeName, 0),
			Kind:     discovery.IdentityKindNodeIndex,
			NodeName: nodeName,
			NodeUID:  nodeUID,
			Plane:    discovery.PlaneClassic,
			Locator:  classic.Locator{Node: nodeName, Index: 0},
		})
		g.Expect(errors.Is(err, classic.ErrUnclaimedNode)).To(BeTrue())
	})

	t.Run("shrinking count makes the index stale", func(t *testing.T) {
		g := NewWithT(t)
		node := newNode(nodeName, nodeUID).
			labels(map[string]string{
				classic.LabelGPUProduct: teslaK80,
				classic.LabelGPUMemory:  gpuMemoryMiB,
				classic.LabelGPUCount:   "1",
			}).
			evidence(corev1.ResourceName(classic.DefaultResourceName)).
			build()
		source := newSource(&stubReader{nodes: map[string]*corev1.Node{nodeName: node}})

		_, err := source.Observe(context.Background(), discovery.Identity{
			ID:       classic.DeviceID(nodeName, 1),
			Kind:     discovery.IdentityKindNodeIndex,
			NodeName: nodeName,
			NodeUID:  nodeUID,
			Plane:    discovery.PlaneClassic,
			Locator:  classic.Locator{Node: nodeName, Index: 1},
		})
		g.Expect(errors.Is(err, classic.ErrIdentityStale)).To(BeTrue())
	})

	t.Run("foreign identity shapes are rejected", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{nodes: fullGPUNodes()})

		uuidKind := discovery.Identity{
			ID:       classic.DeviceID(nodeName, 0),
			Kind:     discovery.IdentityKindUUID,
			NodeName: nodeName,
			NodeUID:  nodeUID,
			Plane:    discovery.PlaneClassic,
			Locator:  classic.Locator{Node: nodeName, Index: 0},
		}
		_, err := source.Observe(context.Background(), uuidKind)
		g.Expect(errors.Is(err, classic.ErrWrongIdentityKind)).To(BeTrue())

		// A locator of another source kind is never interpreted.
		foreignLocator := discovery.Identity{
			ID:       classic.DeviceID(nodeName, 0),
			Kind:     discovery.IdentityKindNodeIndex,
			NodeName: nodeName,
			NodeUID:  nodeUID,
			Plane:    discovery.PlaneClassic,
			Locator:  foreignLocatorFixture{},
		}
		_, err = source.Observe(context.Background(), foreignLocator)
		g.Expect(errors.Is(err, classic.ErrIdentityStale)).To(BeTrue())
	})

	t.Run("missing node", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{nodes: map[string]*corev1.Node{}})

		_, err := source.Observe(context.Background(), discovery.Identity{
			ID:       classic.DeviceID(nodeName, 0),
			Kind:     discovery.IdentityKindNodeIndex,
			NodeName: nodeName,
			NodeUID:  nodeUID,
			Plane:    discovery.PlaneClassic,
			Locator:  classic.Locator{Node: nodeName, Index: 0},
		})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})
}

// foreignLocatorFixture is a locator of another source kind.
type foreignLocatorFixture struct{}

func (foreignLocatorFixture) Kind() string    { return draSourceID }
func (foreignLocatorFixture) Validate() error { return nil }
func (foreignLocatorFixture) Equal(other discovery.Locator) bool {
	return other == foreignLocatorFixture{}
}
func (foreignLocatorFixture) Ref() string { return "gpu.nvidia.com/pool" }

// atUID returns a copy of the node under a different UID.
func atUID(node *corev1.Node, uid types.UID) *corev1.Node {
	changed := *node
	changed.UID = uid
	return &changed
}
