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

package discovery

import (
	"errors"
	"fmt"
)

// Locator is the frozen, source-owned union stored verbatim in spec.source
// (design doc §7). Exactly one arm is set; source refs identify real input
// objects, never an inferred registry.
type Locator struct {
	DRA       *DRALocator       `json:"dra,omitempty"`
	Simulator *SimulatorLocator `json:"simulator,omitempty"`
}

// DRALocator locates a device in a stable resource.k8s.io/v1 ResourceSlice
// advertisement.
type DRALocator struct {
	// Driver is the DRA driver that advertised the device (for example
	// gpu.nvidia.com).
	Driver string
	// Pool is the ResourceSlice pool.name the device was advertised in.
	Pool string
	// Device is the ResourceSlice device.name of this device.
	Device string
}

// SimulatorLocator locates a device in the Run:ai fake-gpu-operator
// per-node topology ConfigMap. It exists only for the opt-in, PoC-only
// classic simulator adapter.
type SimulatorLocator struct {
	// Namespace is the only namespace whose ConfigMaps the adapter reads.
	Namespace string
	// ConfigMap is the per-node topology ConfigMap name
	// (topology-<node> by convention).
	ConfigMap string
	// ID is the synthetic device id inside the ConfigMap; it always
	// equals the identity id.
	ID string
}

// Locator validation sentinels.
var (
	// ErrNoLocator reports a locator with no arm set.
	ErrNoLocator = errors.New("discovery: source locator has no arm set")

	// ErrAmbiguousLocator reports a locator with more than one arm set.
	ErrAmbiguousLocator = errors.New("discovery: source locator has more than one arm set")

	// ErrIncompleteLocator reports a locator arm missing a required
	// field.
	ErrIncompleteLocator = errors.New("discovery: source locator is missing a required field")
)

// Validate checks the exactly-one-arm invariant and the required arm
// fields.
func (l Locator) Validate() error {
	switch {
	case l.DRA == nil && l.Simulator == nil:
		return ErrNoLocator
	case l.DRA != nil && l.Simulator != nil:
		return ErrAmbiguousLocator
	case l.DRA != nil:
		if l.DRA.Driver == "" || l.DRA.Pool == "" || l.DRA.Device == "" {
			return fmt.Errorf("%w: dra locator", ErrIncompleteLocator)
		}
	default:
		if l.Simulator.Namespace == "" || l.Simulator.ConfigMap == "" || l.Simulator.ID == "" {
			return fmt.Errorf("%w: simulator locator", ErrIncompleteLocator)
		}
	}
	return nil
}

// Equal reports deep equality of two locators. It never mutates the
// caller-owned values.
func (l Locator) Equal(other Locator) bool {
	switch {
	case l.DRA != nil || other.DRA != nil:
		if l.DRA == nil || other.DRA == nil {
			return false
		}
		return *l.DRA == *other.DRA
	case l.Simulator != nil || other.Simulator != nil:
		if l.Simulator == nil || other.Simulator == nil {
			return false
		}
		return *l.Simulator == *other.Simulator
	default:
		return true
	}
}

// SourceRef builds the provenance reference for a source and locator
// (design doc §4.2/§7): "<id>/<driver>/<pool>" for a DRA locator and
// "<id>/<namespace>/<configMap>" for a simulator locator — for example
// "dra/gpu.nvidia.com/gpu-node-a" or
// "simulator/gpu-operator/topology-gpu-node-a". The reference is replaced,
// never merged, on an observed source change.
func SourceRef(sourceID string, l Locator) (string, error) {
	if err := l.Validate(); err != nil {
		return "", err
	}
	if l.DRA != nil {
		return fmt.Sprintf("%s/%s/%s", sourceID, l.DRA.Driver, l.DRA.Pool), nil
	}
	return fmt.Sprintf("%s/%s/%s", sourceID, l.Simulator.Namespace, l.Simulator.ConfigMap), nil
}
