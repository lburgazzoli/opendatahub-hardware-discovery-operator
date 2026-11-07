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
	"slices"
	"testing"

	. "github.com/onsi/gomega"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/dra"
)

var sliceGVR = schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceslices"}

func TestSourceIdentity(t *testing.T) {
	g := NewWithT(t)

	source := newSource(&stubReader{})
	g.Expect(source.ID()).To(Equal(draSourceID))
	g.Expect(source.Plane()).To(Equal(discovery.PlaneDRA))
}

func TestNewValidation(t *testing.T) {
	g := NewWithT(t)

	_, err := dra.New(nil)
	g.Expect(err).To(HaveOccurred())

	_, err = dra.New(&stubReader{}, dra.WithDriverAllowList())
	g.Expect(err).To(HaveOccurred())

	_, err = dra.New(&stubReader{}, dra.WithClock(nil))
	g.Expect(err).To(HaveOccurred())
}

func TestClaimsNode(t *testing.T) {
	cases := []struct {
		name   string
		reader *stubReader
		claim  bool
	}{
		{
			"complete advertisement",
			&stubReader{slices: []resourcev1.ResourceSlice{standardSlice()}},
			true,
		},
		{
			// The claim holds even while the pool is incomplete: DRA
			// dominance must suppress the classic fallback.
			"incomplete pool",
			&stubReader{slices: []resourcev1.ResourceSlice{
				newSlice(nvidiaDriver, poolName, nodeName, 1, 2).
					withDevice(gpuDeviceName(), gpuUUID, teslaK80).
					build(),
			}},
			true,
		},
		{
			// A MIG-only node still has allow-listed advertisements: the
			// node is claimed even though no whole GPU is enumerable.
			"mig-only advertisement",
			&stubReader{slices: []resourcev1.ResourceSlice{
				newSlice(nvidiaDriver, poolName, nodeName, 1, 1).
					withRawDevice("mig-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						attrType: {StringValue: new(typeMIG)},
					}, 0).
					build(),
			}},
			true,
		},
		{
			"foreign driver",
			&stubReader{slices: []resourcev1.ResourceSlice{
				newSlice(foreignDriver, poolName, nodeName, 1, 1).
					withDevice(gpuDeviceName(), gpuUUID, teslaK80).
					build(),
			}},
			false,
		},
		{
			// The unfiltered reader returns the slice despite the field
			// selector: the source itself must reject non-node-local
			// layouts instead of guessing a device host.
			"node selector layout",
			&stubReader{
				unfiltered: true,
				slices: []resourcev1.ResourceSlice{
					newSlice(nvidiaDriver, poolName, nodeName, 1, 1).
						withDevice(gpuDeviceName(), gpuUUID, teslaK80).
						withNodeSelector().
						build(),
				},
			},
			false,
		},
		{
			"all nodes layout",
			&stubReader{
				unfiltered: true,
				slices: []resourcev1.ResourceSlice{
					newSlice(nvidiaDriver, poolName, nodeName, 1, 1).
						withDevice(gpuDeviceName(), gpuUUID, teslaK80).
						withAllNodes().
						build(),
				},
			},
			false,
		},
		{"no slices", &stubReader{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(newSource(tc.reader).ClaimsNode(context.Background(), nodeInfo())).To(Equal(tc.claim))
		})
	}

	t.Run("list error withdraws the claim", func(t *testing.T) {
		g := NewWithT(t)
		reader := &stubReader{err: errors.New("boom")}
		g.Expect(newSource(reader).ClaimsNode(context.Background(), nodeInfo())).To(BeFalse())
	})
}

func TestEnumerate(t *testing.T) {
	t.Run("whole gpu identity from qualified attributes", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{standardSlice()}})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Validate()).To(Succeed())
		g.Expect(enumeration.Authoritative()).To(BeTrue())
		g.Expect(enumeration.Identities).To(Equal([]discovery.Identity{standardIdentity()}))
	})

	t.Run("lists with the node name field selector", func(t *testing.T) {
		g := NewWithT(t)
		reader := &stubReader{slices: []resourcev1.ResourceSlice{standardSlice()}}

		_, err := newSource(reader).Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(reader.selector).To(Equal(nodeName))
	})

	t.Run("fgo unqualified fallback keys", func(t *testing.T) {
		g := NewWithT(t)
		slice := newSlice(nvidiaDriver, poolName, nodeName, 1, 1).
			withLegacyDevice(gpuDeviceName(), gpuUUID, teslaK80, memoryBytes).
			build()
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{slice}})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeTrue())
		g.Expect(enumeration.Identities).To(Equal([]discovery.Identity{standardIdentity()}))
	})

	t.Run("qualified keys take precedence over legacy keys", func(t *testing.T) {
		g := NewWithT(t)
		slice := newSlice(nvidiaDriver, poolName, nodeName, 1, 1).
			withRawDevice(gpuDeviceName(), map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				attrType:        {StringValue: new(typeGPU)},
				attrUUID:        {StringValue: new(gpuUUID)},
				legacyUUIDKey:   {StringValue: new("GPU-legacy")},
				attrProductName: {StringValue: new(teslaK80)},
				legacyModelKey:  {StringValue: new("Legacy-K80")},
			}, memoryBytes).
			build()
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{slice}})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Identities).To(Equal([]discovery.Identity{standardIdentity()}))

		observation, err := source.Observe(context.Background(), standardIdentity())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(observation.Facts.Accelerator.Model).To(Equal(teslaK80))
	})

	t.Run("missing uuid is skipped, never fabricated", func(t *testing.T) {
		g := NewWithT(t)
		slice := newSlice(nvidiaDriver, poolName, nodeName, 1, 1).
			withRawDevice(gpuDeviceName(), map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				attrType:        {StringValue: new(typeGPU)},
				attrProductName: {StringValue: new(teslaK80)},
			}, memoryBytes).
			build()
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{slice}})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeTrue())
		g.Expect(enumeration.Identities).To(BeEmpty())
	})

	t.Run("mig and untyped devices are skipped", func(t *testing.T) {
		g := NewWithT(t)
		slice := newSlice(nvidiaDriver, poolName, nodeName, 1, 1).
			withRawDevice("mig-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				attrType: {StringValue: new(typeMIG)},
				attrUUID: {StringValue: new(gpuUUID)},
			}, 0).
			withRawDevice("untyped-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				attrUUID: {StringValue: new(otherGPUUUID)},
			}, 0).
			build()
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{slice}})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeTrue())
		g.Expect(enumeration.Identities).To(BeEmpty())
	})

	t.Run("foreign driver is not enumerated", func(t *testing.T) {
		g := NewWithT(t)
		slice := newSlice(foreignDriver, poolName, nodeName, 1, 1).
			withDevice(gpuDeviceName(), gpuUUID, teslaK80).
			build()
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{slice}})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeTrue())
		g.Expect(enumeration.Identities).To(BeEmpty())
	})

	t.Run("multi-slice complete pool", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{slices: twoGPUSlices()})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeTrue())
		g.Expect(enumeration.Identities).To(Equal([]discovery.Identity{standardIdentity(), otherIdentity()}))
	})

	t.Run("shuffled input is deterministic", func(t *testing.T) {
		g := NewWithT(t)
		reversed := twoGPUSlices()
		slices.Reverse(reversed)

		forward, err := newSource(&stubReader{slices: twoGPUSlices()}).
			Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		backward, err := newSource(&stubReader{slices: reversed}).
			Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(backward.Identities).To(Equal(forward.Identities))
		g.Expect(backward.Identities).To(Equal([]discovery.Identity{standardIdentity(), otherIdentity()}))
	})

	t.Run("incomplete pool is unknown, never zero", func(t *testing.T) {
		g := NewWithT(t)
		slice := newSlice(nvidiaDriver, poolName, nodeName, 1, 2).
			withDevice(gpuDeviceName(), gpuUUID, teslaK80).
			build()
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{slice}})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeFalse())
		g.Expect(enumeration.Identities).To(BeEmpty())
		g.Expect(errors.Is(enumeration.Err, dra.ErrPoolIncomplete)).To(BeTrue())
	})

	t.Run("conflicting declared counts are unknown", func(t *testing.T) {
		g := NewWithT(t)
		first := newSlice(nvidiaDriver, poolName, nodeName, 1, 2).
			withDevice(gpuDeviceName(), gpuUUID, teslaK80).
			build()
		second := newSlice(nvidiaDriver, poolName, nodeName, 1, 3).
			withDevice(otherDeviceName(), otherGPUUUID, otherModel).
			uniqueName("conflict").
			build()
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{first, second}})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeFalse())
		g.Expect(enumeration.Identities).To(BeEmpty())
		g.Expect(errors.Is(enumeration.Err, dra.ErrPoolIncomplete)).To(BeTrue())
	})

	t.Run("partially visible higher generation is unknown", func(t *testing.T) {
		g := NewWithT(t)
		old := singleGPUSlice(poolName)
		next := newSlice(nvidiaDriver, poolName, nodeName, 2, 2).
			withDevice(gpuDeviceName(), gpuUUID, teslaK80).
			build()
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{old, next}})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeFalse())
		g.Expect(enumeration.Identities).To(BeEmpty())
		g.Expect(errors.Is(enumeration.Err, dra.ErrPoolIncomplete)).To(BeTrue())
	})

	t.Run("old generation is ignored", func(t *testing.T) {
		g := NewWithT(t)
		old := newSlice(nvidiaDriver, poolName, nodeName, 1, 1).
			withDevice(gpuDeviceName(), gpuUUID, teslaK80).
			build()
		next := newSlice(nvidiaDriver, poolName, nodeName, 2, 1).
			withDevice(otherDeviceName(), otherGPUUUID, otherModel).
			build()
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{old, next}})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeTrue())
		g.Expect(enumeration.Identities).To(Equal([]discovery.Identity{otherIdentity()}))
	})

	t.Run("same uuid in two pools is ambiguous", func(t *testing.T) {
		g := NewWithT(t)
		first := singleGPUSlice(poolName)
		second := newSlice(nvidiaDriver, otherPool, nodeName, 1, 1).
			withDevice(otherDeviceName(), gpuUUID, teslaK80).
			build()
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{first, second}})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeFalse())
		g.Expect(enumeration.Identities).To(BeEmpty())
		g.Expect(errors.Is(enumeration.Err, dra.ErrAmbiguousIdentity)).To(BeTrue())
	})

	t.Run("identical duplicate advertisement is one device", func(t *testing.T) {
		g := NewWithT(t)
		first := newSlice(nvidiaDriver, poolName, nodeName, 1, 2).
			withDevice(gpuDeviceName(), gpuUUID, teslaK80).
			build()
		second := newSlice(nvidiaDriver, poolName, nodeName, 1, 2).
			withDevice(gpuDeviceName(), gpuUUID, teslaK80).
			uniqueName("duplicate").
			build()
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{first, second}})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeTrue())
		g.Expect(enumeration.Identities).To(Equal([]discovery.Identity{standardIdentity()}))
	})

	t.Run("custom driver allow-list", func(t *testing.T) {
		g := NewWithT(t)
		foreignSlice := newSlice(foreignDriver, otherPool, nodeName, 1, 1).
			withDevice(gpuDeviceName(), gpuUUID, teslaK80).
			uniqueName("foreign").
			build()
		source := newSource(
			&stubReader{slices: []resourcev1.ResourceSlice{standardSlice(), foreignSlice}},
			dra.WithDriverAllowList(foreignDriver),
		)

		g.Expect(source.ClaimsNode(context.Background(), nodeInfo())).To(BeTrue())

		// Only the allow-listed driver is read: the identity comes from
		// the foreign slice and the nvidia slice is ignored.
		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeTrue())
		expected := standardIdentity()
		expected.Locator = dra.Locator{Driver: foreignDriver, Pool: otherPool, Device: gpuDeviceName()}
		g.Expect(enumeration.Identities).To(Equal([]discovery.Identity{expected}))
	})

	t.Run("read failure is unknown, never zero", func(t *testing.T) {
		g := NewWithT(t)
		forbidden := apierrors.NewForbidden(sliceGVR, nodeName, errors.New("rbac"))
		source := newSource(&stubReader{err: forbidden})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeFalse())
		g.Expect(enumeration.Identities).To(BeEmpty())
		g.Expect(enumeration.Err).To(MatchError(forbidden))
	})

	t.Run("invalid node info is a hard error", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{standardSlice()}})

		_, err := source.Enumerate(context.Background(), []discovery.NodeInfo{{UID: nodeUID}})
		g.Expect(err).To(HaveOccurred())
		g.Expect(errors.Is(err, discovery.ErrMissingNode)).To(BeTrue())
	})

	t.Run("cancelled context is a hard error", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{standardSlice()}})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := source.Enumerate(ctx, []discovery.NodeInfo{nodeInfo()})
		g.Expect(err).To(MatchError(context.Canceled))
	})

	t.Run("two nodes combine without cross-attribution", func(t *testing.T) {
		g := NewWithT(t)
		nodeBSlice := newSlice(nvidiaDriver, "pool-b", otherNode, 1, 1).
			withDevice(otherDeviceName(), otherGPUUUID, otherModel).
			uniqueName("nodeb").
			build()
		// The unfiltered reader returns every slice to both node reads:
		// the source itself must attribute devices to their own node.
		source := newSource(&stubReader{
			unfiltered: true,
			slices:     []resourcev1.ResourceSlice{standardSlice(), nodeBSlice},
		})

		enumeration, err := source.Enumerate(context.Background(), []discovery.NodeInfo{
			nodeInfo(),
			{Name: otherNode, UID: otherUID},
		})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(enumeration.Authoritative()).To(BeTrue())
		g.Expect(enumeration.Identities).To(ConsistOf(
			standardIdentity(),
			discovery.Identity{
				ID:       otherGPUUUID,
				Kind:     discovery.IdentityKindUUID,
				NodeName: otherNode,
				NodeUID:  otherUID,
				Plane:    discovery.PlaneDRA,
				Locator:  dra.Locator{Driver: nvidiaDriver, Pool: "pool-b", Device: otherDeviceName()},
			},
		))
	})
}

// expectedMemoryBytes recomputes the contract's memory expectation
// independently of the fixtures' quantity construction.
func expectedMemoryBytes() *resource.Quantity {
	return resource.NewQuantity(memoryBytes, resource.BinarySI)
}
