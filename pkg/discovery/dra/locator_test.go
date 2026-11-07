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

package dra_test

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	resourcev1 "k8s.io/api/resource/v1"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/dra"
)

func TestLocatorContract(t *testing.T) {
	g := NewWithT(t)

	locator := dra.Locator{Driver: nvidiaDriver, Pool: poolName, Device: gpuDeviceName()}
	g.Expect(locator.Kind()).To(Equal(draSourceID))
	g.Expect(locator.Ref()).To(Equal(nvidiaDriver + "/" + poolName))
	g.Expect(locator.Validate()).To(Succeed())

	// Incomplete locators wrap the core sentinel.
	empty := dra.Locator{}
	g.Expect(errors.Is(empty.Validate(), discovery.ErrIncompleteLocator)).To(BeTrue())
	partial := dra.Locator{Driver: nvidiaDriver, Pool: poolName}
	g.Expect(errors.Is(partial.Validate(), discovery.ErrIncompleteLocator)).To(BeTrue())

	// Equal is deep within the kind and false across kinds.
	g.Expect(locator.Equal(dra.Locator{Driver: nvidiaDriver, Pool: poolName, Device: gpuDeviceName()})).To(BeTrue())
	g.Expect(locator.Equal(dra.Locator{Driver: nvidiaDriver, Pool: poolName, Device: "other"})).To(BeFalse())
	g.Expect(locator.Equal(dra.Locator{Driver: foreignDriver, Pool: poolName, Device: gpuDeviceName()})).To(BeFalse())
	g.Expect(locator.Equal(foreignLocatorFixture{})).To(BeFalse())

	// The provenance reference is "<source>/<driver>/<pool>".
	ref, err := discovery.SourceRef(dra.SourceID, locator)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ref).To(Equal(draSourceID + "/" + nvidiaDriver + "/" + poolName))

	_, err = discovery.SourceRef(dra.SourceID, empty)
	g.Expect(errors.Is(err, discovery.ErrIncompleteLocator)).To(BeTrue())
}

func TestCodec(t *testing.T) {
	g := NewWithT(t)

	locator := dra.Locator{Driver: nvidiaDriver, Pool: poolName, Device: gpuDeviceName()}
	src := locator.DeviceSource()
	g.Expect(src.Kind).To(Equal(draSourceID))
	g.Expect(src.DRA).NotTo(BeNil())
	g.Expect(src.DRA.Driver).To(Equal(nvidiaDriver))
	g.Expect(src.DRA.Pool).To(Equal(poolName))
	g.Expect(src.DRA.Device).To(Equal(gpuDeviceName()))

	roundTrip, err := dra.LocatorFromDeviceSource(src)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(roundTrip).To(Equal(locator))

	// Foreign kinds are rejected, never guessed.
	_, err = dra.LocatorFromDeviceSource(v1alpha1.DeviceSource{
		Kind:    classicSourceID,
		Classic: &v1alpha1.ClassicSource{Node: nodeName, Index: 0},
	})
	g.Expect(err).To(HaveOccurred())

	// Invalid arms are rejected.
	_, err = dra.LocatorFromDeviceSource(dra.Locator{}.DeviceSource())
	g.Expect(errors.Is(err, discovery.ErrIncompleteLocator)).To(BeTrue())
}

// TestSelectNodeFold proves the DRA dominance boundary through the
// registry fold (design doc §8.2): the DRA source itself never inspects
// classic input — priority is decided by SelectNode.
func TestSelectNodeFold(t *testing.T) {
	t.Run("dra claims win the fold", func(t *testing.T) {
		g := NewWithT(t)
		draSource := newSource(&stubReader{slices: []resourcev1.ResourceSlice{standardSlice()}})
		sources, err := discovery.NewSources(draSource, &claimFake{
			id:     classicSourceID,
			plane:  discovery.PlaneClassic,
			claims: true,
		})
		g.Expect(err).NotTo(HaveOccurred())

		enumeration, err := draSource.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		selection := discovery.SelectNode(context.Background(), nodeInfo(), sources,
			map[string]discovery.Enumeration{
				draSourceID:     enumeration,
				classicSourceID: {Outcome: discovery.OutcomeSuccess},
			})
		g.Expect(selection.SourceID).To(Equal(draSourceID))
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeSuccess))
		g.Expect(selection.Identities).To(Equal([]discovery.Identity{standardIdentity()}))
	})

	t.Run("dra-incomplete nodes suppress classic fallback", func(t *testing.T) {
		g := NewWithT(t)
		incomplete := newSlice(nvidiaDriver, poolName, nodeName, 1, 2).
			withDevice(gpuDeviceName(), gpuUUID, teslaK80).
			build()
		draSource := newSource(&stubReader{slices: []resourcev1.ResourceSlice{incomplete}})
		sources, err := discovery.NewSources(draSource, &claimFake{
			id:     classicSourceID,
			plane:  discovery.PlaneClassic,
			claims: true,
		})
		g.Expect(err).NotTo(HaveOccurred())

		enumeration, err := draSource.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeFalse())

		selection := discovery.SelectNode(context.Background(), nodeInfo(), sources,
			map[string]discovery.Enumeration{
				draSourceID:     enumeration,
				classicSourceID: {Outcome: discovery.OutcomeSuccess},
			})
		g.Expect(selection.SourceID).To(Equal(draSourceID))
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeUnknown))
		g.Expect(selection.Identities).To(BeEmpty())
	})

	t.Run("classic decides when dra does not claim", func(t *testing.T) {
		g := NewWithT(t)
		draSource := newSource(&stubReader{})
		sources, err := discovery.NewSources(draSource, &claimFake{
			id:     classicSourceID,
			plane:  discovery.PlaneClassic,
			claims: true,
		})
		g.Expect(err).NotTo(HaveOccurred())

		enumeration, err := draSource.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeTrue())

		classicIdentity := discovery.Identity{
			ID:       nodeName + "-gpu-0",
			Kind:     discovery.IdentityKindNodeIndex,
			NodeName: nodeName,
			NodeUID:  nodeUID,
			Plane:    discovery.PlaneClassic,
		}
		selection := discovery.SelectNode(context.Background(), nodeInfo(), sources,
			map[string]discovery.Enumeration{
				draSourceID:     enumeration,
				classicSourceID: {Outcome: discovery.OutcomeSuccess, Identities: []discovery.Identity{classicIdentity}},
			})
		g.Expect(selection.SourceID).To(Equal(classicSourceID))
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeSuccess))
		g.Expect(selection.Identities).To(Equal([]discovery.Identity{classicIdentity}))
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

// foreignLocatorFixture is a locator of another source kind.
type foreignLocatorFixture struct{}

func (foreignLocatorFixture) Kind() string    { return classicSourceID }
func (foreignLocatorFixture) Validate() error { return nil }

func (foreignLocatorFixture) Equal(other discovery.Locator) bool {
	return other == foreignLocatorFixture{}
}

func (foreignLocatorFixture) Ref() string { return nodeName }
