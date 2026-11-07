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

package classic

import (
	"fmt"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
)

// DeviceSource renders the locator as the spec.source classic arm (design
// doc §7): the source package owns the mapping between its locator shape
// and the API arm, so the controllers never interpret locator internals.
func (l Locator) DeviceSource() v1alpha1.DeviceSource {
	return v1alpha1.DeviceSource{
		Kind: v1alpha1.DeviceSourceKindClassic,
		Classic: &v1alpha1.ClassicSource{
			Node:  l.Node,
			Index: l.Index,
		},
	}
}

// LocatorFromDeviceSource extracts the classic locator from a spec.source
// value. It rejects sources of other kinds and invalid classic arms
// instead of guessing.
func LocatorFromDeviceSource(src v1alpha1.DeviceSource) (Locator, error) {
	if src.Kind != v1alpha1.DeviceSourceKindClassic || src.Classic == nil {
		return Locator{}, fmt.Errorf("classic: spec.source is not a %s locator (kind %q)", SourceID, src.Kind)
	}
	locator := Locator{Node: src.Classic.Node, Index: src.Classic.Index}
	if err := locator.Validate(); err != nil {
		return Locator{}, err
	}
	return locator, nil
}
