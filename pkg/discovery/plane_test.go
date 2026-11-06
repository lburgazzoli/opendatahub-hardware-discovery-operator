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

func TestResolvePlane(t *testing.T) {
	cases := []struct {
		name     string
		evidence discovery.PlaneEvidence
		plane    discovery.Plane
		claimed  bool
	}{
		{
			// DRA wins even while incomplete and even when classic
			// evidence exists.
			"dra dominates",
			discovery.PlaneEvidence{NodeName: nodeName, DRA: true, Classic: true, Simulator: true},
			discovery.PlaneDRA,
			true,
		},
		{
			"dra only",
			discovery.PlaneEvidence{NodeName: nodeName, DRA: true},
			discovery.PlaneDRA,
			true,
		},
		{
			"classic claims",
			discovery.PlaneEvidence{NodeName: nodeName, Classic: true, Simulator: true},
			discovery.PlaneClassic,
			true,
		},
		{
			// Classic needs the extended-resource evidence AND the opt-in
			// identity source.
			"classic without simulator source",
			discovery.PlaneEvidence{NodeName: nodeName, Classic: true},
			"",
			false,
		},
		{
			"classic without extended-resource evidence",
			discovery.PlaneEvidence{NodeName: nodeName, Simulator: true},
			"",
			false,
		},
		{
			// Neither plane has the required evidence: unknown, never
			// zero devices.
			"no evidence",
			discovery.PlaneEvidence{NodeName: nodeName},
			"",
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			plane, ok := discovery.ResolvePlane(tc.evidence)
			g.Expect(ok).To(Equal(tc.claimed))
			g.Expect(plane).To(Equal(tc.plane))
		})
	}
}

func TestSelectNode(t *testing.T) {
	cause := errors.New("pool generation incomplete")
	identities := []discovery.Identity{validIdentity()}
	successful := discovery.Enumeration{Outcome: discovery.OutcomeSuccess, Identities: identities}
	successfulEmpty := discovery.Enumeration{Outcome: discovery.OutcomeSuccess}
	unknown := discovery.Enumeration{Outcome: discovery.OutcomeUnknown, Err: cause}
	draEvidence := discovery.PlaneEvidence{NodeName: nodeName, DRA: true}
	classicEvidence := discovery.PlaneEvidence{NodeName: nodeName, Classic: true, Simulator: true}

	t.Run("authoritative dra snapshot is used", func(t *testing.T) {
		g := NewWithT(t)
		selection := discovery.SelectNode(draEvidence, successful, successfulEmpty)
		g.Expect(selection.Plane).To(Equal(discovery.PlaneDRA))
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeSuccess))
		g.Expect(selection.Identities).To(Equal(identities))
	})

	t.Run("authoritative dra empty prunes", func(t *testing.T) {
		g := NewWithT(t)
		selection := discovery.SelectNode(draEvidence, successfulEmpty, successfulEmpty)
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeSuccess))
		g.Expect(selection.Identities).To(BeEmpty())
	})

	t.Run("classic fallback is suppressed on dra-incomplete nodes", func(t *testing.T) {
		g := NewWithT(t)
		// DRA advertisements exist but the DRA snapshot is incomplete:
		// the node is unknown even though classic enumerated successfully.
		selection := discovery.SelectNode(draEvidence, unknown, successful)
		g.Expect(selection.Plane).To(Equal(discovery.PlaneDRA))
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeUnknown))
		g.Expect(selection.Err).To(MatchError(cause))
		g.Expect(selection.Identities).To(BeEmpty())
	})

	t.Run("authoritative classic snapshot is used", func(t *testing.T) {
		g := NewWithT(t)
		selection := discovery.SelectNode(classicEvidence, unknown, successfulEmpty)
		g.Expect(selection.Plane).To(Equal(discovery.PlaneClassic))
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeSuccess))
		g.Expect(selection.Identities).To(BeEmpty())
	})

	t.Run("unknown classic enumeration retains", func(t *testing.T) {
		g := NewWithT(t)
		selection := discovery.SelectNode(classicEvidence, successful, unknown)
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeUnknown))
		g.Expect(selection.Err).To(MatchError(cause))
	})

	t.Run("no plane evidence is unknown, never zero", func(t *testing.T) {
		g := NewWithT(t)
		selection := discovery.SelectNode(discovery.PlaneEvidence{NodeName: nodeName}, successful, successful)
		g.Expect(selection.Plane).To(BeEmpty())
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeUnknown))
		g.Expect(selection.Identities).To(BeEmpty())
		g.Expect(errors.Is(selection.Err, discovery.ErrNoPlaneEvidence)).To(BeTrue())
	})

	t.Run("unset enumeration outcome is unknown", func(t *testing.T) {
		g := NewWithT(t)
		selection := discovery.SelectNode(draEvidence, discovery.Enumeration{}, successful)
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeUnknown))
		g.Expect(errors.Is(selection.Err, discovery.ErrInvalidOutcome)).To(BeTrue())
	})

	t.Run("partial identities on unknown are dropped", func(t *testing.T) {
		g := NewWithT(t)
		// Contract-violating input: an unknown enumeration never
		// contributes a partial hardware set.
		partial := discovery.Enumeration{Outcome: discovery.OutcomeUnknown, Err: cause, Identities: identities}
		selection := discovery.SelectNode(classicEvidence, successful, partial)
		g.Expect(selection.Outcome).To(Equal(discovery.OutcomeUnknown))
		g.Expect(selection.Identities).To(BeEmpty())
	})
}
