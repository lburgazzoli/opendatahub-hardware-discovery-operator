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
	"slices"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
)

func TestDeviceName(t *testing.T) {
	longID := strings.Repeat("a", 300)

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"uuid unchanged", deviceID, strings.ToLower(deviceID)},
		{"lowercased", "GPU-NODE-A", "gpu-node-a"},
		{"invalid chars folded", "GPU_NODE*1", "gpu-node-1"},
		{"separator runs collapsed", "..gpu--node..", "gpu-node"},
		{"lone dot kept", "a..b", "a.b"},
		{"mixed separators collapse", "a.-b", "a-b"},
		{"leading separators trimmed", "-x-", "x"},
		{"all invalid becomes bare hash", "???", hashPrefix("???", 8)},
		{"non ascii folded", "Ω", hashPrefix("Ω", 8)},
		{
			"overlong slug truncated and suffixed",
			longID,
			strings.Repeat("a", 244) + "-" + hashPrefix(longID, 8),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(discovery.DeviceName(tc.input)).To(Equal(tc.want))
			// Deterministic: the same input always yields the same name.
			g.Expect(discovery.DeviceName(tc.input)).To(Equal(tc.want))
		})
	}
}

func TestDeviceNameAlwaysLegal(t *testing.T) {
	g := NewWithT(t)

	// Whatever the raw identity looks like, the derived name is a legal
	// DNS-1123 subdomain (or the bare hash when nothing survives).
	for _, input := range []string{deviceID, "", "???", "---", "A_B.C", strings.Repeat("x", 400), "Ω"} {
		name := discovery.DeviceName(input)
		g.Expect(utilvalidation.IsDNS1123Subdomain(name)).To(BeEmpty(), "input %q gave %q", input, name)
		g.Expect(len(name)).To(BeNumerically("<=", 253))
	}
}

func TestDeviceNameOrderIndependent(t *testing.T) {
	g := NewWithT(t)

	inputs := []string{deviceID, "GPU_NODE*1", "..gpu--node..", longInput("z")}
	forward := map[string]string{}
	for _, input := range inputs {
		forward[input] = discovery.DeviceName(input)
	}
	backward := map[string]string{}
	for _, input := range slices.Backward(inputs) {
		backward[input] = discovery.DeviceName(input)
	}
	g.Expect(backward).To(Equal(forward))
}

func TestResolveDeviceName(t *testing.T) {
	g := NewWithT(t)

	const baseName = "gpu-1"

	// Free base name and the identity's own base name are kept as-is.
	g.Expect(discovery.ResolveDeviceName(collideID, nil)).To(Equal(baseName))
	g.Expect(discovery.ResolveDeviceName(collideID, map[string]string{baseName: collideID})).To(Equal(baseName))

	// A base name held by a different raw identity gets the deterministic
	// sha256 suffix.
	suffixed := discovery.ResolveDeviceName(collideIDVariant, map[string]string{baseName: collideID})
	g.Expect(suffixed).To(Equal(baseName + "-" + hashPrefix(collideIDVariant, 8)))
	g.Expect(suffixed).To(Equal(discovery.ResolveDeviceName(collideIDVariant, map[string]string{baseName: collideID})))
	g.Expect(suffixed).NotTo(Equal(baseName))
}

func TestResolveDeviceNameStableAcrossScans(t *testing.T) {
	g := NewWithT(t)

	// Two raw identities that sanitize to the same slug. First scan: the
	// caller assigns names in raw-id sort order, seeding the taken map as
	// objects are created (the documented caller protocol).
	ids := []string{collideID, collideIDVariant}
	slices.Sort(ids)
	firstScan := map[string]string{}
	for _, id := range ids {
		firstScan[discovery.ResolveDeviceName(id, firstScan)] = id
	}
	g.Expect(firstScan).To(HaveLen(2))
	g.Expect(firstScan["gpu-1"]).To(Equal(collideID))

	// Second scan: the taken map now reflects the existing objects, and
	// re-resolution yields the same names — no rename churn.
	secondScan := map[string]string{}
	for _, id := range ids {
		secondScan[discovery.ResolveDeviceName(id, firstScan)] = id
	}
	g.Expect(secondScan).To(Equal(firstScan))
}

func TestAmbiguousIDs(t *testing.T) {
	g := NewWithT(t)

	g.Expect(discovery.AmbiguousIDs(nil)).To(BeEmpty())

	sameNodeTwice := []discovery.Identity{validIdentity(), validIdentity()}
	g.Expect(discovery.AmbiguousIDs(sameNodeTwice)).To(Equal(map[string]struct{}{deviceID: {}}))

	onTwoNodes := []discovery.Identity{validIdentity(), atNode(validIdentity(), otherNode, otherUID)}
	g.Expect(discovery.AmbiguousIDs(onTwoNodes)).To(Equal(map[string]struct{}{deviceID: {}}))

	unique := []discovery.Identity{
		validIdentity(),
		{
			ID:       otherDeviceID,
			Kind:     discovery.IdentityKindUUID,
			NodeName: otherNode,
			NodeUID:  otherUID,
			Plane:    discovery.PlaneDRA,
			Locator:  draLocator(),
		},
	}
	g.Expect(discovery.AmbiguousIDs(unique)).To(BeEmpty())

	// The input slice is not modified.
	before := slices.Clone(onTwoNodes)
	_ = discovery.AmbiguousIDs(onTwoNodes)
	g.Expect(onTwoNodes).To(Equal(before))
}

func TestIdentityKey(t *testing.T) {
	g := NewWithT(t)

	id := validIdentity()
	g.Expect(id.Key()).To(Equal(validIdentity().Key()))

	// The host UID cannot be dropped from matching: the same id on a new
	// UID is a new incarnation, and a different id never matches.
	reincarnated := id
	reincarnated.NodeUID = otherUID
	g.Expect(reincarnated.Key()).NotTo(Equal(id.Key()))

	otherDevice := id
	otherDevice.ID = otherDeviceID
	g.Expect(otherDevice.Key()).NotTo(Equal(id.Key()))

	// Keys are usable as map keys across incarnations.
	counts := map[discovery.IdentityKey]int{
		id.Key():           1,
		reincarnated.Key(): 1,
	}
	g.Expect(counts).To(HaveLen(2))
}

func TestIdentityAugments(t *testing.T) {
	g := NewWithT(t)

	id := validIdentity()
	g.Expect(id.Augments(id.Key(), discovery.PlaneDRA)).To(BeTrue())

	// Never combine allocation planes and never transfer across
	// incarnations.
	g.Expect(id.Augments(id.Key(), discovery.PlaneClassic)).To(BeFalse())
	reincarnated := id
	reincarnated.NodeUID = otherUID
	g.Expect(id.Augments(reincarnated.Key(), discovery.PlaneDRA)).To(BeFalse())
	otherDevice := id
	otherDevice.ID = otherDeviceID
	g.Expect(otherDevice.Augments(id.Key(), discovery.PlaneDRA)).To(BeFalse())
}

func TestNodeLabelValue(t *testing.T) {
	g := NewWithT(t)

	g.Expect(discovery.NodeLabelValue(nodeName)).To(Equal(nodeName))

	// A 63-character node name is a legal label value and is kept whole.
	legal63 := strings.Repeat("n", 63)
	g.Expect(discovery.NodeLabelValue(legal63)).To(Equal(legal63))

	// Longer names use the deterministic 52+1+10 truncation.
	over64 := strings.Repeat("n", 64)
	truncated := discovery.NodeLabelValue(over64)
	g.Expect(truncated).To(Equal(strings.Repeat("n", 52) + "-" + hashPrefix(over64, 10)))
	g.Expect(len(truncated)).To(Equal(63))
	g.Expect(truncated).To(Equal(discovery.NodeLabelValue(over64)))

	// The hash covers the full node name, not the truncated prefix.
	sharesPrefix := strings.Repeat("n", 52) + strings.Repeat("m", 30)
	g.Expect(discovery.NodeLabelValue(sharesPrefix)).NotTo(Equal(truncated))
}

func TestDeviceLabels(t *testing.T) {
	g := NewWithT(t)

	labels := discovery.DeviceLabels(acceleratorClass, nodeName)
	g.Expect(labels).To(Equal(map[string]string{
		discovery.LabelDeviceClass: acceleratorClass,
		discovery.LabelNode:        nodeName,
	}))

	// A freshly allocated map per call: caller mutations never leak.
	labels[discovery.LabelNode] = "mutated"
	delete(labels, discovery.LabelDeviceClass)
	fresh := discovery.DeviceLabels(acceleratorClass, nodeName)
	g.Expect(fresh).To(Equal(map[string]string{
		discovery.LabelDeviceClass: acceleratorClass,
		discovery.LabelNode:        nodeName,
	}))

	// Long node names are truncated in the label only.
	longNode := strings.Repeat("n", 70)
	g.Expect(discovery.DeviceLabels(acceleratorClass, longNode)).To(Equal(map[string]string{
		discovery.LabelDeviceClass: acceleratorClass,
		discovery.LabelNode:        strings.Repeat("n", 52) + "-" + hashPrefix(longNode, 10),
	}))
}

// longInput builds a long raw identity fixture.
func longInput(char string) string {
	return strings.Repeat(char, 300)
}

// atNode returns a copy of the identity placed on another node.
func atNode(id discovery.Identity, name string, uid types.UID) discovery.Identity {
	id.NodeName = name
	id.NodeUID = uid
	return id
}
