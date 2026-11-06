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
	"errors"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
)

func TestLocatorValidate(t *testing.T) {
	g := NewWithT(t)

	g.Expect(draLocator().Validate()).To(Succeed())
	g.Expect(simulatorLocator().Validate()).To(Succeed())

	cases := []struct {
		name    string
		locator discovery.Locator
		want    error
	}{
		{"no arm", discovery.Locator{}, discovery.ErrNoLocator},
		{
			"both arms",
			discovery.Locator{DRA: draLocator().DRA, Simulator: simulatorLocator().Simulator},
			discovery.ErrAmbiguousLocator,
		},
		{
			"dra missing driver",
			discovery.Locator{DRA: &discovery.DRALocator{Pool: nodeName, Device: deviceID}},
			discovery.ErrIncompleteLocator,
		},
		{
			"simulator missing configMap",
			discovery.Locator{Simulator: &discovery.SimulatorLocator{Namespace: simulatorNS, ID: deviceID}},
			discovery.ErrIncompleteLocator,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			err := tc.locator.Validate()
			g.Expect(err).To(HaveOccurred())
			g.Expect(errors.Is(err, tc.want)).To(BeTrue(), "expected %v, got %v", tc.want, err)
		})
	}
}

func TestLocatorEqual(t *testing.T) {
	g := NewWithT(t)

	g.Expect(draLocator().Equal(draLocator())).To(BeTrue())
	g.Expect(simulatorLocator().Equal(simulatorLocator())).To(BeTrue())
	g.Expect(discovery.Locator{}.Equal(discovery.Locator{})).To(BeTrue())

	// Deep comparison: field differences and arm mismatches are changes.
	changedPool := draLocator()
	changedPool.DRA.Pool = otherNode
	g.Expect(draLocator().Equal(changedPool)).To(BeFalse())

	g.Expect(draLocator().Equal(simulatorLocator())).To(BeFalse())
	g.Expect(draLocator().Equal(discovery.Locator{})).To(BeFalse())
	g.Expect(discovery.Locator{}.Equal(draLocator())).To(BeFalse())

	// Caller-owned values are not mutated by the comparison.
	original := draLocator()
	_ = original.Equal(simulatorLocator())
	g.Expect(original).To(Equal(draLocator()))
}

func TestSourceRef(t *testing.T) {
	g := NewWithT(t)

	draRef, err := discovery.SourceRef("dra", draLocator())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(draRef).To(Equal("dra/" + nvidiaDriver + "/" + nodeName))

	simulatorRef, err := discovery.SourceRef("simulator", simulatorLocator())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(simulatorRef).To(Equal("simulator/" + simulatorNS + "/" + simulatorCM))

	_, err = discovery.SourceRef("dra", discovery.Locator{})
	g.Expect(errors.Is(err, discovery.ErrNoLocator)).To(BeTrue())

	incomplete := draLocator()
	incomplete.DRA.Device = ""
	_, err = discovery.SourceRef("dra", incomplete)
	g.Expect(errors.Is(err, discovery.ErrIncompleteLocator)).To(BeTrue())
}
