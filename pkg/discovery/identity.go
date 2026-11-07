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
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
)

// Service-written label keys. The prefix belongs to this
// service only; every label below it is written by the singleton
// controller and never by a consumer.
const (
	// LabelPrefix is the prefix of every service-written label.
	LabelPrefix = "hardwarediscovery.platform.opendatahub.io"

	// LabelDeviceClass mirrors spec.class for consumer selection. Its
	// value is an enum, legal as a label value by construction.
	LabelDeviceClass = LabelPrefix + "/device-class"

	// LabelNode indexes the hosting node. It is an index only, never an
	// identity source; the full node name always remains in
	// spec.host.name.
	LabelNode = LabelPrefix + "/node"
)

// Name and label derivation limits.
const (
	// maxSubdomainLength is the DNS-1123 subdomain length limit for
	// derived object names.
	maxSubdomainLength = 253

	// nameHashLength is the length of the sha256 hex suffix appended to
	// colliding or non-legal derived object names.
	nameHashLength = 8

	// nameHashSuffixLength is the full length of the "-" plus hash suffix.
	nameHashSuffixLength = 1 + nameHashLength

	// nodeLabelPrefixLength is the kept prefix length of an over-long node
	// name in the node label value.
	nodeLabelPrefixLength = 52

	// nodeLabelHashLength is the length of the sha256 hex suffix of the
	// node label value.
	nodeLabelHashLength = 10
)

// IdentityKey is the deterministic matching key of one device incarnation:
// the device id — vendor UUID or node-scoped synthetic id — paired with
// the hosting Node UID. The host UID cannot be dropped from matching: the
// same id observed on a new Node UID is a new incarnation, never an
// in-place transfer.
type IdentityKey struct {
	// ID is the device id (vendor UUID or node-scoped synthetic id).
	ID string
	// NodeUID is the UID of the hosting Node incarnation.
	NodeUID types.UID
}

// Key returns the matching key of the identity.
func (i Identity) Key() IdentityKey {
	return IdentityKey{ID: i.ID, NodeUID: i.NodeUID}
}

// Augments reports whether this observed identity may augment the device
// incarnation identified by key on plane: it must name the same proven
// identity — the same device id and the same host UID — and the same
// allocation plane. Sources may augment the same proven identity only and
// never combine allocation planes.
func (i Identity) Augments(key IdentityKey, plane Plane) bool {
	return i.Key() == key && i.Plane == plane
}

// DeviceName derives the deterministic HardwareDevice object name for the
// device identity id: the id lowercased, invalid
// characters folded into collapsed separators, leading and trailing
// separators trimmed. When the slug is not a legal DNS-1123 subdomain —
// for example because it exceeds 253 characters or sanitizes to nothing —
// the deterministic sha256 suffix is appended, truncating the slug as
// needed. The name is a function of the identity id only, never of the
// host, and the caller never invents UUIDs.
func DeviceName(id string) string {
	s := slugify(id)
	if s != "" && len(s) <= maxSubdomainLength && len(utilvalidation.IsDNS1123Subdomain(s)) == 0 {
		return s
	}
	return withHashSuffix(s, id)
}

// ResolveDeviceName returns the object name to use for the device identity
// id given the object names already taken (name → identity id of the
// holder). It returns DeviceName(id) unless that name is held by a
// different raw identity, in which case the deterministic sha256
// disambiguation suffix is appended: two raw identities
// that sanitize to the same slug never share an object.
//
// Callers should match existing devices by Identity.Key first and derive
// names only for identities without an existing object, seeding taken with
// every existing and newly assigned name: the same inputs then always
// produce the same names, so re-resolution never renames devices. A
// derived name held by a foreign object is a controller-level
// NameCollision, never adoption; this function only disambiguates
// identities the service itself owns.
func ResolveDeviceName(id string, taken map[string]string) string {
	base := DeviceName(id)
	if holder, ok := taken[base]; !ok || holder == id {
		return base
	}
	return withHashSuffix(base, id)
}

// AmbiguousIDs returns the device ids that appear more than once among
// identities — the same id discovered twice, or the same id on two
// simultaneously live nodes. Per the approved contract, every identity
// carrying an ambiguous id is rejected (IdentityAmbiguous): one is never
// silently picked, and duplicates are never resolved by list order or
// index. The input is not modified.
func AmbiguousIDs(ids []Identity) map[string]struct{} {
	counts := make(map[string]int, len(ids))
	for _, id := range ids {
		counts[id.ID]++
	}
	ambiguous := make(map[string]struct{})
	for id, n := range counts {
		if n > 1 {
			ambiguous[id] = struct{}{}
		}
	}
	return ambiguous
}

// DeviceLabels returns the deterministic, service-written label set of a
// HardwareDevice of the given class on the given node.
// The returned map is freshly allocated; callers may keep or modify it
// without affecting later calls. class is an enum value, legal as a label
// value by construction.
func DeviceLabels(class, node string) map[string]string {
	return map[string]string{
		LabelDeviceClass: class,
		LabelNode:        NodeLabelValue(node),
	}
}

// NodeLabelValue returns the deterministic value of the service-written
// node label for a node name: the full node name when it
// is a legal label value, otherwise "<first-52-chars>-<sha256[:10]>" (63
// characters total). The truncation is deterministic and unit-tested; the
// label is an index, never an identity source, and the full node name
// always remains in spec.host.name. Node names are DNS-1123 subdomains,
// so over-length is the only illegal case in practice.
func NodeLabelValue(node string) string {
	if len(utilvalidation.IsValidLabelValue(node)) == 0 {
		return node
	}
	prefix := node[:min(len(node), nodeLabelPrefixLength)]
	prefix = strings.TrimRight(prefix, "-_.")
	if prefix == "" {
		return hashHex(node, nodeLabelHashLength)
	}
	return prefix + "-" + hashHex(node, nodeLabelHashLength)
}

// slugify lowercases s and folds every rune that is not a lowercase ASCII
// alphanumeric, '-' or '.' into the separator run; runs of adjacent
// separators collapse to a single '-' (a lone '.' between alphanumerics
// survives) and leading and trailing separators are trimmed. A non-empty
// result therefore always matches the DNS-1123 subdomain shape, up to
// length.
func slugify(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	var pending byte // pending separator: 0, '-' or '.'
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			if pending != 0 && b.Len() > 0 {
				b.WriteByte(pending)
			}
			pending = 0
			b.WriteRune(r)
		case r == '-' || r == '.':
			if pending == 0 {
				pending = byte(r)
			} else if pending != '.' || r != '.' {
				pending = '-'
			}
		default:
			pending = '-'
		}
	}
	return b.String()
}

// withHashSuffix appends "-<sha256(id) hex prefix>" to name, truncating
// name as needed so the result stays a legal DNS-1123 subdomain. A name
// that sanitizes to nothing becomes the bare hash.
func withHashSuffix(name, id string) string {
	hash := hashHex(id, nameHashLength)
	name = name[:min(len(name), maxSubdomainLength-nameHashSuffixLength)]
	name = strings.TrimRight(name, "-.")
	if name == "" {
		return hash
	}
	return name + "-" + hash
}

// hashHex returns the first n hex characters of the sha256 digest of s.
func hashHex(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:n]
}
