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
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/classic"
)

func TestLocatorContract(t *testing.T) {
	g := NewWithT(t)

	locator := classic.Locator{Node: nodeName, Index: 1}
	g.Expect(locator.Kind()).To(Equal("classic"))
	g.Expect(locator.Ref()).To(Equal(nodeName))
	g.Expect(locator.Validate()).To(Succeed())
	g.Expect(locator.DeviceSource()).To(Equal(locator.DeviceSource())) // deterministic

	// Incomplete locators wrap the core sentinel.
	empty := classic.Locator{}
	g.Expect(errors.Is(empty.Validate(), discovery.ErrIncompleteLocator)).To(BeTrue())
	negative := classic.Locator{Node: nodeName, Index: -1}
	g.Expect(errors.Is(negative.Validate(), discovery.ErrIncompleteLocator)).To(BeTrue())

	// Equal is deep within the kind and false across kinds.
	g.Expect(locator.Equal(classic.Locator{Node: nodeName, Index: 1})).To(BeTrue())
	g.Expect(locator.Equal(classic.Locator{Node: nodeName, Index: 0})).To(BeFalse())
	g.Expect(locator.Equal(classic.Locator{Node: otherNode, Index: 1})).To(BeFalse())
	g.Expect(locator.Equal(foreignLocatorFixture{})).To(BeFalse())

	// The provenance reference is "<source>/<node>".
	ref, err := discovery.SourceRef(classic.SourceID, locator)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ref).To(Equal("classic/" + nodeName))

	_, err = discovery.SourceRef(classic.SourceID, empty)
	g.Expect(errors.Is(err, discovery.ErrIncompleteLocator)).To(BeTrue())
}

func TestDeviceID(t *testing.T) {
	g := NewWithT(t)

	g.Expect(classic.DeviceID(nodeName, 0)).To(Equal(nodeName + "-gpu-0"))
	g.Expect(classic.DeviceID(nodeName, 7)).To(Equal(nodeName + "-gpu-7"))

	// Deterministic for the same inputs and distinct per index.
	g.Expect(classic.DeviceID(nodeName, 0)).To(Equal(classic.DeviceID(nodeName, 0)))
	g.Expect(classic.DeviceID(nodeName, 0)).NotTo(Equal(classic.DeviceID(nodeName, 1)))
	g.Expect(classic.DeviceID(nodeName, 0)).NotTo(Equal(classic.DeviceID(otherNode, 0)))

	// Node names are DNS subdomains up to 253 chars: the id derivation
	// hash-truncates to stay within the 63-char identity id limit, and
	// the truncation stays deterministic and index-distinct.
	longNode := strings.Repeat("n", 253)
	for _, index := range []int32{0, 1, 41} {
		id := classic.DeviceID(longNode, index)
		g.Expect(len(id)).To(BeNumerically("<=", classic.MaxIDLength), "index %d: %q", index, id)
		g.Expect(id).To(Equal(classic.DeviceID(longNode, index)))
	}
	g.Expect(classic.DeviceID(longNode, 0)).NotTo(Equal(classic.DeviceID(longNode, 1)))

	// Names just under the limit stay in the plain form.
	fits := strings.Repeat("n", 63-len("-gpu-0"))
	g.Expect(classic.DeviceID(fits, 0)).To(Equal(fits + "-gpu-0"))
	over := strings.Repeat("n", 63-len("-gpu-0")+1)
	g.Expect(len(classic.DeviceID(over, 0))).To(BeNumerically("<=", classic.MaxIDLength))
}

func TestCodec(t *testing.T) {
	g := NewWithT(t)

	locator := classic.Locator{Node: nodeName, Index: 2}
	src := locator.DeviceSource()
	g.Expect(string(src.Kind)).To(Equal("classic"))
	g.Expect(src.Classic).NotTo(BeNil())
	g.Expect(src.Classic.Node).To(Equal(nodeName))
	g.Expect(src.Classic.Index).To(Equal(int32(2)))

	roundTrip, err := classic.LocatorFromDeviceSource(src)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(roundTrip).To(Equal(locator))

	// Foreign kinds and invalid arms are rejected, never guessed.
	_, err = classic.LocatorFromDeviceSource(v1alpha1.DeviceSource{
		Kind: draSourceID,
		DRA:  &v1alpha1.DRASource{Driver: "gpu.nvidia.com", Pool: "pool", Device: "device"},
	})
	g.Expect(err).To(HaveOccurred())

	_, err = classic.LocatorFromDeviceSource(classic.Locator{}.DeviceSource())
	g.Expect(errors.Is(err, discovery.ErrIncompleteLocator)).To(BeTrue())
}

// TestSelectNodeFold proves the DRA dominance boundary through the
// registry fold: the classic source itself never
// inspects DRA input — priority is decided by SelectNode.
func TestSelectNodeFold(t *testing.T) {
	draClaims := &claimFake{id: draSourceID, plane: discovery.PlaneDRA, claims: true}
	successful := discovery.Enumeration{Outcome: discovery.OutcomeSuccess}

	t.Run("classic decides when dra does not claim", func(t *testing.T) {
		g := NewWithT(t)
		classicSource := newSource(&stubReader{nodes: fullGPUNodes()})
		sources, err := discovery.NewSources(
			&claimFake{id: draSourceID, plane: discovery.PlaneDRA, claims: false},
			classicSource,
		)
		g.Expect(err).NotTo(HaveOccurred())

		enumeration, err := classicSource.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		selection := discovery.SelectNode(context.Background(), nodeInfo(), sources,
			map[string]discovery.Enumeration{"classic": enumeration})
		g.Expect(selection.SourceID).To(Equal("classic"))
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeSuccess))
		g.Expect(selection.Identities).To(HaveLen(2))
	})

	t.Run("dra claims win the fold", func(t *testing.T) {
		g := NewWithT(t)
		classicSource := newSource(&stubReader{nodes: fullGPUNodes()})
		sources, err := discovery.NewSources(draClaims, classicSource)
		g.Expect(err).NotTo(HaveOccurred())

		enumeration, err := classicSource.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		selection := discovery.SelectNode(context.Background(), nodeInfo(), sources,
			map[string]discovery.Enumeration{
				draSourceID: successful,
				"classic":   enumeration,
			})
		g.Expect(selection.SourceID).To(Equal(draSourceID))
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeSuccess))
		g.Expect(selection.Identities).To(BeEmpty())
	})
}

// claimFake is a minimal foreign source for the fold test.
type claimFake struct {
	id     string
	plane  discovery.Plane
	claims bool
}

func (s *claimFake) ID() string             { return s.id }
func (s *claimFake) Plane() discovery.Plane { return s.plane }

func (s *claimFake) ClaimsNode(_ context.Context, _ discovery.NodeInfo) bool {
	return s.claims
}

func (s *claimFake) Enumerate(
	_ context.Context,
	_ []discovery.NodeInfo,
) (discovery.Enumeration, error) {
	return discovery.Enumeration{}, nil
}

func (s *claimFake) Observe(
	_ context.Context,
	_ discovery.Identity,
) (discovery.Observation, error) {
	return discovery.Observation{}, nil
}
