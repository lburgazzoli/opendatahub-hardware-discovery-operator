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

// Locator is the pluggable, source-owned locator stored in spec.source
// (design doc §7, amendments 2+3). Each source package implements it for
// its own locator shape; the core never enumerates source kinds. The
// implementation's Kind must match the spec.source.kind arm that carries
// the locator, and source implementations are expected to map their
// locator to and from that arm themselves.
type Locator interface {
	// Kind is the registered source kind that owns this locator (for
	// example "dra" or "classic").
	Kind() string
	// Validate checks the locator's required fields.
	Validate() error
	// Equal reports deep equality with another locator. It never mutates
	// the caller's values; locators of different kinds are never equal.
	Equal(Locator) bool
	// Ref renders the locator's provenance suffix (design doc §4.2), for
	// example "gpu.nvidia.com/<pool>" or "<node>".
	Ref() string
}

// Locator validation sentinels. Source implementations own their Validate
// methods and are expected to wrap these sentinels so callers can classify
// results with errors.Is.
var (
	// ErrNoLocator reports a missing locator: every identity carries one.
	ErrNoLocator = errors.New("discovery: source locator is missing")

	// ErrIncompleteLocator reports a locator missing a required field.
	ErrIncompleteLocator = errors.New("discovery: source locator is missing a required field")
)

// SourceRef builds the provenance reference for a source and locator
// (design doc §4.2/§7): "<source-id>/<locator ref>" — for example
// "dra/gpu.nvidia.com/gpu-node-a" or "classic/gpu-node-a". The reference is
// replaced, never merged, on an observed source change.
func SourceRef(sourceID string, l Locator) (string, error) {
	if l == nil {
		return "", ErrNoLocator
	}
	if err := l.Validate(); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s", sourceID, l.Ref()), nil
}
