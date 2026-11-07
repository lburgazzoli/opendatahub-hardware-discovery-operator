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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
)

// Locator locates a device in the classic labels input of its hosting
// node: the Node whose GFD-style labels
// describe the device, plus the device's index within the node's
// published gpu.count. Devices located this way carry a synthetic
// node-scoped identity (identity.kind nodeIndex).
type Locator struct {
	// Node is the name of the Node whose labels describe the device.
	Node string
	// Index is the device's index within the node's gpu.count; stable for
	// the node incarnation.
	Index int32
}

// Kind identifies the classic source (discovery.Locator interface).
func (l Locator) Kind() string { return SourceID }

// Validate checks the locator's required fields.
func (l Locator) Validate() error {
	if l.Node == "" || l.Index < 0 {
		return fmt.Errorf("%w: classic locator (node %q, index %d)", discovery.ErrIncompleteLocator, l.Node, l.Index)
	}
	return nil
}

// Equal reports deep equality; it never equals a locator of another kind.
func (l Locator) Equal(other discovery.Locator) bool {
	o, ok := other.(Locator)
	return ok && l == o
}

// Ref renders the locator's provenance suffix: the node name.
func (l Locator) Ref() string { return l.Node }

// Identity id derivation limits: spec.identity.id is
// capped at 63 characters.
const (
	// MaxIDLength is the maximum length of a derived identity id.
	MaxIDLength = 63

	// idHashLength is the length of the sha256 hex suffix of a truncated
	// identity id.
	idHashLength = 8
)

// DeviceID derives the synthetic node-scoped identity id of a device:
// "<node>-gpu-<index>". The id is
// deterministic for the node incarnation, and when the node name is too
// long it is hash-truncated to stay within MaxIDLength: the kept node
// prefix is followed by "-gpu-<index>-<sha256[:8]>" of the full
// untruncated form. The id is an index within the node incarnation —
// never a vendor identity — and is labeled as such via
// discovery.IdentityKindNodeIndex.
func DeviceID(node string, index int32) string {
	base := fmt.Sprintf("%s-gpu-%d", node, index)
	if len(base) <= MaxIDLength {
		return base
	}
	tail := fmt.Sprintf("-gpu-%d-%s", index, hashPrefix(base, idHashLength))
	prefix := strings.TrimRight(node[:min(len(node), MaxIDLength-len(tail))], "-.")
	if prefix == "" {
		return hashPrefix(base, MaxIDLength)
	}
	return prefix + tail
}

// hashPrefix returns the first n hex characters of the sha256 digest of s.
func hashPrefix(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:n]
}
