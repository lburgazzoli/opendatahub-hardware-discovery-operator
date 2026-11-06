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

package discovery_test

import (
	"crypto/sha256"
	"encoding/hex"

	"k8s.io/apimachinery/pkg/types"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
)

// Named fixtures shared by the discovery contract tests. Expected hash
// values are recomputed here from the contract's sha256 rule, independent
// of the implementation's helpers.

const (
	deviceID         = "GPU-11111111-2222-3333-4444-555555555555"
	otherDeviceID    = "GPU-99999999-8888-7777-6666-555555555555"
	nodeName         = "gpu-node-a"
	nodeUID          = types.UID("11111111-1111-1111-1111-111111111111")
	otherNode        = "gpu-node-b"
	otherUID         = types.UID("22222222-2222-2222-2222-222222222222")
	nvidiaDriver     = "gpu.nvidia.com"
	simulatorNS      = "gpu-operator"
	simulatorCM      = "topology-gpu-node-a"
	acceleratorClass = "accelerator"
	teslaK80         = "Tesla-K80"
	collideID        = "GPU-1"
	collideIDVariant = "GPU_1"
)

// draLocator is a valid DRA arm fixture.
func draLocator() discovery.Locator {
	return discovery.Locator{
		DRA: &discovery.DRALocator{
			Driver: nvidiaDriver,
			Pool:   nodeName,
			Device: deviceID,
		},
	}
}

// simulatorLocator is a valid simulator arm fixture.
func simulatorLocator() discovery.Locator {
	return discovery.Locator{
		Simulator: &discovery.SimulatorLocator{
			Namespace: simulatorNS,
			ConfigMap: simulatorCM,
			ID:        deviceID,
		},
	}
}

// validIdentity is a complete, contract-valid identity fixture.
func validIdentity() discovery.Identity {
	return discovery.Identity{
		ID:       deviceID,
		NodeName: nodeName,
		NodeUID:  nodeUID,
		Plane:    discovery.PlaneDRA,
		Locator:  draLocator(),
	}
}

// hashPrefix recomputes the contract's sha256 hex prefix for an input,
// independently of the implementation.
func hashPrefix(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:n]
}
