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
	"context"
	"errors"
	"fmt"
)

// ErrNoPlaneEvidence reports that no registered source claims a node: the
// node's inventory is unknown, never zero devices.
var ErrNoPlaneEvidence = errors.New("discovery: no registered source claims the node")

// ErrMissingEnumeration reports a source that claims a node without a
// matching entry in the per-source enumerations passed to SelectNode. It
// is a caller wiring error, never an authoritative result.
var ErrMissingEnumeration = errors.New("discovery: claiming source has no enumeration")

// ErrDuplicateSource reports a source registry that registers two sources
// with the same ID.
var ErrDuplicateSource = errors.New("discovery: duplicate source id in registry")

// Sources is the ordered source registry.
// Priority is the registration order: the production binary registers DRA
// before classic, so DRA claims win the SelectNode fold. Duplicate source
// IDs are rejected at registration. The zero value is an empty registry.
type Sources struct {
	list []Source
}

// NewSources builds a registry from sources in priority order. It rejects
// duplicate source IDs: one kind must never decide a node twice.
func NewSources(sources ...Source) (Sources, error) {
	seen := make(map[string]struct{}, len(sources))
	for _, src := range sources {
		if src == nil {
			return Sources{}, fmt.Errorf("%w: nil source", ErrDuplicateSource)
		}
		if _, dup := seen[src.ID()]; dup {
			return Sources{}, fmt.Errorf("%w: %q", ErrDuplicateSource, src.ID())
		}
		seen[src.ID()] = struct{}{}
	}
	return Sources{list: append([]Source(nil), sources...)}, nil
}

// MustNewSources is NewSources for statically known registrations; it
// panics on duplicate IDs, which is a programming error caught at startup.
func MustNewSources(sources ...Source) Sources {
	registry, err := NewSources(sources...)
	if err != nil {
		panic(err)
	}
	return registry
}

// List returns the registered sources in priority order. The returned
// slice is a copy; callers cannot reorder or mutate the registry through
// it.
func (s Sources) List() []Source {
	return append([]Source(nil), s.list...)
}

// Len returns the number of registered sources.
func (s Sources) Len() int {
	return len(s.list)
}

// NodeSelection is the resolved discovery decision for one node.
type NodeSelection struct {
	// SourceID is the ID of the source that claimed the node; empty when
	// no registered source claims it.
	SourceID string
	// Plane is the allocation plane of the claiming source; empty when no
	// registered source claims the node.
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

// SelectNode resolves the authoritative enumeration for one node by
// folding the registered sources' claims in priority order.
//
// The first source that claims the node decides: its enumeration's Outcome
// governs — Success is authoritative even when empty, Unknown retains —
// and no later source is consulted, so a classic enumeration is never used
// as a fallback for a DRA-claimed node, including DRA-incomplete ones.
// When no registered source claims the node, the selection is Unknown with
// ErrNoPlaneEvidence: unknown, never zero devices.
//
// enumerated carries each source's Enumeration keyed by source ID; a
// claiming source without an entry is a wiring error reported as Unknown
// with ErrMissingEnumeration, never an authoritative result.
//
// Selection keeps the allocation planes separate: identities are never
// merged across sources, and a partially visible (Unknown) enumeration
// contributes no identities.
func SelectNode(ctx context.Context, node NodeInfo, sources Sources, enumerated map[string]Enumeration) NodeSelection {
	for _, src := range sources.list {
		if !src.ClaimsNode(ctx, node) {
			continue
		}
		enum, ok := enumerated[src.ID()]
		if !ok {
			return NodeSelection{
				SourceID: src.ID(),
				Plane:    src.Plane(),
				Outcome:  OutcomeUnknown,
				Err:      fmt.Errorf("%w: source %q, node %q", ErrMissingEnumeration, src.ID(), node.Name),
			}
		}
		switch enum.Outcome {
		case OutcomeSuccess:
			return NodeSelection{SourceID: src.ID(), Plane: src.Plane(), Outcome: OutcomeSuccess, Identities: enum.Identities}
		case OutcomeUnknown:
			// Identities are deliberately dropped: an unknown enumeration
			// is never a partial hardware set.
			return NodeSelection{SourceID: src.ID(), Plane: src.Plane(), Outcome: OutcomeUnknown, Err: enum.Err}
		default:
			return NodeSelection{
				SourceID: src.ID(),
				Plane:    src.Plane(),
				Outcome:  OutcomeUnknown,
				Err:      fmt.Errorf("%w: source %q, node %q", ErrInvalidOutcome, src.ID(), node.Name),
			}
		}
	}
	return NodeSelection{
		Outcome: OutcomeUnknown,
		Err:     fmt.Errorf("%w: node %q", ErrNoPlaneEvidence, node.Name),
	}
}
