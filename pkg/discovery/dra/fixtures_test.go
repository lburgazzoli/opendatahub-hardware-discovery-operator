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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/dra"
)

// Named fixtures shared by the DRA source tests. Expected values are
// computed here from the approved contract, independent of the
// implementation.

const (
	nodeName        = "gpu-node-a"
	otherNode       = "gpu-node-b"
	nvidiaDriver    = "gpu.nvidia.com"
	foreignDriver   = "gpu.example.com"
	poolName        = "gpu-node-a"
	otherPool       = "gpu-node-a-2"
	gpuUUID         = "GPU-11111111-2222-3333-4444-555555555555"
	otherGPUUUID    = "GPU-99999999-8888-7777-6666-555555555555"
	teslaK80        = "Tesla-K80"
	otherModel      = "Tesla-V100"
	draSourceID     = "dra"
	classicSourceID = "classic"

	// 11441 MiB expressed in bytes, the FGO gpuMemory convention.
	memoryBytes = int64(11441) * 1024 * 1024

	// Attribute and capacity keys of the approved whole-GPU contract,
	// restated here so fixtures stay independent of the implementation's
	// unexported constants.
	attrType        = "gpu.nvidia.com/type"
	attrUUID        = "gpu.nvidia.com/uuid"
	attrProductName = "gpu.nvidia.com/productName"
	legacyUUIDKey   = "uuid"
	legacyModelKey  = "model"
	capacityMemory  = "memory"
	typeGPU         = "gpu"
	typeMIG         = "mig"
)

var (
	nodeUID  = types.UID("11111111-1111-1111-1111-111111111111")
	otherUID = types.UID("22222222-2222-2222-2222-222222222222")
	fixedNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
)

// stubReader is an in-memory client.Reader serving ResourceSlices. It
// honors exactly the spec.nodeName field selector the source relies on
// and records the last selector it served; Get is deliberately
// unsupported so any use fails the test. The unfiltered mode bypasses
// the selector to prove the source rejects non-node-local advertisements
// itself.
type stubReader struct {
	slices     []resourcev1.ResourceSlice
	err        error
	unfiltered bool
	selector   string
}

func (r *stubReader) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return fmt.Errorf("stub reader: Get must not be used by the dra source")
}

func (r *stubReader) List(
	_ context.Context,
	list client.ObjectList,
	opts ...client.ListOption,
) error {
	if r.err != nil {
		return r.err
	}
	target, ok := list.(*resourcev1.ResourceSliceList)
	if !ok {
		return fmt.Errorf("stub reader: unexpected list type %T", list)
	}

	selected := ""
	for _, opt := range opts {
		if fields, ok := opt.(client.MatchingFields); ok {
			selected = fields[resourcev1.ResourceSliceSelectorNodeName]
		}
	}
	r.selector = selected

	filtered := make([]resourcev1.ResourceSlice, 0, len(r.slices))
	for _, slice := range r.slices {
		if r.unfiltered || selected == "" ||
			(slice.Spec.NodeName != nil && *slice.Spec.NodeName == selected) {
			filtered = append(filtered, slice)
		}
	}
	target.Items = filtered
	return nil
}

// sliceBuilder assembles ResourceSlice fixtures.
type sliceBuilder struct {
	slice resourcev1.ResourceSlice
}

// newSlice builds a node-local slice of a pool at a generation with a
// declared resourceSliceCount.
func newSlice(driver, pool, node string, generation, count int64) *sliceBuilder {
	return &sliceBuilder{slice: resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: pool + "-" + fmt.Sprint(generation) + "-" + node},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   driver,
			NodeName: new(node),
			Pool: resourcev1.ResourcePool{
				Name:               pool,
				Generation:         generation,
				ResourceSliceCount: count,
			},
		},
	}}
}

// withDevice adds a whole-GPU device with the fully qualified attribute
// contract (gpu.nvidia.com/{type,uuid,productName}) and the standard
// memory capacity.
func (b *sliceBuilder) withDevice(name, uuid, model string) *sliceBuilder {
	return b.withRawDevice(name, map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
		attrType:        {StringValue: new(typeGPU)},
		attrUUID:        {StringValue: new(uuid)},
		attrProductName: {StringValue: new(model)},
	}, memoryBytes)
}

// withLegacyDevice adds a whole-GPU device using the v0.2.1 back-compat
// unqualified uuid/model keys. The qualified type discriminator is still
// set: upstream DeviceClass CEL reads it, so FGO always publishes it.
func (b *sliceBuilder) withLegacyDevice(name, uuid, model string, memoryBytes int64) *sliceBuilder {
	return b.withRawDevice(name, map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
		attrType:       {StringValue: new(typeGPU)},
		legacyUUIDKey:  {StringValue: new(uuid)},
		legacyModelKey: {StringValue: new(model)},
	}, memoryBytes)
}

// withRawDevice adds a device with an arbitrary attribute map and an
// optional memory capacity (0 omits it).
func (b *sliceBuilder) withRawDevice(
	name string,
	attributes map[resourcev1.QualifiedName]resourcev1.DeviceAttribute,
	memoryBytes int64,
) *sliceBuilder {
	device := resourcev1.Device{Name: name, Attributes: attributes}
	if memoryBytes > 0 {
		device.Capacity = map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
			capacityMemory: {Value: *resource.NewQuantity(memoryBytes, resource.BinarySI)},
		}
	}
	b.slice.Spec.Devices = append(b.slice.Spec.Devices, device)
	return b
}

// withNodeSelector rebinds the slice to a nodeSelector layout.
func (b *sliceBuilder) withNodeSelector() *sliceBuilder {
	b.slice.Spec.NodeName = nil
	b.slice.Spec.NodeSelector = &corev1.NodeSelector{}
	b.slice.Name += "-selector"
	return b
}

// withAllNodes rebinds the slice to an allNodes layout.
func (b *sliceBuilder) withAllNodes() *sliceBuilder {
	b.slice.Spec.NodeName = nil
	b.slice.Spec.AllNodes = new(true)
	b.slice.Name += "-allnodes"
	return b
}

// uniqueName makes the slice's object name unique among fixtures.
func (b *sliceBuilder) uniqueName(suffix string) *sliceBuilder {
	b.slice.Name += "-" + suffix
	return b
}

func (b *sliceBuilder) build() resourcev1.ResourceSlice {
	return b.slice
}

// gpuDeviceName is the device name of the standard fixture GPU.
func gpuDeviceName() string { return strings.ToLower(gpuUUID) }

// otherDeviceName is the device name of the second fixture GPU.
func otherDeviceName() string { return strings.ToLower(otherGPUUUID) }

// standardSlice is the canonical single-GPU advertisement: one complete
// pool, one whole GPU with the full attribute and capacity contract.
func standardSlice() resourcev1.ResourceSlice {
	return singleGPUSlice(poolName)
}

// twoGPUSlices builds a complete two-slice, two-GPU pool.
func twoGPUSlices() []resourcev1.ResourceSlice {
	first := newSlice(nvidiaDriver, poolName, nodeName, 1, 2).
		withDevice(gpuDeviceName(), gpuUUID, teslaK80).
		build()
	second := newSlice(nvidiaDriver, poolName, nodeName, 1, 2).
		withDevice(otherDeviceName(), otherGPUUUID, otherModel).
		uniqueName("second").
		build()
	return []resourcev1.ResourceSlice{first, second}
}

// newSource builds the source over a reader with the fixed test clock.
func newSource(reader client.Reader, opts ...dra.Option) *dra.Source {
	opts = append([]dra.Option{dra.WithClock(func() time.Time { return fixedNow })}, opts...)
	source, err := dra.New(reader, opts...)
	if err != nil {
		panic(fmt.Sprintf("fixture source: %v", err))
	}
	return source
}

// nodeInfo is the NodeInfo of the standard fixture node.
func nodeInfo() discovery.NodeInfo {
	return discovery.NodeInfo{Name: nodeName, UID: nodeUID}
}

// standardIdentity is the identity the standard slice must enumerate to.
func standardIdentity() discovery.Identity {
	return discovery.Identity{
		ID:       gpuUUID,
		Kind:     discovery.IdentityKindUUID,
		NodeName: nodeName,
		NodeUID:  nodeUID,
		Plane:    discovery.PlaneDRA,
		Locator:  dra.Locator{Driver: nvidiaDriver, Pool: poolName, Device: gpuDeviceName()},
	}
}

// otherIdentity is the identity of the second GPU of the two-GPU pool.
func otherIdentity() discovery.Identity {
	return discovery.Identity{
		ID:       otherGPUUUID,
		Kind:     discovery.IdentityKindUUID,
		NodeName: nodeName,
		NodeUID:  nodeUID,
		Plane:    discovery.PlaneDRA,
		Locator:  dra.Locator{Driver: nvidiaDriver, Pool: poolName, Device: otherDeviceName()},
	}
}

// singleGPUSlice builds a complete single-GPU pool slice advertising the
// standard GPU under the given pool name.
func singleGPUSlice(pool string) resourcev1.ResourceSlice {
	return newSlice(nvidiaDriver, pool, nodeName, 1, 1).
		withDevice(gpuDeviceName(), gpuUUID, teslaK80).
		build()
}
