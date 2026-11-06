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

// ErrNoPlaneEvidence reports that no allocation plane has the required
// evidence for a node: the node's inventory is unknown, never zero
// devices (design doc §8.2).
var ErrNoPlaneEvidence = errors.New("discovery: no allocation plane has the required evidence for the node")

// PlaneEvidence is the per-node input of plane resolution (design doc
// §8.2). The caller computes it from the Node object and the enabled
// sources' view of the cluster.
type PlaneEvidence struct {
	// NodeName is the Node the evidence applies to.
	NodeName string
	// DRA is true when node-local advertisements from an allow-listed DRA
	// driver exist — even while incomplete. DRA then wins plane detection
	// and the classic source must not claim the node.
	DRA bool
	// Classic is true when the node publishes classic GPU
	// extended-resource evidence (nvidia.com/gpu in node allocatable or
	// capacity).
	Classic bool
	// Simulator is true when the opt-in classic identity source is
	// enabled.
	Simulator bool
}

// ResolvePlane returns the allocation plane that claims the node (design
// doc §8.2): DRA whenever node-local allow-listed advertisements exist
// (even while incomplete); classic only when no DRA advertisements exist,
// classic extended-resource evidence is present and the opt-in identity
// source is enabled. ok is false when neither plane has the required
// evidence — the node's inventory is then unknown, never zero devices.
func ResolvePlane(e PlaneEvidence) (plane Plane, ok bool) {
	switch {
	case e.DRA:
		return PlaneDRA, true
	case e.Classic && e.Simulator:
		return PlaneClassic, true
	default:
		return "", false
	}
}

// NodeSelection is the resolved discovery decision for one node.
type NodeSelection struct {
	// Plane is the allocation plane that claimed the node; empty when no
	// plane has the required evidence.
	Plane Plane
	// Outcome is the outcome of the claiming source's enumeration:
	// Success (authoritative, possibly empty) or Unknown (retain).
	Outcome Outcome
	// Identities are the discovered identities of an authoritative
	// snapshot; always empty on Unknown.
	Identities []Identity
	// Err is the classified cause when Outcome is Unknown.
	Err error
}

// SelectNode resolves the authoritative enumeration for one node from the
// per-plane enumerations and the plane evidence (design doc §8.2/§8.3).
//
// DRA claims the node whenever allow-listed advertisements exist: the DRA
// enumeration decides — Success is authoritative even when empty, Unknown
// retains — and the classic enumeration is never used as a fallback,
// including for DRA-incomplete nodes. Otherwise classic claims the node
// only with extended-resource evidence and the opt-in identity source,
// and its enumeration decides the same way. When neither plane has the
// required evidence the selection is Unknown with ErrNoPlaneEvidence.
//
// Selection keeps the allocation planes separate: identities are never
// merged across sources, and a partially visible (Unknown) enumeration
// contributes no identities.
func SelectNode(evidence PlaneEvidence, dra, classic Enumeration) NodeSelection {
	plane, ok := ResolvePlane(evidence)
	if !ok {
		return NodeSelection{
			Outcome: OutcomeUnknown,
			Err:     fmt.Errorf("%w: node %q", ErrNoPlaneEvidence, evidence.NodeName),
		}
	}
	enum := classic
	if plane == PlaneDRA {
		enum = dra
	}
	switch enum.Outcome {
	case OutcomeSuccess:
		return NodeSelection{Plane: plane, Outcome: OutcomeSuccess, Identities: enum.Identities}
	case OutcomeUnknown:
		// Identities are deliberately dropped: an unknown enumeration is
		// never a partial hardware set.
		return NodeSelection{Plane: plane, Outcome: OutcomeUnknown, Err: enum.Err}
	default:
		return NodeSelection{
			Plane:   plane,
			Outcome: OutcomeUnknown,
			Err:     fmt.Errorf("%w: node %q selected the %s plane", ErrInvalidOutcome, evidence.NodeName, plane),
		}
	}
}
