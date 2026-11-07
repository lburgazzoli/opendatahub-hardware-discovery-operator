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
	classicDeviceID  = "gpu-node-a-gpu-0"
	draSourceID      = "dra"
	classicSourceID  = "classic"
	acceleratorClass = "accelerator"
	teslaK80         = "Tesla-K80"
	collideID        = "GPU-1"
	collideIDVariant = "GPU_1"
)

// fakeLocator is a test-local Locator implementation proving the pluggable
// interface contract without depending on any source package: the core
// tests exercise locators as opaque values.
type fakeLocator struct {
	kind    string
	ref     string
	invalid error // when set, Validate fails with this error
}

func (l fakeLocator) Kind() string { return l.kind }
func (l fakeLocator) Validate() error {
	if l.invalid != nil {
		return l.invalid
	}
	return nil
}
func (l fakeLocator) Equal(other discovery.Locator) bool {
	o, ok := other.(fakeLocator)
	return ok && l == o
}
func (l fakeLocator) Ref() string { return l.ref }

// draLocator is a valid DRA locator fixture.
func draLocator() discovery.Locator {
	return fakeLocator{kind: draSourceID, ref: nvidiaDriver + "/" + nodeName}
}

// classicLocator is a valid classic labels locator fixture.
func classicLocator() discovery.Locator {
	return fakeLocator{kind: classicSourceID, ref: nodeName}
}

// validIdentity is a complete, contract-valid identity fixture (a real
// vendor UUID identity, as discovered by the DRA source).
func validIdentity() discovery.Identity {
	return discovery.Identity{
		ID:       deviceID,
		Kind:     discovery.IdentityKindUUID,
		NodeName: nodeName,
		NodeUID:  nodeUID,
		Plane:    discovery.PlaneDRA,
		Locator:  draLocator(),
	}
}

// nodeIndexIdentity is a complete, contract-valid synthetic identity
// fixture (as discovered by the classic labels source).
func nodeIndexIdentity() discovery.Identity {
	return discovery.Identity{
		ID:       classicDeviceID,
		Kind:     discovery.IdentityKindNodeIndex,
		NodeName: nodeName,
		NodeUID:  nodeUID,
		Plane:    discovery.PlaneClassic,
		Locator:  classicLocator(),
	}
}

// hashPrefix recomputes the contract's sha256 hex prefix for an input,
// independently of the implementation.
func hashPrefix(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:n]
}
