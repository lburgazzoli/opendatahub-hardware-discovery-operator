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

package dra

import (
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// The approved NVIDIA whole-GPU attribute contract, verified against the
// pinned fake-gpu-operator v0.2.1 dra-plugin.
// Qualified gpu.nvidia.com/* keys are primary; the unqualified uuid/model
// keys are the v0.2.1 back-compat forms and are read exactly as emitted,
// never guessed from device names or counts.
const (
	// attrType is the whole-GPU discriminator attribute.
	attrType = "gpu.nvidia.com/type"

	// attrUUID is the primary vendor device identity attribute.
	attrUUID = "gpu.nvidia.com/uuid"

	// attrProductName is the primary device model attribute.
	attrProductName = "gpu.nvidia.com/productName"

	// attrUUIDFallback is the v0.2.1 back-compat identity key, read
	// exactly as emitted when the qualified key is absent.
	attrUUIDFallback = "uuid"

	// attrProductFallback is the v0.2.1 back-compat model key.
	attrProductFallback = "model"

	// attrTypeGPU is the only accepted whole-GPU type value.
	attrTypeGPU = "gpu"

	// capacityMemory is the device memory capacity key, in bytes.
	capacityMemory = "memory"
)

// isWholeGPU reports whether the device matches the approved whole-GPU
// discriminator: gpu.nvidia.com/type == "gpu". MIG instances, VFIO and
// other partition alternatives publish different types and are skipped,
// never counted as physical cards.
func isWholeGPU(device resourcev1.Device) bool {
	return stringValue(device.Attributes[attrType]) == attrTypeGPU
}

// deviceUUID extracts the device's vendor identity: the qualified
// gpu.nvidia.com/uuid attribute, falling back to the v0.2.1 unqualified
// uuid key exactly as emitted. An empty result means the device has no
// usable identity and is rejected — a UUID is never derived from the
// device name, a count or an index.
func deviceUUID(device resourcev1.Device) string {
	if uuid := stringValue(device.Attributes[attrUUID]); uuid != "" {
		return uuid
	}
	return stringValue(device.Attributes[attrUUIDFallback])
}

// deviceModel extracts the device model: the qualified productName
// attribute, falling back to the v0.2.1 unqualified model key. Absent
// stays absent.
func deviceModel(device resourcev1.Device) string {
	if model := stringValue(device.Attributes[attrProductName]); model != "" {
		return model
	}
	return stringValue(device.Attributes[attrProductFallback])
}

// deviceMemory extracts the device memory capacity in bytes. Absent
// stays absent; the value is never inferred from another capacity.
func deviceMemory(device resourcev1.Device) *resource.Quantity {
	capacity, ok := device.Capacity[capacityMemory]
	if !ok {
		return nil
	}
	value := capacity.Value
	return &value
}

// stringValue reads a string-valued attribute; attributes of other value
// types are not string data and read as absent.
func stringValue(attr resourcev1.DeviceAttribute) string {
	if attr.StringValue == nil {
		return ""
	}
	return *attr.StringValue
}
