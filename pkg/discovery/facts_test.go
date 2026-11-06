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
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
)

func TestFactsEqual(t *testing.T) {
	cases := []struct {
		name  string
		a     discovery.Facts
		b     discovery.Facts
		equal bool
	}{
		{"identical full facts", fullFacts(), fullFacts(), true},
		{"both memory absent", discovery.Facts{Model: teslaK80}, discovery.Facts{Model: teslaK80}, true},
		{
			// Absent is never zero: an omitted fact is not a published
			// zero-sized one.
			"absent vs zero memory",
			discovery.Facts{Model: teslaK80},
			discovery.Facts{Model: teslaK80, Memory: quantity("0")},
			false,
		},
		{
			// Equal values in different Quantity formats are equal.
			"memory formats",
			discovery.Facts{Memory: quantity("12Gi")},
			discovery.Facts{Memory: quantity("12884901888")},
			true,
		},
		{"different memory", fullFacts(), withMemory(fullFacts(), "24Gi"), false},
		{"different model", fullFacts(), withModel(fullFacts(), "Tesla-V100"), false},
		{"different vendor", fullFacts(), withVendor(fullFacts(), "AMD"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(tc.a.Equal(tc.b)).To(Equal(tc.equal))
			g.Expect(tc.b.Equal(tc.a)).To(Equal(tc.equal))
		})
	}
}

func TestFactsEqualDoesNotMutateCallerValues(t *testing.T) {
	g := NewWithT(t)

	owned := quantity("12Gi")
	facts := discovery.Facts{Memory: owned}
	_ = facts.Equal(discovery.Facts{Memory: quantity("12884901888")})

	// The caller-owned Quantity keeps its value and string form.
	g.Expect(owned.String()).To(Equal("12Gi"))
	g.Expect(facts.Memory).To(Equal(owned))
}

func TestObservationEqual(t *testing.T) {
	stamped := time.Date(2026, 11, 6, 15, 32, 31, 0, time.UTC)
	base := discovery.Observation{
		Identity:    validIdentity(),
		Facts:       fullFacts(),
		CollectedAt: stamped,
	}

	t.Run("collection time is excluded", func(t *testing.T) {
		g := NewWithT(t)
		later := base
		later.CollectedAt = stamped.Add(10 * time.Minute)
		g.Expect(discovery.ObservationEqual(base, later)).To(BeTrue())
		g.Expect(discovery.ObservationEqual(base, base)).To(BeTrue())
	})

	t.Run("factual and locator changes are detected", func(t *testing.T) {
		g := NewWithT(t)

		changedFacts := base
		changedFacts.Facts = withMemory(fullFacts(), "24Gi")
		g.Expect(discovery.ObservationEqual(base, changedFacts)).To(BeFalse())

		changedPlane := base
		changedPlane.Identity.Plane = discovery.PlaneClassic
		g.Expect(discovery.ObservationEqual(base, changedPlane)).To(BeFalse())

		changedLocator := base
		changedLocator.Identity.Locator = draLocator()
		changedLocator.Identity.Locator.DRA.Pool = otherNode
		g.Expect(discovery.ObservationEqual(base, changedLocator)).To(BeFalse())

		changedArm := base
		changedArm.Identity.Locator = simulatorLocator()
		g.Expect(discovery.ObservationEqual(base, changedArm)).To(BeFalse())

		reincarnated := base
		reincarnated.Identity.NodeUID = otherUID
		g.Expect(discovery.ObservationEqual(base, reincarnated)).To(BeFalse())

		otherDevice := base
		otherDevice.Identity.ID = otherDeviceID
		g.Expect(discovery.ObservationEqual(base, otherDevice)).To(BeFalse())
	})
}

// fullFacts is a fully populated facts fixture.
func fullFacts() discovery.Facts {
	return discovery.Facts{Vendor: "NVIDIA", Model: teslaK80, Memory: quantity("12635713536")}
}

// withMemory returns a copy of f with a different memory fixture.
func withMemory(f discovery.Facts, s string) discovery.Facts {
	f.Memory = quantity(s)
	return f
}

// withModel returns a copy of f with a different model.
func withModel(f discovery.Facts, model string) discovery.Facts {
	f.Model = model
	return f
}

// withVendor returns a copy of f with a different vendor.
func withVendor(f discovery.Facts, vendor string) discovery.Facts {
	f.Vendor = vendor
	return f
}
