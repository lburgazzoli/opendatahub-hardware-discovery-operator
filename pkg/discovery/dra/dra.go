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

// Package dra implements the stable DRA discovery source (design doc
// §8): whole NVIDIA GPUs advertised in resource.k8s.io/v1 ResourceSlice
// pools by allow-listed drivers, with the vendor GPU UUID as the device
// identity (identity.kind uuid). The source reads only ResourceSlices
// through an injected read-only client — DeviceClasses carry no identity
// and are not read — and never writes.
//
// Pool handling follows the approved contract: pools are keyed by
// (driver, pool.name), only the highest generation is read, and a
// snapshot is usable only when the slices present at that generation
// satisfy the declared resourceSliceCount. Incomplete or conflicting
// pools are Unknown — no partial updates, no fallback to classic facts.
// The source claims a node whenever node-local allow-listed
// advertisements exist, even while incomplete, and wins the registry
// fold over classic.
package dra

import (
	"context"
	"errors"
	"fmt"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
)

// SourceID identifies the DRA source in provenance references and the
// source registry.
const SourceID = "dra"

// DefaultDriver is the default (and in this PoC, the only real) allow
// listed whole-GPU DRA driver.
const DefaultDriver = "gpu.nvidia.com"

// Sentinel errors reported by the source. Callers classify results with
// errors.Is, never by matching message text.
var (
	// ErrPoolIncomplete reports a pool whose slices at the highest
	// generation do not satisfy the declared resourceSliceCount, or that
	// declare conflicting counts. The node's inventory is then unknown —
	// retained, never pruned.
	ErrPoolIncomplete = errors.New("dra: pool snapshot at the highest generation is incomplete")

	// ErrAmbiguousIdentity reports the same vendor UUID advertised by
	// different devices: one is never silently picked, and duplicates are
	// never resolved by list order.
	ErrAmbiguousIdentity = errors.New("dra: duplicate vendor identity in conflicting advertisements")

	// ErrWrongIdentityKind reports an identity that is not uuid.
	ErrWrongIdentityKind = errors.New("dra: identity kind is not uuid")

	// ErrIdentityStale reports an identity that no longer resolves
	// against current complete evidence: a vanished pool or device, a
	// device name that now maps to a different GPU, or a foreign locator.
	ErrIdentityStale = errors.New("dra: identity does not resolve against current evidence")
)

// Option configures the DRA source.
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
	driverAllowList []string
	clock           func() time.Time
}

// defaults returns the default option set: the pinned gpu.nvidia.com
// driver allow-list and the system clock. Tests inject their own clock;
// the collector stamps observations through it.
func defaults() options {
	return options{
		driverAllowList: []string{DefaultDriver},
		clock:           time.Now,
	}
}

func (o options) validate() error {
	if len(o.driverAllowList) == 0 {
		return fmt.Errorf("dra: driver allow-list must not be empty")
	}
	if o.clock == nil {
		return fmt.Errorf("dra: clock must not be nil")
	}
	return nil
}

// WithDriverAllowList replaces the trusted whole-GPU driver set (default
// [gpu.nvidia.com]). Only allow-listed drivers are read.
func WithDriverAllowList(drivers ...string) Option {
	return optionFunc(func(opts *options) {
		opts.driverAllowList = drivers
	})
}

// WithClock injects the collector clock used to stamp observations
// (default time.Now).
func WithClock(clock func() time.Time) Option {
	return optionFunc(func(opts *options) {
		opts.clock = clock
	})
}

// Source is the stable DRA discovery source. It reads ResourceSlices
// through the injected read-only client and never writes.
type Source struct {
	reader client.Reader
	opts   options
}

// New builds the DRA source over a read-only client. Configuration is
// fixed at construction; the returned source is safe for concurrent use
// because it holds no mutable state.
func New(reader client.Reader, opts ...Option) (*Source, error) {
	if reader == nil {
		return nil, fmt.Errorf("dra: reader must not be nil")
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

// Plane is the DRA allocation plane (discovery.Source interface).
func (s *Source) Plane() discovery.Plane { return discovery.PlaneDRA }

// ClaimsNode reports whether the DRA source claims the node
// (discovery.Source interface): node-local advertisements from an
// allow-listed driver exist — even while the pool is incomplete. A node
// with no such advertisements is not claimed by this source.
func (s *Source) ClaimsNode(ctx context.Context, node discovery.NodeInfo) bool {
	slices, err := s.listSlices(ctx, node.Name)
	if err != nil {
		return false
	}
	return s.hasAdvertisements(slices, node.Name)
}

// hasAdvertisements reports whether any allow-listed node-local slice
// exists for the node, regardless of pool completeness.
func (s *Source) hasAdvertisements(slices []resourcev1.ResourceSlice, nodeName string) bool {
	allowed := driverSet(s.opts.driverAllowList)
	for i := range slices {
		if _, trusted := allowed[slices[i].Spec.Driver]; trusted && isNodeLocal(&slices[i], nodeName) {
			return true
		}
	}
	return false
}

// Enumerate returns the identities the source discovers on the given
// nodes (discovery.Source interface): whole-GPU UUID identities from
// every complete pool advertising on the node. Any incomplete or
// conflicting pool classifies the whole enumeration as Unknown: prior
// state is retained, never pruned, and there is no fallback to classic
// facts. The same UUID advertised by different devices is a conflict
// (ErrAmbiguousIdentity), never a silent pick. The returned error is
// reserved for invalid arguments and context cancellation.
func (s *Source) Enumerate(ctx context.Context, nodes []discovery.NodeInfo) (discovery.Enumeration, error) {
	if err := ctx.Err(); err != nil {
		return discovery.Enumeration{}, err
	}
	identities := make([]discovery.Identity, 0)
	for _, node := range nodes {
		if err := node.Validate(); err != nil {
			return discovery.Enumeration{}, err
		}
		slices, err := s.listSlices(ctx, node.Name)
		if err != nil {
			return unknownEnumeration(fmt.Errorf("enumerating node %q: %w", node.Name, err)), nil
		}
		nodeIdentities, err := s.enumerateNode(slices, node)
		if err != nil {
			return unknownEnumeration(fmt.Errorf("enumerating node %q: %w", node.Name, err)), nil
		}
		identities = append(identities, nodeIdentities...)
	}
	return discovery.Enumeration{Outcome: discovery.OutcomeSuccess, Identities: identities}, nil
}

// enumerateNode derives one node's identities from its slices. Pools are
// visited in a deterministic order; devices within a pool follow slice
// and device order as published.
func (s *Source) enumerateNode(
	slices []resourcev1.ResourceSlice,
	node discovery.NodeInfo,
) ([]discovery.Identity, error) {
	snapshots := selectPools(slices, s.opts.driverAllowList, node.Name)

	identities := make([]discovery.Identity, 0)
	seenUUIDs := make(map[string]Locator)
	for _, key := range sortedPoolKeys(snapshots) {
		snapshot := snapshots[key]
		if !snapshot.complete {
			if snapshot.countsConflict {
				return nil, fmt.Errorf("%w: driver %q pool %q generation %d: conflicting declared resourceSliceCount",
					ErrPoolIncomplete, key.driver, key.name, snapshot.generation)
			}
			return nil, fmt.Errorf("%w: driver %q pool %q generation %d: %d of %d slices",
				ErrPoolIncomplete, key.driver, key.name, snapshot.generation,
				len(snapshot.slices), snapshot.sliceCount)
		}
		for i := range snapshot.slices {
			slice := &snapshot.slices[i]
			for _, device := range slice.Spec.Devices {
				identity, ok := deviceIdentity(key, node, device)
				if !ok {
					continue
				}
				if previous, dup := seenUUIDs[identity.ID]; dup {
					if previous.Equal(identity.Locator) {
						// The same device advertised twice identically is
						// one device.
						continue
					}
					return nil, fmt.Errorf("%w: uuid %q in %q and %q",
						ErrAmbiguousIdentity, identity.ID, previous.Ref(), identity.Locator.Ref())
				}
				seenUUIDs[identity.ID] = identity.Locator.(Locator)
				identities = append(identities, identity)
			}
		}
	}
	return identities, nil
}

// deviceIdentity maps one advertised device to an identity. ok is false
// for devices outside the approved whole-GPU contract — MIG or partition
// alternatives, foreign types, missing UUIDs — which are skipped, never
// counted as physical cards.
func deviceIdentity(
	key poolKey,
	node discovery.NodeInfo,
	device resourcev1.Device,
) (discovery.Identity, bool) {
	if !isWholeGPU(device) {
		return discovery.Identity{}, false
	}
	uuid := deviceUUID(device)
	if uuid == "" {
		// Missing-ID rejection: a UUID is never derived from the device
		// name, a count or an index.
		return discovery.Identity{}, false
	}
	return discovery.Identity{
		ID:       uuid,
		Kind:     discovery.IdentityKindUUID,
		NodeName: node.Name,
		NodeUID:  node.UID,
		Plane:    discovery.PlaneDRA,
		Locator: Locator{
			Driver: key.driver,
			Pool:   key.name,
			Device: device.Name,
		},
	}, true
}

// Observe resolves one previously enumerated identity into its current
// advertised facts (discovery.Source interface). The identity is
// re-resolved against current complete evidence — pool presence and
// completeness, device presence, and the UUID the device name now maps
// to — so a reused device name cannot silently retarget an existing
// device. An error means the observation failed: callers keep the last
// facts and surface a condition instead of deleting or restamping them.
func (s *Source) Observe(ctx context.Context, id discovery.Identity) (discovery.Observation, error) {
	if err := ctx.Err(); err != nil {
		return discovery.Observation{}, err
	}
	if id.Kind != discovery.IdentityKindUUID {
		return discovery.Observation{}, fmt.Errorf("%w: %q for device %q", ErrWrongIdentityKind, id.Kind, id.ID)
	}
	locator, ok := id.Locator.(Locator)
	if !ok {
		return discovery.Observation{}, fmt.Errorf("%w: device %q has a non-dra locator", ErrIdentityStale, id.ID)
	}

	slices, err := s.listSlices(ctx, id.NodeName)
	if err != nil {
		return discovery.Observation{}, fmt.Errorf("observing device %q: %w", id.ID, err)
	}
	snapshot, err := s.resolveSnapshot(slices, id.NodeName, locator)
	if err != nil {
		return discovery.Observation{}, fmt.Errorf("observing device %q: %w", id.ID, err)
	}

	device, found := findDevice(snapshot, locator.Device)
	if !found {
		return discovery.Observation{}, fmt.Errorf("%w: device %q is not advertised", ErrIdentityStale, locator.Device)
	}
	if !isWholeGPU(device) {
		return discovery.Observation{}, fmt.Errorf("%w: device %q is no longer a whole gpu", ErrIdentityStale, locator.Device)
	}
	if uuid := deviceUUID(device); uuid != id.ID {
		return discovery.Observation{}, fmt.Errorf(
			"%w: device %q now maps to uuid %q", ErrIdentityStale, locator.Device, uuid)
	}

	observed := discovery.Facts{}
	model := deviceModel(device)
	memory := deviceMemory(device)
	if model != "" || memory != nil {
		observed.Accelerator = &discovery.AcceleratorFacts{
			Model:  model,
			Memory: memory,
		}
	}
	return discovery.Observation{Identity: id, Facts: observed, CollectedAt: s.opts.clock()}, nil
}

// resolveSnapshot selects the locator's pool snapshot from the node's
// slices. A vanished pool, an incomplete snapshot or a foreign driver
// cannot be observed against.
func (s *Source) resolveSnapshot(
	slices []resourcev1.ResourceSlice,
	nodeName string,
	locator Locator,
) (poolSnapshot, error) {
	snapshots := selectPools(slices, s.opts.driverAllowList, nodeName)
	snapshot, present := snapshots[poolKey{driver: locator.Driver, name: locator.Pool}]
	if !present {
		return poolSnapshot{}, fmt.Errorf("%w: pool %q is not advertised", ErrIdentityStale, locator.Ref())
	}
	if !snapshot.complete {
		return poolSnapshot{}, fmt.Errorf("%w: pool %q", ErrPoolIncomplete, locator.Ref())
	}
	return snapshot, nil
}

// findDevice locates a device by name within a pool snapshot.
func findDevice(snapshot poolSnapshot, deviceName string) (resourcev1.Device, bool) {
	for i := range snapshot.slices {
		for _, device := range snapshot.slices[i].Spec.Devices {
			if device.Name == deviceName {
				return device, true
			}
		}
	}
	return resourcev1.Device{}, false
}

// listSlices reads the node's ResourceSlices via the server-supported
// spec.nodeName field selector.
func (s *Source) listSlices(ctx context.Context, nodeName string) ([]resourcev1.ResourceSlice, error) {
	list := &resourcev1.ResourceSliceList{}
	if err := s.reader.List(ctx, list, client.MatchingFields{
		resourcev1.ResourceSliceSelectorNodeName: nodeName,
	}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// unknownEnumeration classifies a read or pool failure: identities are
// dropped because a partially visible snapshot is unknown, never a
// partial hardware set.
func unknownEnumeration(cause error) discovery.Enumeration {
	return discovery.Enumeration{Outcome: discovery.OutcomeUnknown, Err: cause}
}
