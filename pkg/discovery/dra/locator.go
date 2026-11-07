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
	"fmt"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
)

// Locator locates a device in a stable resource.k8s.io/v1 ResourceSlice
// advertisement (design doc §4.1/§8): the allow-listed driver, the pool
// name and the device name within the pool. Devices located this way
// carry a real vendor identity (identity.kind uuid).
type Locator struct {
	// Driver is the DRA driver that advertised the device (for example
	// gpu.nvidia.com).
	Driver string
	// Pool is the ResourceSlice pool.name the device was advertised in.
	Pool string
	// Device is the ResourceSlice device.name of this device.
	Device string
}

// Kind identifies the DRA source (discovery.Locator interface).
func (l Locator) Kind() string { return SourceID }

// Validate checks the locator's required fields.
func (l Locator) Validate() error {
	if l.Driver == "" || l.Pool == "" || l.Device == "" {
		return fmt.Errorf("%w: dra locator (driver %q, pool %q, device %q)",
			discovery.ErrIncompleteLocator, l.Driver, l.Pool, l.Device)
	}
	return nil
}

// Equal reports deep equality; it never equals a locator of another kind.
func (l Locator) Equal(other discovery.Locator) bool {
	o, ok := other.(Locator)
	return ok && l == o
}

// Ref renders the locator's provenance suffix: "<driver>/<pool>".
func (l Locator) Ref() string { return l.Driver + "/" + l.Pool }
