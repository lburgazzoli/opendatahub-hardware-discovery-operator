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

// Package simulator installs and observes the approved Run:ai
// fake-gpu-operator (FGO) PoC simulator on run-owned Kind clusters.
//
// Everything in this package is test-only support code: it mutates cluster
// state (node labels, simulator releases, topology inputs) that production
// discovery code must only ever read. The frozen install profiles, values
// and node labels are the approved simulator/test profile and must not be
// changed without a re-approved contract change.
package simulator

// Frozen pins and identifiers (approved simulator/test profile).
const (
	// Namespace is fixed: the FGO v0.2.1 native DRA plugin hard-codes
	// http://topology-server.gpu-operator/topology/nodes/.
	Namespace = "gpu-operator"

	// ReleaseName is the Helm release name used for the simulator.
	ReleaseName = "fake-gpu-operator"

	// ChartRepository is the released FGO chart repository.
	ChartRepository = "oci://ghcr.io/run-ai/fake-gpu-operator/fake-gpu-operator"

	// ChartDigest pins the exact reviewed chart artifact (v0.2.1,
	// content-identical to the inspected tag).
	ChartDigest = "sha256:e73fc8c0d0a375dfc452eae63b1446cd82e5a302634905acdbd31cbc5a036c11"

	// TopologyCMName is FGO's TOPOLOGY_CM_NAME: per-node ConfigMaps are
	// named <TopologyCMName>-<node>.
	TopologyCMName = "topology"

	// TopologyDataKey is the ConfigMap data key holding topology.yml.
	TopologyDataKey = "topology.yml"

	// NodePoolLabelKey assigns nodes to a simulated GPU node pool.
	NodePoolLabelKey = "run.ai/simulated-gpu-node-pool"

	// DefaultNodePool is the single pool defined by the frozen values.
	DefaultNodePool = "default"

	// DRADriver is the DRA driver name used by FGO's fake NVIDIA plugin.
	DRADriver = "gpu.nvidia.com"

	// DeviceClassName is the gpu.nvidia.com DeviceClass the chart renders
	// for the DRA profile (nvidiaDraDriver disabled).
	DeviceClassName = "gpu.nvidia.com"

	// DRAPodComponentLabel selects the dra-plugin kubelet plugin pods
	// (app.kubernetes.io/component=kubeletplugin in the FGO chart).
	DRAPodComponentLabel = "kubeletplugin"

	// DevicePluginPodComponentLabel selects the classic fake device-plugin
	// pods (component=device-plugin in the FGO chart's device-plugin
	// DaemonSet; the pod labels are app/component=device-plugin).
	DevicePluginPodComponentLabel = "device-plugin"
)

// ChartRef returns the digest-pinned chart reference.
func ChartRef() string {
	return ChartRepository + "@" + ChartDigest
}

// Profile selects one of the two frozen simulator install profiles. The
// two-plane demonstration runs them sequentially in separate run-owned
// clusters; never both in the same cluster.
type Profile string

const (
	// ProfileClassic enables the fake classic device plugin, which
	// publishes the nvidia.com/gpu extended resource on labeled nodes; the
	// classic e2e scenario additionally applies GFD-style node labels as
	// scenario data. No DRA plugin runs in this profile.
	ProfileClassic Profile = "classic"

	// ProfileDRA enables the fake native DRA plugin publishing real
	// resource.k8s.io/v1 ResourceSlices for driver gpu.nvidia.com. No
	// device plugin runs in this profile.
	ProfileDRA Profile = "dra"
)

// valuesEnabledKey is the Helm values key every FGO component uses to
// toggle itself.
const valuesEnabledKey = "enabled"

// componentValues returns the single-key values block enabling or disabling
// one simulator component.
func componentValues(enabled bool) map[string]any {
	return map[string]any{valuesEnabledKey: enabled}
}

// Values returns the exact frozen Helm values for the profile: the real
// NVIDIA stack, KWOK, compute-domain, telemetry and MIG
// machinery stay off; only the fake components the PoC needs are enabled.
func (p Profile) Values() map[string]any {
	values := map[string]any{
		"gpuOperator":            componentValues(false),
		"nvidiaDraDriver":        componentValues(false),
		"kwokDraPlugin":          componentValues(false),
		"kwokGpuDevicePlugin":    componentValues(false),
		"computeDomainDraPlugin": componentValues(false),
		"statusUpdater":          componentValues(true),
		"topologyServer":         componentValues(true),
		"topologyConfigMap":      componentValues(true),
		"statusExporter":         componentValues(false),
		"migFaker":               componentValues(false),
		"runtimeClass":           componentValues(false),
		"builtinProfiles":        componentValues(false),
		"topology": map[string]any{
			"nodePools": map[string]any{
				DefaultNodePool: map[string]any{
					"gpuProduct": "Tesla-K80",
					"gpuCount":   2,
					"gpuMemory":  11441,
				},
			},
			"nodePoolLabelKey": NodePoolLabelKey,
		},
	}

	switch p {
	case ProfileClassic:
		values["devicePlugin"] = componentValues(true)
		values["draPlugin"] = componentValues(false)
	case ProfileDRA:
		values["devicePlugin"] = componentValues(false)
		values["draPlugin"] = componentValues(true)
	default:
		return nil
	}

	return values
}

// NodeLabels returns the node labels the profile's fake components select
// on. Labels are applied by Setup to every node of the run-owned cluster.
//
// Note: the DRA profile needs the node-pool label as well as the plugin
// selector. The frozen contract lists only the dra-plugin label for DRA,
// but FGO's status-updater creates the per-node topology ConfigMaps (the
// DRA plugin's only input, served live by the topology-server) exclusively
// for nodes carrying the node-pool label — without it there is no topology
// input and the DRA plugin cannot enumerate (verified in the v0.2.1
// source, internal/status-updater/handlers/node/topology_cm.go).
func (p Profile) NodeLabels() map[string]string {
	switch p {
	case ProfileClassic:
		return map[string]string{
			NodePoolLabelKey:                      DefaultNodePool,
			"nvidia.com/gpu.deploy.device-plugin": "true",
		}
	case ProfileDRA:
		return map[string]string{
			NodePoolLabelKey:                       DefaultNodePool,
			"nvidia.com/gpu.deploy.dra-plugin-gpu": "true",
		}
	default:
		return nil
	}
}

// String implements fmt.Stringer.
func (p Profile) String() string {
	return string(p)
}
