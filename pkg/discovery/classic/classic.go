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

// Package classic implements the classic labels discovery source (design
// doc §9, amendment 3): a production source that discovers accelerators
// on pre-DRA nodes from exactly what a real classic (ClusterPolicy/GFD)
// cluster publishes — GFD-style node labels (gpu.product, gpu.memory MiB,
// gpu.count) plus classic extended-resource evidence (the configured
// resource name in node allocatable/capacity), read through an injected
// read-only client. The source never writes and is not aware of any
// simulator.
//
// Per the user-approved relaxation of the no-fabricated-identity rule,
// classic devices carry a synthetic node-scoped identity
// (identity.kind nodeIndex, id "<node>-gpu-<index>"): stable for the node
// incarnation, dying with it, and never presented as vendor identity.
// Facts are homogeneous per node — GFD publishes one product/memory per
// node — which is a documented limitation of the labels input.
package classic

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
)

// SourceID identifies the classic source in provenance references and
// the source registry.
const SourceID = "classic"

// Sentinel errors reported by the source. Callers classify results with
// errors.Is, never by matching message text.
var (
	// ErrNodeUIDMismatch reports an identity whose hosting node UID
	// changed: a new node incarnation is a new device set, never an
	// in-place retarget of an existing one.
	ErrNodeUIDMismatch = errors.New("classic: node uid changed")

	// ErrUnclaimedNode reports an observe target whose node no longer
	// publishes the classic evidence (extended resource plus a parseable
	// gpu.count).
	ErrUnclaimedNode = errors.New("classic: node is no longer claimed by the classic source")

	// ErrIdentityStale reports an identity that is not among the node's
	// current devices: wrong locator shape, index out of the published
	// range, or an id that no longer matches the derivation.
	ErrIdentityStale = errors.New("classic: identity is not among the node's current devices")

	// ErrMalformedLabel reports a present but unusable node label.
	ErrMalformedLabel = errors.New("classic: node label is malformed")

	// ErrWrongIdentityKind reports an identity that is not nodeIndex.
	ErrWrongIdentityKind = errors.New("classic: identity kind is not nodeIndex")
)

// Option configures the classic source.
type Option interface {
	apply(*options)
}

type optionFunc func(*options)

func (fn optionFunc) apply(opts *options) {
	if fn != nil && opts != nil {
		fn(opts)
	}
}

// options holds the optional source configuration.
type options struct {
	resourceName string
	clock        func() time.Time
}

// defaults returns the default option set: the nvidia.com/gpu extended
// resource as plane evidence and the system clock. Tests inject their own
// clock; the collector stamps observations through it.
func defaults() options {
	return options{
		resourceName: DefaultResourceName,
		clock:        time.Now,
	}
}

func (o options) validate() error {
	if o.resourceName == "" {
		return fmt.Errorf("classic: resource name must not be empty")
	}
	if o.clock == nil {
		return fmt.Errorf("classic: clock must not be nil")
	}
	return nil
}

// WithResourceName selects the extended resource whose presence in node
// allocatable or capacity is classic-plane evidence (default
// nvidia.com/gpu).
func WithResourceName(resourceName string) Option {
	return optionFunc(func(opts *options) {
		opts.resourceName = resourceName
	})
}

// WithClock injects the collector clock used to stamp observations
// (default time.Now).
func WithClock(clock func() time.Time) Option {
	return optionFunc(func(opts *options) {
		opts.clock = clock
	})
}

// Source is the classic labels discovery source. It reads Nodes through
// the injected read-only client and never writes.
type Source struct {
	reader client.Reader
	opts   options
}

// New builds the classic source over a read-only client. Configuration is
// fixed at construction; the returned source is safe for concurrent use
// because it holds no mutable state.
func New(reader client.Reader, opts ...Option) (*Source, error) {
	if reader == nil {
		return nil, fmt.Errorf("classic: reader must not be nil")
	}
	o := defaults()
	for _, opt := range opts {
		opt.apply(&o)
	}
	if err := o.validate(); err != nil {
		return nil, err
	}
	return &Source{reader: reader, opts: o}, nil
}

// ID identifies the source (discovery.Source interface).
func (s *Source) ID() string { return SourceID }

// Plane is the classic allocation plane (discovery.Source interface).
func (s *Source) Plane() discovery.Plane { return discovery.PlaneClassic }

// ClaimsNode reports whether the classic source claims the node
// (discovery.Source interface): classic extended-resource evidence must
// be present and the node's gpu.count label must be parseable. Nodes
// without either are unknown for this source — never an authoritative
// empty set — and the source never interprets allocatable as remaining
// free GPUs.
func (s *Source) ClaimsNode(ctx context.Context, node discovery.NodeInfo) bool {
	n, err := s.fetchNode(ctx, node)
	if err != nil {
		return false
	}
	return s.claims(n)
}

// claims applies the claim rule to a fetched Node.
func (s *Source) claims(node *corev1.Node) bool {
	if !extendedResourceEvidence(node, s.opts.resourceName) {
		return false
	}
	_, ok := nodeGPUCount(node)
	return ok
}

// Enumerate returns the identities the source discovers on the given
// nodes (discovery.Source interface): gpu.count nodeIndex identities per
// claimed node. Unclaimed nodes contribute nothing — their inventory is
// decided by the registry fold, not by this source. A node read failure
// classifies the whole enumeration as Unknown: prior state is retained,
// never pruned. The returned error is reserved for invalid arguments and
// context cancellation.
func (s *Source) Enumerate(ctx context.Context, nodes []discovery.NodeInfo) (discovery.Enumeration, error) {
	if err := ctx.Err(); err != nil {
		return discovery.Enumeration{}, err
	}
	identities := make([]discovery.Identity, 0)
	for _, node := range nodes {
		if err := node.Validate(); err != nil {
			return discovery.Enumeration{}, err
		}
		n, err := s.fetchNode(ctx, node)
		if err != nil {
			return unknownEnumeration(fmt.Errorf("enumerating node %q: %w", node.Name, err)), nil
		}
		if !s.claims(n) {
			continue
		}
		count, _ := nodeGPUCount(n)
		identities = append(identities, nodeIdentities(n, count)...)
	}
	return discovery.Enumeration{Outcome: discovery.OutcomeSuccess, Identities: identities}, nil
}

// Observe resolves one previously enumerated identity into its current
// label facts (discovery.Source interface). The identity is re-resolved
// against current evidence — node UID, claim, index range and id
// derivation — so a changed node cannot silently retarget an existing
// device. An error means the observation failed: callers keep the last
// facts and surface a condition instead of deleting or restamping them.
func (s *Source) Observe(ctx context.Context, id discovery.Identity) (discovery.Observation, error) {
	if err := ctx.Err(); err != nil {
		return discovery.Observation{}, err
	}
	if id.Kind != discovery.IdentityKindNodeIndex {
		return discovery.Observation{}, fmt.Errorf("%w: %q for device %q", ErrWrongIdentityKind, id.Kind, id.ID)
	}
	locator, ok := id.Locator.(Locator)
	if !ok {
		return discovery.Observation{}, fmt.Errorf("%w: device %q has a non-classic locator", ErrIdentityStale, id.ID)
	}

	n, err := s.fetchNode(ctx, discovery.NodeInfo{Name: id.NodeName, UID: id.NodeUID})
	if err != nil {
		return discovery.Observation{}, fmt.Errorf("observing device %q: %w", id.ID, err)
	}
	if !s.claims(n) {
		return discovery.Observation{}, fmt.Errorf("%w: node %q", ErrUnclaimedNode, n.Name)
	}

	count, _ := nodeGPUCount(n)
	switch {
	case locator.Node != n.Name:
		return discovery.Observation{}, fmt.Errorf("%w: locator node %q", ErrIdentityStale, locator.Node)
	case locator.Index < 0 || locator.Index >= int32(count):
		return discovery.Observation{}, fmt.Errorf("%w: index %d of %d devices", ErrIdentityStale, locator.Index, count)
	case id.ID != DeviceID(n.Name, locator.Index):
		return discovery.Observation{}, fmt.Errorf("%w: id %q", ErrIdentityStale, id.ID)
	}

	facts, err := acceleratorFacts(n)
	if err != nil {
		return discovery.Observation{}, fmt.Errorf("observing device %q: %w", id.ID, err)
	}

	observed := discovery.Facts{}
	if facts.present() {
		observed.Accelerator = &discovery.AcceleratorFacts{
			Model:  facts.model,
			Memory: facts.memory,
		}
	}
	return discovery.Observation{Identity: id, Facts: observed, CollectedAt: s.opts.clock()}, nil
}

// fetchNode reads the Node by name and verifies the caller's UID: a UID
// mismatch means the caller's view is stale — the node was recreated —
// and is reported as ErrNodeUIDMismatch.
func (s *Source) fetchNode(ctx context.Context, node discovery.NodeInfo) (*corev1.Node, error) {
	var n corev1.Node
	if err := s.reader.Get(ctx, client.ObjectKey{Name: node.Name}, &n); err != nil {
		return nil, err
	}
	if node.UID != "" && n.UID != node.UID {
		return nil, fmt.Errorf("%w: have %q, caller saw %q", ErrNodeUIDMismatch, n.UID, node.UID)
	}
	return &n, nil
}

// nodeIdentities derives the node's gpu.count nodeIndex identities.
func nodeIdentities(node *corev1.Node, count int64) []discovery.Identity {
	identities := make([]discovery.Identity, 0, count)
	for index := range count {
		identities = append(identities, discovery.Identity{
			ID:       DeviceID(node.Name, int32(index)),
			Kind:     discovery.IdentityKindNodeIndex,
			NodeName: node.Name,
			NodeUID:  node.UID,
			Plane:    discovery.PlaneClassic,
			Locator:  Locator{Node: node.Name, Index: int32(index)},
		})
	}
	return identities
}

// unknownEnumeration classifies a read failure: identities are dropped
// because a partially visible snapshot is unknown, never a partial
// hardware set.
func unknownEnumeration(cause error) discovery.Enumeration {
	return discovery.Enumeration{Outcome: discovery.OutcomeUnknown, Err: cause}
}
