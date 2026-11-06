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
)

// HardwareDevice represents one whole physical or simulated hardware device
// (in this PoC, one whole GPU). One object never represents an allocation
// replica, a MIG slice, or any other partition of a device.
//
// The spec is written exclusively by the HardwareDiscovery singleton
// controller; the status is written exclusively by the HardwareDevice
// controller through the /status subresource.
//
// Additive extension policy (design doc §4.5): future facts are added as new
// optional status fields (or optional structs with their own validation) and
// new spec.class enum values; no placeholder empty structs, catch-all vendor
// attribute maps, or speculative locators ship in v1alpha1.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,path=hardwaredevices,shortName=hwdev;hw,categories=odh;platform;all
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.class`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.status.model`
// +kubebuilder:printcolumn:name="Memory",type=string,JSONPath=`.status.memory`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.host.name`
// +kubebuilder:printcolumn:name="Plane",type=string,JSONPath=`.status.allocationPlane`
// +kubebuilder:printcolumn:name="Collected",type=date,JSONPath=`.status.collectedAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
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
// +kubebuilder:validation:XValidation:rule="(has(self.source.dra) ? 1 : 0) + (has(self.source.simulator) ? 1 : 0) == 1",message="exactly one source locator (dra or simulator) is required"
// +kubebuilder:validation:XValidation:rule="!has(self.source.simulator) || self.source.simulator.id == self.identity.id",message="simulator source id must match spec.identity.id"
type HardwareDeviceSpec struct {
	// class is the device class this inventory entry belongs to.
	// accelerator is the only class in this PoC; new values are additive,
	// reviewed schema changes.
	// +kubebuilder:validation:Enum=accelerator
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="class is immutable"
	Class string `json:"class"`

	// identity is the immutable physical (or simulated) device identity.
	// +required
	Identity DeviceIdentity `json:"identity"`

	// host is the Node the device was discovered on. Node replacement is
	// delete-and-recreate of the HardwareDevice, never an in-place UID
	// transfer.
	// +required
	Host DeviceHost `json:"host"`

	// source is the locator union telling the device controller how to
	// re-observe this device. Exactly one arm must be set.
	// +required
	Source DeviceSource `json:"source"`
}

// DeviceIdentity identifies one whole physical or simulated device.
type DeviceIdentity struct {
	// id is the physical or simulated device ID (for NVIDIA GPUs, the GPU
	// UUID). It is never derived from counts, list indexes, or aggregates.
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

// DeviceSource is the frozen, source-owned locator union. Exactly one arm is
// set, enforced by a counting CEL rule on HardwareDeviceSpec so that adding a
// third arm later is a reviewed schema change, not a silent reopening.
type DeviceSource struct {
	// dra locates the device in a stable resource.k8s.io/v1 ResourceSlice
	// advertisement. The enforced driver allow-list is the singleton's
	// spec.sources.dra.driverAllowList (default pin gpu.nvidia.com).
	// +optional
	DRA *DRASource `json:"dra,omitempty"`

	// simulator locates the device in the Run:ai fake-gpu-operator
	// per-node topology ConfigMap. It is written only by the opt-in,
	// PoC-only classic simulator profile.
	// +optional
	Simulator *SimulatorSource `json:"simulator,omitempty"`
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

// SimulatorSource is the fake-gpu-operator topology ConfigMap locator of one
// device. It exists only for the PoC simulator profile and is opt-in.
type SimulatorSource struct {
	// namespace is the namespace whose ConfigMaps the opt-in simulator
	// adapter reads; no other namespace is ever read.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`

	// configMap is the per-node topology ConfigMap name
	// (topology-<node> by convention).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ConfigMap string `json:"configMap"`

	// id is the synthetic device ID inside the ConfigMap; it always equals
	// spec.identity.id (enforced by CEL).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	ID string `json:"id"`
}

// HardwareDeviceStatus is the observed state of one device, written only by
// the HardwareDevice controller via the /status subresource. Optional facts
// the source does not publish are omitted, never zero or defaulted.
type HardwareDeviceStatus struct {
	// vendor is the device vendor when the source publishes it. Neither the
	// PoC simulator ConfigMaps nor the NVIDIA DRA attributes carry vendor,
	// so it is omitted in this PoC.
	// +optional
	Vendor string `json:"vendor,omitempty"`

	// model is the observed device model (fake-gpu-operator gpuProduct or
	// the DRA gpu.nvidia.com/productName attribute).
	// +optional
	Model string `json:"model,omitempty"`

	// memory is the observed device memory as a Quantity in bytes when the
	// source publishes it.
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`

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
	// example dra/gpu.nvidia.com/<pool> or simulator/<namespace>/<configmap>.
	// It is replaced, never merged, on an observed source change.
	// +optional
	SourceRef string `json:"sourceRef,omitempty"`

	// conditions describe the device observation (not hardware health and
	// not free allocation capacity).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the generation of the spec most recently
	// observed by the device controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
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
