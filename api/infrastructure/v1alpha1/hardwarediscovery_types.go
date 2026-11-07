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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
)

// HardwareDiscovery is the singleton configuration of the hardware discovery
// service. Exactly one object named "cluster" may exist; the fixed instance
// name is enforced by a root-schema CEL rule, so no webhook is needed.
// Deleting and recreating "cluster" is the documented reset path.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,path=hardwarediscoveries
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'cluster'",message="the HardwareDiscovery singleton must be named 'cluster'"
type HardwareDiscovery struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec is the desired state of HardwareDiscovery.
	// +required
	Spec HardwareDiscoverySpec `json:"spec"`

	// status is the observed state of HardwareDiscovery.
	// +optional
	Status HardwareDiscoveryStatus `json:"status,omitzero"`
}

// HardwareDiscoverySpec is the singleton configuration: enabled device
// classes, node selection, lifecycle timings and source-specific settings.
// Empty node selection means all eligible nodes.
type HardwareDiscoverySpec struct {
	// class is the device class this singleton discovers. accelerator is
	// the only class in this PoC; new values are additive, reviewed schema
	// changes.
	// +kubebuilder:validation:Enum=accelerator
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="class is immutable"
	Class string `json:"class"`

	// nodeSelector selects the nodes whose devices are discovered. An
	// omitted or empty selector matches all eligible nodes.
	// +optional
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`

	// maxStale bounds the heartbeat for unchanged facts: unchanged
	// successful observations rewrite status at most this often.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:default="10m"
	// +kubebuilder:validation:Pattern=`^([0-9]+(s|m|h))+$`
	// +optional
	MaxStale metav1.Duration `json:"maxStale,omitzero"`

	// rescanPeriod bounds the periodic rescan that runs even without input
	// events.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:default="5m"
	// +kubebuilder:validation:Pattern=`^([0-9]+(s|m|h))+$`
	// +optional
	RescanPeriod metav1.Duration `json:"rescanPeriod,omitzero"`

	// nodeGracePeriod is how long devices of an absent or UID-changed node
	// are retained before pruning. The deadline is persisted on each
	// HardwareDevice via the node-orphaned-at annotation so controller
	// restarts cannot reset it.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:default="15m"
	// +kubebuilder:validation:Pattern=`^([0-9]+(s|m|h))+$`
	// +optional
	NodeGracePeriod metav1.Duration `json:"nodeGracePeriod,omitzero"`

	// sources holds the source-specific settings.
	// +optional
	Sources DiscoverySources `json:"sources,omitzero"`
}

// DiscoverySources is the per-source configuration of the singleton.
type DiscoverySources struct {
	// dra configures the stable resource.k8s.io/v1 source.
	// +optional
	DRA *DRASourceConfig `json:"dra,omitempty"`

	// classic configures the classic labels source (GFD-style node labels
	// plus node extended resources).
	// +optional
	Classic *ClassicSourceConfig `json:"classic,omitempty"`
}

// DRASourceConfig configures the stable DRA source.
type DRASourceConfig struct {
	// enabled turns the stable DRA source on or off.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// driverAllowList restricts which DRA drivers are trusted to advertise
	// whole GPUs. Only allow-listed drivers are read.
	// +optional
	// +kubebuilder:default={gpu.nvidia.com}
	DriverAllowList []string `json:"driverAllowList,omitempty"`
}

// ClassicSourceConfig configures the classic labels source. The source is
// enabled by default: its node claims stay evidence-gated (the configured
// resource name in node allocatable/capacity plus GFD-style labels), so
// enabling it never fabricates devices on non-GPU nodes.
type ClassicSourceConfig struct {
	// enabled turns the classic labels source on or off.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// resourceName is the extended resource whose presence in node
	// allocatable or capacity is classic-plane evidence.
	// +kubebuilder:default=nvidia.com/gpu
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*\/)?[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +optional
	ResourceName string `json:"resourceName,omitempty"`
}

// HardwareDiscoveryStatus is the observed state of the singleton: collector
// health and a compact per-node inventory overview. Conditions describe the
// collector and its observations, never hardware health or free allocation
// capacity. The platform status block (phase, observedGeneration and
// framework conditions) is embedded inline from the odh-platform-utilities
// framework so the type satisfies fwapi.PlatformObject.
type HardwareDiscoveryStatus struct {
	// Status is the common platform status block: phase,
	// observedGeneration and framework api.Condition conditions.
	// +optional
	fwapi.Status `json:",inline"`

	// nodeSummaries is a compact per-selected-node inventory overview.
	// +optional
	// +listType=map
	// +listMapKey=name
	NodeSummaries []NodeSummary `json:"nodeSummaries,omitempty"`
}

// GetStatus returns the embedded platform status block (fwapi.WithStatus).
func (s *HardwareDiscovery) GetStatus() *fwapi.Status {
	return &s.Status.Status
}

// GetConditions returns the platform conditions (fwapi.ConditionsAccessor).
func (s *HardwareDiscovery) GetConditions() []fwapi.Condition {
	return s.Status.GetConditions()
}

// SetConditions replaces the platform conditions (fwapi.ConditionsAccessor).
func (s *HardwareDiscovery) SetConditions(conditions []fwapi.Condition) {
	s.Status.SetConditions(conditions)
}

// NodeSummary is the per-node inventory overview of one selected node.
type NodeSummary struct {
	// name is the Node name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// uid is the last observed Node UID.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	UID types.UID `json:"uid"`

	// plane is the allocation plane that claimed the node, omitted when
	// unknown.
	// +optional
	// +kubebuilder:validation:Enum=classic;dra
	Plane string `json:"plane,omitempty"`

	// deviceCount is the number of the service's owned HardwareDevice
	// objects for this node.
	// +kubebuilder:validation:Minimum=0
	DeviceCount int32 `json:"deviceCount"`

	// outcome is whether the last enumeration of this node was an
	// authoritative snapshot (Success, possibly empty) or unknown
	// (Unknown: error, missing API, forbidden, or incomplete pool).
	// +kubebuilder:validation:Enum=Success;Unknown
	Outcome string `json:"outcome"`
}

// +kubebuilder:object:root=true

// HardwareDiscoveryList contains a list of HardwareDiscovery.
type HardwareDiscoveryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []HardwareDiscovery `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &HardwareDiscovery{}, &HardwareDiscoveryList{})
		return nil
	})
}
