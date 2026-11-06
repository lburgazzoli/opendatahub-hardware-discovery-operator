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

	// simulator configures the opt-in, PoC-only fake-gpu-operator classic
	// source adapter. It is disabled by default.
	// +optional
	Simulator *SimulatorSourceConfig `json:"simulator,omitempty"`
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

// SimulatorSourceConfig configures the opt-in classic PoC source adapter that
// reads the fake-gpu-operator per-node topology ConfigMaps.
// +kubebuilder:validation:XValidation:rule="!(has(self.enabled) && self.enabled) || (has(self.namespace) && size(self.namespace) > 0)",message="namespace is required when the simulator source is enabled"
type SimulatorSourceConfig struct {
	// enabled turns the simulator source on. It is opt-in and defaults to
	// false: it demonstrates discovery control flow, not production classic
	// hardware discovery.
	// +kubebuilder:default=false
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// namespace is the only namespace whose ConfigMaps the adapter reads.
	// It is required when the simulator source is enabled.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// configMapPrefix is the fake-gpu-operator topology ConfigMap name
	// prefix (the chart's TOPOLOGY_CM_NAME).
	// +kubebuilder:default=topology
	// +optional
	ConfigMapPrefix string `json:"configMapPrefix,omitempty"`
}

// HardwareDiscoveryStatus is the observed state of the singleton: collector
// health and a compact per-node inventory overview. Conditions describe the
// collector and its observations, never hardware health or free allocation
// capacity.
type HardwareDiscoveryStatus struct {
	// observedGeneration is the generation of the spec most recently
	// observed by the singleton controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions describe collector/observation health. Reasons include
	// Reconciled, SourcesUnavailable, SourceForbidden, PoolIncomplete,
	// NameCollision, IdentityAmbiguous and InvalidConfiguration.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// nodeSummaries is a compact per-selected-node inventory overview.
	// +optional
	// +listType=map
	// +listMapKey=name
	NodeSummaries []NodeSummary `json:"nodeSummaries,omitempty"`
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
