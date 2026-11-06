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

package v1alpha1_test

// Shared fixture values from the approved example CRs
// (docs/design/poc-contract.md §4.3/§5.3). Tests deliberately hard-code
// expectations instead of sharing production constants.
const (
	deviceKind        = "HardwareDevice"
	discoveryKind     = "HardwareDiscovery"
	apiVersion        = "infrastructure.platform.opendatahub.io/v1alpha1"
	acceleratorClass  = "accelerator"
	deviceID          = "GPU-11111111-2222-3333-4444-555555555555"
	deviceName        = "gpu-11111111-2222-3333-4444-555555555555"
	nodeName          = "gpu-node-a"
	nodeUID           = "11111111-1111-1111-1111-111111111111"
	nvidiaDriver      = "gpu.nvidia.com"
	deviceClassLabel  = "hardwarediscovery.platform.opendatahub.io/device-class"
	nodeLabel         = "hardwarediscovery.platform.opendatahub.io/node"
	nodePoolLabelKey  = "run.ai/simulated-gpu-node-pool"
	nodePoolLabelVal  = "default"
	simulatorSourceNS = "gpu-operator"
)
