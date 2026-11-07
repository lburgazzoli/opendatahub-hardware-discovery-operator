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

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
)

// HardwareDevice represents one whole hardware device (in this PoC, one
// whole GPU). One object never represents an allocation replica, a MIG
// slice, or any other partition of a device.
//
// The spec is written exclusively by the HardwareDiscovery singleton
// controller; the status is written exclusively by the HardwareDevice
// controller through the /status subresource.
//
// Additive extension policy (design doc §4.5): per-class observed facts live
// in optional status structs selected by the immutable spec.class (in this
// PoC, only status.accelerator); future classes arrive as new sibling
// structs plus new spec.class enum values, and cross-class facts stay at the
// status top level. No placeholder empty structs, catch-all vendor attribute
// maps, or speculative locators ship in v1alpha1.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,path=hardwaredevices,shortName=hwdev;hw,categories=odh;platform;all
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.class`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.status.accelerator.model`
// +kubebuilder:printcolumn:name="Memory",type=string,JSONPath=`.status.accelerator.memory`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.host.name`
// +kubebuilder:printcolumn:name="Plane",type=string,JSONPath=`.status.allocationPlane`
// +kubebuilder:printcolumn:name="Collected",type=date,JSONPath=`.status.collectedAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="!has(self.status.accelerator) || self.spec.class == 'accelerator'",message="status.accelerator is only valid when spec.class is accelerator"
type HardwareDevice struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec is the desired state of HardwareDevice.
	// +required
	Spec HardwareDeviceSpec `json:"spec"`

	// status is the observed state of HardwareDevice.
	// +optional
	Status HardwareDeviceStatus `json:"status,omitzero"`
}

// HardwareDeviceSpec is the identity and locator contract of one device.
// It is owned by the HardwareDiscovery singleton controller.
// +kubebuilder:validation:XValidation:rule="(has(self.source.dra) ? 1 : 0) + (has(self.source.classic) ? 1 : 0) == 1",message="exactly one source locator (dra or classic) is required"
// +kubebuilder:validation:XValidation:rule="(self.source.kind == 'dra') == has(self.source.dra)",message="source.kind must match the set source arm"
// +kubebuilder:validation:XValidation:rule="!has(self.source.classic) || self.source.classic.node == self.host.name",message="classic source node must match spec.host.name"
type HardwareDeviceSpec struct {
	// class is the device class this inventory entry belongs to.
	// accelerator is the only class in this PoC; new values are additive,
	// reviewed schema changes.
	// +kubebuilder:validation:Enum=accelerator
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="class is immutable"
	Class string `json:"class"`

	// identity is the immutable device identity.
	// +required
	Identity DeviceIdentity `json:"identity"`

	// host is the Node the device was discovered on. Node replacement is
	// delete-and-recreate of the HardwareDevice, never an in-place UID
	// transfer.
	// +required
	Host DeviceHost `json:"host"`

	// source is the locator telling the device controller how to re-observe
	// this device: the registered source kind plus exactly one typed arm.
	// +required
	Source DeviceSource `json:"source"`
}

// DeviceIdentity identifies one whole device, real or synthetic.
type DeviceIdentity struct {
	// kind is the nature of the identity: uuid is a real vendor device ID
	// (for NVIDIA GPUs, the GPU UUID published by DRA); nodeIndex is a
	// synthetic, node-scoped identity derived from the node name and the
	// device index within the node (classic labels source). A nodeIndex
	// identity is stable only for the node incarnation and is not joinable
	// with telemetry or vendor tooling.
	// +kubebuilder:validation:Enum=uuid;nodeIndex
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="identity.kind is immutable"
	Kind string `json:"kind"`

	// id is the device ID: the vendor device ID (GPU UUID) for kind uuid,
	// or the deterministic <node-name>-gpu-<index> for kind nodeIndex
	// (hash-truncated to stay within 63 characters). It is immutable and
	// unique within the node incarnation.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="identity.id is immutable"
	ID string `json:"id"`
}

// DeviceHost references the Node a device was discovered on.
type DeviceHost struct {
	// name is the Node name at discovery time. The full node name always
	// lives here even when the service-written node label is truncated.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// uid is the Node UID at discovery time. A UID change is a new node
	// incarnation: the old device object is deleted after the grace period
	// and a fresh one is created; status is never transferred.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="host.uid is immutable"
	UID types.UID `json:"uid"`
}

// DeviceSource is the source-owned locator of one device: the registered
// source kind plus exactly one typed arm (enforced by a counting CEL rule on
// HardwareDeviceSpec, so that adding a new arm later is a reviewed schema
// change, not a silent reopening). The production binary registers the dra
// and classic source kinds.
type DeviceSource struct {
	// kind is the registered source kind that owns the locator arm. It must
	// match the set arm (enforced by CEL).
	// +kubebuilder:validation:Enum=dra;classic
	Kind string `json:"kind"`

	// dra locates the device in a stable resource.k8s.io/v1 ResourceSlice
	// advertisement. The enforced driver allow-list is the singleton's
	// spec.sources.dra.driverAllowList (default pin gpu.nvidia.com).
	// +optional
	DRA *DRASource `json:"dra,omitempty"`

	// classic locates the device in the classic labels input of its hosting
	// node (GFD-style node labels plus node extended resources).
	// +optional
	Classic *ClassicSource `json:"classic,omitempty"`
}

// DRASource is the ResourceSlice locator of one device.
type DRASource struct {
	// driver is the DRA driver that advertised the device (for example
	// gpu.nvidia.com). The accepted set is configurable via the singleton's
	// driverAllowList; the value must be a DNS-style driver name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Driver string `json:"driver"`

	// pool is the ResourceSlice pool.name the device was advertised in.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Pool string `json:"pool"`

	// device is the ResourceSlice device.name of this device.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Device string `json:"device"`
}

// ClassicSource is the classic labels locator of one device: the hosting
// node and the device's index within the node's published gpu.count. The
// identity of such a device is synthetic and node-scoped
// (spec.identity.kind: nodeIndex).
type ClassicSource struct {
	// node is the name of the Node whose labels describe this device. It
	// always equals spec.host.name (enforced by CEL).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Node string `json:"node"`

	// index is the device's index within the node's published gpu.count.
	// It is stable for the node incarnation and dies with it.
	// +kubebuilder:validation:Minimum=0
	Index int32 `json:"index"`
}

// HardwareDeviceStatus is the observed state of one device, written only by
// the HardwareDevice controller via the /status subresource. Per-class facts
// live in the optional struct selected by the immutable spec.class; optional
// facts the source does not publish are omitted, never zero or defaulted.
// The platform status block (phase, observedGeneration and framework
// conditions) is embedded inline from the odh-platform-utilities framework
// so the type satisfies fwapi.PlatformObject.
type HardwareDeviceStatus struct {
	// Status is the common platform status block: phase,
	// observedGeneration and framework api.Condition conditions.
	// +optional
	fwapi.Status `json:",inline"`

	// accelerator carries the observed facts of an accelerator-class device.
	// It is present iff facts have been observed for a device whose
	// spec.class is accelerator (enforced by CEL). Future device classes
	// arrive as new sibling structs; class-specific facts never live at the
	// status top level.
	// +optional
	Accelerator *AcceleratorDeviceStatus `json:"accelerator,omitempty"`

	// allocationPlane is the allocation plane that evidenced the device.
	// +optional
	// +kubebuilder:validation:Enum=classic;dra
	AllocationPlane string `json:"allocationPlane,omitempty"`

	// collectedAt is the collector observation time. It is the collector's
	// own clock, never a producer timestamp, and it is only refreshed by a
	// successful observation.
	// +optional
	CollectedAt *metav1.Time `json:"collectedAt,omitempty"`

	// sourceRef is the provenance of the last successful observation, for
	// example dra/gpu.nvidia.com/<pool> or classic/<node>. It is replaced,
	// never merged, on an observed source change.
	// +optional
	SourceRef string `json:"sourceRef,omitempty"`
}

// GetStatus returns the embedded platform status block (fwapi.WithStatus).
func (d *HardwareDevice) GetStatus() *fwapi.Status {
	return &d.Status.Status
}

// GetConditions returns the platform conditions (fwapi.ConditionsAccessor).
func (d *HardwareDevice) GetConditions() []fwapi.Condition {
	return d.Status.GetConditions()
}

// SetConditions replaces the platform conditions (fwapi.ConditionsAccessor).
func (d *HardwareDevice) SetConditions(conditions []fwapi.Condition) {
	d.Status.SetConditions(conditions)
}

// AcceleratorDeviceStatus carries the observed facts of one whole
// accelerator (GPU). Optional facts the source does not publish are omitted,
// never zero or defaulted.
type AcceleratorDeviceStatus struct {
	// vendor is the device vendor when the source publishes it. Neither the
	// classic GFD labels nor the NVIDIA DRA attributes carry vendor, so it
	// is omitted in this PoC.
	// +optional
	Vendor string `json:"vendor,omitempty"`

	// model is the observed device model (the classic
	// nvidia.com/gpu.product label or the DRA gpu.nvidia.com/productName
	// attribute).
	// +optional
	Model string `json:"model,omitempty"`

	// memory is the observed device memory as a Quantity in bytes when the
	// source publishes it.
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`
}

// +kubebuilder:object:root=true

// HardwareDeviceList contains a list of HardwareDevice.
type HardwareDeviceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []HardwareDevice `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &HardwareDevice{}, &HardwareDeviceList{})
		return nil
	})
}
