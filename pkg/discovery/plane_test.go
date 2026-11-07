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

	. "github.com/onsi/gomega"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
)

// claimSource is a minimal fake Source with a static node-claim rule,
// proving the pluggable contract is implementable exactly as documented.
type claimSource struct {
	id     string
	plane  discovery.Plane
	claims map[string]bool
}

var _ discovery.Source = (*claimSource)(nil)

func (s *claimSource) ID() string             { return s.id }
func (s *claimSource) Plane() discovery.Plane { return s.plane }

func (s *claimSource) ClaimsNode(_ context.Context, node discovery.NodeInfo) bool {
	return s.claims[node.Name]
}

func (s *claimSource) Enumerate(
	ctx context.Context,
	nodes []discovery.NodeInfo,
) (discovery.Enumeration, error) {
	if err := ctx.Err(); err != nil {
		return discovery.Enumeration{}, err
	}
	return discovery.Enumeration{}, nil
}

func (s *claimSource) Observe(
	ctx context.Context,
	id discovery.Identity,
) (discovery.Observation, error) {
	if err := ctx.Err(); err != nil {
		return discovery.Observation{}, err
	}
	return discovery.Observation{}, nil
}

// registry builds the production-shaped registry: DRA registered before
// classic, so DRA claims win the fold.
func registry(draClaims, classicClaims bool) discovery.Sources {
	return discovery.MustNewSources(
		&claimSource{
			id:     draSourceID,
			plane:  discovery.PlaneDRA,
			claims: map[string]bool{nodeName: draClaims},
		},
		&claimSource{
			id:     classicSourceID,
			plane:  discovery.PlaneClassic,
			claims: map[string]bool{nodeName: classicClaims},
		},
	)
}

func TestSourcesRegistry(t *testing.T) {
	t.Run("duplicate source ids are rejected", func(t *testing.T) {
		g := NewWithT(t)
		_, err := discovery.NewSources(
			&claimSource{id: draSourceID, claims: map[string]bool{}},
			&claimSource{id: draSourceID, claims: map[string]bool{}},
		)
		g.Expect(errors.Is(err, discovery.ErrDuplicateSource)).To(BeTrue())
	})

	t.Run("nil sources are rejected", func(t *testing.T) {
		g := NewWithT(t)
		_, err := discovery.NewSources(nil)
		g.Expect(errors.Is(err, discovery.ErrDuplicateSource)).To(BeTrue())
	})

	t.Run("priority order is preserved and the list is a copy", func(t *testing.T) {
		g := NewWithT(t)

		sources := registry(true, true)
		g.Expect(sources.Len()).To(Equal(2))
		list := sources.List()
		g.Expect(list[0].ID()).To(Equal(draSourceID))
		g.Expect(list[1].ID()).To(Equal(classicSourceID))

		// Mutating the returned slice cannot reorder the registry.
		list[0], list[1] = list[1], list[0]
		g.Expect(sources.List()[0].ID()).To(Equal(draSourceID))
	})

	t.Run("empty registry is usable and claims nothing", func(t *testing.T) {
		g := NewWithT(t)
		var sources discovery.Sources
		g.Expect(sources.Len()).To(Equal(0))
		selection := discovery.SelectNode(
			context.Background(),
			discovery.NodeInfo{Name: nodeName, UID: nodeUID},
			sources,
			nil,
		)
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeUnknown))
		g.Expect(errors.Is(selection.Err, discovery.ErrNoPlaneEvidence)).To(BeTrue())
	})
}

func TestSelectNode(t *testing.T) {
	cause := errors.New("pool generation incomplete")
	identities := []discovery.Identity{validIdentity()}
	successful := discovery.Enumeration{Outcome: discovery.OutcomeSuccess, Identities: identities}
	successfulEmpty := discovery.Enumeration{Outcome: discovery.OutcomeSuccess}
	unknown := discovery.Enumeration{Outcome: discovery.OutcomeUnknown, Err: cause}
	node := discovery.NodeInfo{Name: nodeName, UID: nodeUID}

	t.Run("first claimer in priority order decides", func(t *testing.T) {
		g := NewWithT(t)
		// Both sources claim the node: DRA is registered first, so its
		// enumeration decides even though classic enumerated too.
		selection := discovery.SelectNode(context.Background(), node, registry(true, true), map[string]discovery.Enumeration{
			draSourceID:     successful,
			classicSourceID: successfulEmpty,
		})
		g.Expect(selection.SourceID).To(Equal(draSourceID))
		g.Expect(selection.Plane).To(Equal(discovery.PlaneDRA))
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeSuccess))
		g.Expect(selection.Identities).To(Equal(identities))
	})

	t.Run("authoritative empty snapshot prunes", func(t *testing.T) {
		g := NewWithT(t)
		selection := discovery.SelectNode(context.Background(), node, registry(true, true), map[string]discovery.Enumeration{
			draSourceID:     successfulEmpty,
			classicSourceID: successful,
		})
		g.Expect(selection.SourceID).To(Equal(draSourceID))
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeSuccess))
		g.Expect(selection.Identities).To(BeEmpty())
	})

	t.Run("classic fallback is suppressed on dra-incomplete nodes", func(t *testing.T) {
		g := NewWithT(t)
		// DRA claims the node but its snapshot is incomplete: the node is
		// unknown even though classic enumerated successfully — the later
		// source is never consulted as a fallback.
		selection := discovery.SelectNode(context.Background(), node, registry(true, true), map[string]discovery.Enumeration{
			draSourceID:     unknown,
			classicSourceID: successful,
		})
		g.Expect(selection.SourceID).To(Equal(draSourceID))
		g.Expect(selection.Plane).To(Equal(discovery.PlaneDRA))
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeUnknown))
		g.Expect(selection.Err).To(MatchError(cause))
		g.Expect(selection.Identities).To(BeEmpty())
	})

	t.Run("lower-priority source decides when the higher one does not claim", func(t *testing.T) {
		g := NewWithT(t)
		selection := discovery.SelectNode(context.Background(), node, registry(false, true), map[string]discovery.Enumeration{
			classicSourceID: successful,
		})
		g.Expect(selection.SourceID).To(Equal(classicSourceID))
		g.Expect(selection.Plane).To(Equal(discovery.PlaneClassic))
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeSuccess))
		g.Expect(selection.Identities).To(Equal(identities))
	})

	t.Run("unknown classic enumeration retains", func(t *testing.T) {
		g := NewWithT(t)
		selection := discovery.SelectNode(context.Background(), node, registry(false, true), map[string]discovery.Enumeration{
			classicSourceID: unknown,
		})
		g.Expect(selection.SourceID).To(Equal(classicSourceID))
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeUnknown))
		g.Expect(selection.Err).To(MatchError(cause))
	})

	t.Run("claiming source without enumeration is a wiring error", func(t *testing.T) {
		g := NewWithT(t)
		selection := discovery.SelectNode(context.Background(), node, registry(false, true), nil)
		g.Expect(selection.SourceID).To(Equal(classicSourceID))
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeUnknown))
		g.Expect(errors.Is(selection.Err, discovery.ErrMissingEnumeration)).To(BeTrue())
		g.Expect(selection.Identities).To(BeEmpty())
	})

	t.Run("no claiming source is unknown, never zero", func(t *testing.T) {
		g := NewWithT(t)
		enumerations := map[string]discovery.Enumeration{
			draSourceID:     successful,
			classicSourceID: successful,
		}
		selection := discovery.SelectNode(context.Background(), node, registry(false, false), enumerations)
		g.Expect(selection.SourceID).To(BeEmpty())
		g.Expect(selection.Plane).To(BeEmpty())
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeUnknown))
		g.Expect(selection.Identities).To(BeEmpty())
		g.Expect(errors.Is(selection.Err, discovery.ErrNoPlaneEvidence)).To(BeTrue())
	})

	t.Run("unset enumeration outcome is unknown", func(t *testing.T) {
		g := NewWithT(t)
		selection := discovery.SelectNode(context.Background(), node, registry(true, false), map[string]discovery.Enumeration{
			draSourceID: {},
		})
		g.Expect(selection.SourceID).To(Equal(draSourceID))
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeUnknown))
		g.Expect(errors.Is(selection.Err, discovery.ErrInvalidOutcome)).To(BeTrue())
	})

	t.Run("partial identities on unknown are dropped", func(t *testing.T) {
		g := NewWithT(t)
		// Contract-violating input: an unknown enumeration never
		// contributes a partial hardware set.
		partial := discovery.Enumeration{Outcome: discovery.OutcomeUnknown, Err: cause, Identities: identities}
		selection := discovery.SelectNode(context.Background(), node, registry(true, false), map[string]discovery.Enumeration{
			draSourceID: partial,
		})
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeUnknown))
		g.Expect(selection.Identities).To(BeEmpty())
	})
}
