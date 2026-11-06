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
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
)

func TestPlaneAndOutcomeValues(t *testing.T) {
	g := NewWithT(t)

	g.Expect(string(discovery.PlaneClassic)).To(Equal("classic"))
	g.Expect(string(discovery.PlaneDRA)).To(Equal("dra"))
	g.Expect(discovery.PlaneClassic.Valid()).To(BeTrue())
	g.Expect(discovery.PlaneDRA.Valid()).To(BeTrue())
	g.Expect(discovery.Plane("wire").Valid()).To(BeFalse())

	// Success and Unknown are the only outcomes, and they are distinct:
	// an authoritative empty snapshot never equals an unknown read.
	g.Expect(string(discovery.OutcomeSuccess)).To(Equal("Success"))
	g.Expect(string(discovery.OutcomeUnknown)).To(Equal("Unknown"))
	g.Expect(discovery.OutcomeSuccess).NotTo(Equal(discovery.OutcomeUnknown))
}

func TestNodeInfoValidate(t *testing.T) {
	g := NewWithT(t)

	g.Expect(discovery.NodeInfo{Name: nodeName, UID: nodeUID}.Validate()).To(Succeed())
	g.Expect(discovery.NodeInfo{Name: nodeName}.Validate()).NotTo(Succeed())
	g.Expect(discovery.NodeInfo{UID: nodeUID}.Validate()).NotTo(Succeed())

	err := discovery.NodeInfo{}.Validate()
	g.Expect(errors.Is(err, discovery.ErrMissingNode)).To(BeTrue())
}

func TestIdentityValidate(t *testing.T) {
	g := NewWithT(t)

	g.Expect(validIdentity().Validate()).To(Succeed())

	cases := []struct {
		name     string
		mutate   func(*discovery.Identity)
		expected error
	}{
		{"missing id", func(i *discovery.Identity) { i.ID = "" }, discovery.ErrMissingID},
		{"missing node name", func(i *discovery.Identity) { i.NodeName = "" }, discovery.ErrMissingNode},
		{"missing node uid", func(i *discovery.Identity) { i.NodeUID = "" }, discovery.ErrMissingNode},
		{"unknown plane", func(i *discovery.Identity) { i.Plane = "wire" }, discovery.ErrUnknownPlane},
		{"no locator", func(i *discovery.Identity) { i.Locator = discovery.Locator{} }, discovery.ErrNoLocator},
		{
			"ambiguous locator",
			func(i *discovery.Identity) { i.Locator.Simulator = simulatorLocator().Simulator },
			discovery.ErrAmbiguousLocator,
		},
		{
			"incomplete dra locator",
			func(i *discovery.Identity) { i.Locator.DRA.Pool = "" },
			discovery.ErrIncompleteLocator,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			id := validIdentity()
			tc.mutate(&id)
			err := id.Validate()
			g.Expect(err).To(MatchError(ContainSubstring("discovery:")))
			g.Expect(errors.Is(err, tc.expected)).To(BeTrue(), "expected %v, got %v", tc.expected, err)
		})
	}
}

func TestFactsValidate(t *testing.T) {
	g := NewWithT(t)

	// Absent facts are omitted: all-absent is a valid, empty observation.
	g.Expect(discovery.Facts{}.Validate()).To(Succeed())
	g.Expect(discovery.Facts{Model: teslaK80}.Validate()).To(Succeed())
	g.Expect(discovery.Facts{Memory: quantity("12635713536")}.Validate()).To(Succeed())

	// A published memory of zero violates absent-never-zero.
	g.Expect(errors.Is(discovery.Facts{Memory: quantity("0")}.Validate(), discovery.ErrZeroMemory)).To(BeTrue())
	g.Expect(errors.Is(discovery.Facts{Memory: quantity("0Gi")}.Validate(), discovery.ErrZeroMemory)).To(BeTrue())
}

func TestEnumerationValidate(t *testing.T) {
	g := NewWithT(t)

	cause := errors.New("incomplete pool")

	// Empty + Success is authoritative; Success never carries a cause.
	empty := discovery.Enumeration{Outcome: discovery.OutcomeSuccess}
	g.Expect(empty.Validate()).To(Succeed())
	g.Expect(empty.Authoritative()).To(BeTrue())

	withIDs := discovery.Enumeration{
		Outcome:    discovery.OutcomeSuccess,
		Identities: []discovery.Identity{validIdentity()},
	}
	g.Expect(withIDs.Validate()).To(Succeed())
	g.Expect(withIDs.Authoritative()).To(BeTrue())

	// Unknown carries a classified cause and never a partial hardware set.
	unknown := discovery.Enumeration{Outcome: discovery.OutcomeUnknown, Err: cause}
	g.Expect(unknown.Validate()).To(Succeed())
	g.Expect(unknown.Authoritative()).To(BeFalse())

	cases := []struct {
		name string
		enum discovery.Enumeration
		want error
	}{
		{
			"success with cause",
			discovery.Enumeration{Outcome: discovery.OutcomeSuccess, Err: cause},
			discovery.ErrSuccessWithCause,
		},
		{"unknown without cause", discovery.Enumeration{Outcome: discovery.OutcomeUnknown}, discovery.ErrUnknownWithoutCause},
		{
			"unknown with partial identities",
			discovery.Enumeration{
				Outcome:    discovery.OutcomeUnknown,
				Err:        cause,
				Identities: []discovery.Identity{validIdentity()},
			},
			discovery.ErrPartialEnumeration,
		},
		{"unset outcome", discovery.Enumeration{}, discovery.ErrInvalidOutcome},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			err := tc.enum.Validate()
			g.Expect(err).To(HaveOccurred())
			g.Expect(errors.Is(err, tc.want)).To(BeTrue(), "expected %v, got %v", tc.want, err)
		})
	}
}

func TestObservationValidate(t *testing.T) {
	g := NewWithT(t)

	stamped := time.Date(2026, 11, 6, 15, 32, 31, 0, time.UTC)
	observation := discovery.Observation{
		Identity:    validIdentity(),
		Facts:       discovery.Facts{Model: teslaK80, Memory: quantity("12635713536")},
		CollectedAt: stamped,
	}
	g.Expect(observation.Validate()).To(Succeed())

	unstamped := observation
	unstamped.CollectedAt = time.Time{}
	g.Expect(errors.Is(unstamped.Validate(), discovery.ErrUnstampedObservation)).To(BeTrue())

	zeroMemory := observation
	zeroMemory.Facts.Memory = quantity("0")
	g.Expect(errors.Is(zeroMemory.Validate(), discovery.ErrZeroMemory)).To(BeTrue())

	noIdentity := observation
	noIdentity.Identity.ID = ""
	g.Expect(errors.Is(noIdentity.Validate(), discovery.ErrMissingID)).To(BeTrue())
}

// recordingSource is a minimal fake Source proving the frozen contract is
// implementable exactly as documented, with the context propagated.
type recordingSource struct {
	enumerated []discovery.NodeInfo
	observed   []discovery.Identity
}

var _ discovery.Source = (*recordingSource)(nil)

func (s *recordingSource) ID() string             { return "dra" }
func (s *recordingSource) Plane() discovery.Plane { return discovery.PlaneDRA }

func (s *recordingSource) Enumerate(
	ctx context.Context,
	nodes []discovery.NodeInfo,
) (discovery.Enumeration, error) {
	if err := ctx.Err(); err != nil {
		return discovery.Enumeration{}, err
	}
	s.enumerated = append(s.enumerated, nodes...)
	identities := make([]discovery.Identity, 0, len(nodes))
	for _, node := range nodes {
		identities = append(identities, discovery.Identity{
			ID:       deviceID,
			NodeName: node.Name,
			NodeUID:  node.UID,
			Plane:    discovery.PlaneDRA,
			Locator:  draLocator(),
		})
	}
	return discovery.Enumeration{Outcome: discovery.OutcomeSuccess, Identities: identities}, nil
}

func (s *recordingSource) Observe(
	ctx context.Context,
	id discovery.Identity,
) (discovery.Observation, error) {
	if err := ctx.Err(); err != nil {
		return discovery.Observation{}, err
	}
	s.observed = append(s.observed, id)
	return discovery.Observation{
		Identity:    id,
		Facts:       discovery.Facts{Model: teslaK80, Memory: quantity("12635713536")},
		CollectedAt: time.Date(2026, 11, 6, 15, 32, 31, 0, time.UTC),
	}, nil
}

func TestSourceContractUsage(t *testing.T) {
	g := NewWithT(t)

	source := &recordingSource{}
	nodes := []discovery.NodeInfo{
		{Name: nodeName, UID: nodeUID},
		{Name: otherNode, UID: otherUID},
	}

	enumeration, err := source.Enumerate(context.Background(), nodes)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(enumeration.Validate()).To(Succeed())
	g.Expect(enumeration.Authoritative()).To(BeTrue())
	g.Expect(enumeration.Identities).To(HaveLen(len(nodes)))
	g.Expect(source.enumerated).To(HaveLen(len(nodes)))

	observation, err := source.Observe(context.Background(), enumeration.Identities[0])
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(observation.Validate()).To(Succeed())
	g.Expect(observation.Facts.Model).To(Equal(teslaK80))

	// A cancelled context is a hard failure outside the classification:
	// callers treat it as unknown too.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = source.Enumerate(ctx, nodes)
	g.Expect(err).To(MatchError(context.Canceled))
}

// quantity builds a heap Quantity fixture with a stable string form.
func quantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}
