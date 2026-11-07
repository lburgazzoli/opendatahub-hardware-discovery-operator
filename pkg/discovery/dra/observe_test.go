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
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/dra"
)

func TestObserve(t *testing.T) {
	t.Run("full facts with collector clock", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{standardSlice()}})

		observation, err := source.Observe(context.Background(), standardIdentity())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(observation.Validate()).To(Succeed())
		g.Expect(observation.Identity).To(Equal(standardIdentity()))
		g.Expect(observation.CollectedAt).To(Equal(fixedNow))

		g.Expect(observation.Facts.Accelerator).NotTo(BeNil())
		g.Expect(observation.Facts.Accelerator.Model).To(Equal(teslaK80))
		g.Expect(observation.Facts.Accelerator.Memory).To(Equal(expectedMemoryBytes()))
		// Neither the qualified nor the legacy attributes carry vendor:
		// omitted, never defaulted.
		g.Expect(observation.Facts.Accelerator.Vendor).To(BeEmpty())
	})

	t.Run("legacy attribute facts", func(t *testing.T) {
		g := NewWithT(t)
		slice := newSlice(nvidiaDriver, poolName, nodeName, 1, 1).
			withLegacyDevice(gpuDeviceName(), gpuUUID, teslaK80, memoryBytes).
			build()
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{slice}})

		observation, err := source.Observe(context.Background(), standardIdentity())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(observation.Facts.Accelerator.Model).To(Equal(teslaK80))
		g.Expect(observation.Facts.Accelerator.Memory).To(Equal(expectedMemoryBytes()))
	})

	t.Run("absent facts are omitted, never zero", func(t *testing.T) {
		g := NewWithT(t)
		slice := newSlice(nvidiaDriver, poolName, nodeName, 1, 1).
			withRawDevice(gpuDeviceName(), map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				attrType: {StringValue: new(typeGPU)},
				attrUUID: {StringValue: new(gpuUUID)},
			}, 0).
			build()
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{slice}})

		observation, err := source.Observe(context.Background(), standardIdentity())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(observation.Validate()).To(Succeed())
		g.Expect(observation.Facts.Accelerator).To(BeNil())
	})

	t.Run("model without memory", func(t *testing.T) {
		g := NewWithT(t)
		slice := newSlice(nvidiaDriver, poolName, nodeName, 1, 1).
			withLegacyDevice(gpuDeviceName(), gpuUUID, teslaK80, 0).
			build()
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{slice}})

		observation, err := source.Observe(context.Background(), standardIdentity())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(observation.Facts.Accelerator.Model).To(Equal(teslaK80))
		g.Expect(observation.Facts.Accelerator.Memory).To(BeNil())
	})

	t.Run("deleted device is stale", func(t *testing.T) {
		g := NewWithT(t)
		slice := newSlice(nvidiaDriver, poolName, nodeName, 1, 1).
			withDevice("another-device-0", otherGPUUUID, teslaK80).
			build()
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{slice}})

		_, err := source.Observe(context.Background(), standardIdentity())
		g.Expect(errors.Is(err, dra.ErrIdentityStale)).To(BeTrue())
	})

	t.Run("renamed uuid is stale", func(t *testing.T) {
		g := NewWithT(t)
		// The device name still exists but now maps to a different GPU:
		// a reused name never silently retargets an existing device.
		slice := newSlice(nvidiaDriver, poolName, nodeName, 1, 1).
			withDevice(gpuDeviceName(), otherGPUUUID, teslaK80).
			build()
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{slice}})

		_, err := source.Observe(context.Background(), standardIdentity())
		g.Expect(errors.Is(err, dra.ErrIdentityStale)).To(BeTrue())
	})

	t.Run("vanished pool is stale", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{})

		_, err := source.Observe(context.Background(), standardIdentity())
		g.Expect(errors.Is(err, dra.ErrIdentityStale)).To(BeTrue())
	})

	t.Run("incomplete pool fails observation", func(t *testing.T) {
		g := NewWithT(t)
		slice := newSlice(nvidiaDriver, poolName, nodeName, 1, 2).
			withDevice(gpuDeviceName(), gpuUUID, teslaK80).
			build()
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{slice}})

		_, err := source.Observe(context.Background(), standardIdentity())
		g.Expect(errors.Is(err, dra.ErrPoolIncomplete)).To(BeTrue())
	})

	t.Run("type changed is stale", func(t *testing.T) {
		g := NewWithT(t)
		slice := newSlice(nvidiaDriver, poolName, nodeName, 1, 1).
			withRawDevice(gpuDeviceName(), map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				attrType: {StringValue: new(typeMIG)},
				attrUUID: {StringValue: new(gpuUUID)},
			}, 0).
			build()
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{slice}})

		_, err := source.Observe(context.Background(), standardIdentity())
		g.Expect(errors.Is(err, dra.ErrIdentityStale)).To(BeTrue())
	})

	t.Run("wrong identity kind", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{standardSlice()}})

		id := standardIdentity()
		id.Kind = discovery.IdentityKindNodeIndex
		_, err := source.Observe(context.Background(), id)
		g.Expect(errors.Is(err, dra.ErrWrongIdentityKind)).To(BeTrue())
	})

	t.Run("foreign locator", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{standardSlice()}})

		id := standardIdentity()
		id.Locator = foreignLocatorFixture{}
		_, err := source.Observe(context.Background(), id)
		g.Expect(errors.Is(err, dra.ErrIdentityStale)).To(BeTrue())
	})

	t.Run("read failure", func(t *testing.T) {
		g := NewWithT(t)
		forbidden := apierrors.NewForbidden(sliceGVR, nodeName, errors.New("rbac"))
		source := newSource(&stubReader{err: forbidden})

		_, err := source.Observe(context.Background(), standardIdentity())
		g.Expect(apierrors.IsForbidden(err)).To(BeTrue())
	})

	t.Run("cancelled context is a hard error", func(t *testing.T) {
		g := NewWithT(t)
		source := newSource(&stubReader{slices: []resourcev1.ResourceSlice{standardSlice()}})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := source.Observe(ctx, standardIdentity())
		g.Expect(err).To(MatchError(context.Canceled))
	})
}
