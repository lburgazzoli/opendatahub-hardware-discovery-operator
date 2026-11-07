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

// Package discovery holds the source and identity contracts shared by the
// discovery source plugins and the two reconcilers. The package is pure,
// source-agnostic normalization logic: it never enumerates source kinds,
// planes or input types. It imports only k8s.io/apimachinery and the
// standard library, never constructs HardwareDevice objects, and never
// reads the wall clock or the cluster. Mapping discovery results into
// HardwareDeviceStatus happens once, in the device controller; changing
// this representation requires a re-approved contract change.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
)

// Plane is the allocation plane that evidenced a device.
type Plane string

const (
	// PlaneClassic is the classic device-plugin allocation plane.
	PlaneClassic Plane = "classic"
	// PlaneDRA is the dynamic resource allocation plane.
	PlaneDRA Plane = "dra"
)

// Valid reports whether p is one of the known allocation planes.
func (p Plane) Valid() bool {
	return p == PlaneClassic || p == PlaneDRA
}

// IdentityKind distinguishes real vendor identity from synthetic
// node-scoped identity: uuid is a real
// vendor device ID (GPU UUID, published by DRA); nodeIndex is a synthetic
// identity derived from the node name and the device index within the node
// (classic labels source) — stable only for the node incarnation and not
// joinable with telemetry or vendor tooling. It is persisted as
// spec.identity.kind.
type IdentityKind string

const (
	// IdentityKindUUID is a real vendor device identity.
	IdentityKindUUID IdentityKind = "uuid"
	// IdentityKindNodeIndex is a synthetic node-scoped identity.
	IdentityKindNodeIndex IdentityKind = "nodeIndex"
)

// Valid reports whether k is one of the known identity kinds.
func (k IdentityKind) Valid() bool {
	return k == IdentityKindUUID || k == IdentityKindNodeIndex
}

// Outcome distinguishes authoritative source results from unknown ones.
// The two are never conflated: an authoritative empty snapshot is allowed
// to prune, while an unknown result preserves all prior state.
type Outcome string

const (
	// OutcomeSuccess is an authoritative snapshot; it may be empty.
	OutcomeSuccess Outcome = "Success"
	// OutcomeUnknown covers errors, missing APIs, forbidden reads and
	// incomplete pools.
	OutcomeUnknown Outcome = "Unknown"
)

// Sentinel errors reported by the Validate methods and the selection
// helpers. Callers classify results with errors.Is, never by matching
// message text.
var (
	// ErrMissingID reports a discovered identity without a device id.
	// Device ids are never derived from resource counts or list indexes:
	// a device without a usable id is rejected.
	ErrMissingID = errors.New("discovery: device identity id is empty")

	// ErrMissingNode reports an identity or node context without a
	// complete Node reference (name and UID).
	ErrMissingNode = errors.New("discovery: node reference is incomplete")

	// ErrUnknownPlane reports an allocation plane that is neither classic
	// nor dra.
	ErrUnknownPlane = errors.New("discovery: allocation plane is not classic or dra")

	// ErrUnknownIdentityKind reports an identity kind that is neither uuid
	// nor nodeIndex.
	ErrUnknownIdentityKind = errors.New("discovery: identity kind is not uuid or nodeIndex")

	// ErrZeroMemory reports a memory fact published as zero instead of
	// omitted: optional facts are absent or present, never zero or
	// defaulted.
	ErrZeroMemory = errors.New("discovery: memory fact must be omitted, not zero")

	// ErrUnstampedObservation reports an observation that carries no
	// collector collection time.
	ErrUnstampedObservation = errors.New("discovery: observation is not stamped with a collection time")

	// ErrSuccessWithCause reports a Success enumeration that also carries
	// an error.
	ErrSuccessWithCause = errors.New("discovery: success enumeration must not carry an error")

	// ErrUnknownWithoutCause reports an Unknown enumeration without a
	// classified cause.
	ErrUnknownWithoutCause = errors.New("discovery: unknown enumeration must carry a classified cause")

	// ErrPartialEnumeration reports an Unknown enumeration that also
	// carries identities: a partially visible snapshot is unknown, never a
	// partial hardware set.
	ErrPartialEnumeration = errors.New("discovery: unknown enumeration must not carry identities")

	// ErrInvalidOutcome reports an enumeration outcome that is neither
	// Success nor Unknown.
	ErrInvalidOutcome = errors.New("discovery: enumeration outcome is not Success or Unknown")
)

// NodeInfo is the node context passed to Source.Enumerate. It carries only
// what sources need to enumerate a node; the caller computes it from the
// Node object.
type NodeInfo struct {
	// Name is the Node name.
	Name string
	// UID is the Node UID. It becomes part of every discovered identity
	// and cannot be dropped from matching.
	UID types.UID
}

// Validate reports whether the node reference is complete.
func (n NodeInfo) Validate() error {
	if n.Name == "" || n.UID == "" {
		return fmt.Errorf("%w: node %q", ErrMissingNode, n.Name)
	}
	return nil
}

// Identity is a discovered device identity plus how to re-observe it.
type Identity struct {
	// ID is the device id: the vendor device ID (GPU UUID) for Kind uuid,
	// or the deterministic <node-name>-gpu-<index> for Kind nodeIndex.
	// nodeIndex ids are node-scoped and die with the node incarnation —
	// a deliberate, user-approved relaxation of the earlier
	// no-fabricated-identity rule, kept distinguishable from
	// vendor identity via Kind.
	ID string
	// Kind is the nature of the identity: uuid (real vendor identity,
	// DRA) or nodeIndex (synthetic node-scoped identity, classic labels).
	Kind IdentityKind
	// NodeName is the name of the Node the device was discovered on.
	NodeName string
	// NodeUID is the UID of the Node the device was discovered on.
	NodeUID types.UID
	// Plane is the allocation plane that evidenced the device.
	Plane Plane
	// Locator is the source-owned locator stored verbatim in spec.source.
	Locator Locator
}

// Validate reports whether the identity is complete: a device id, an
// identity kind, a full Node reference, a known allocation plane and a
// valid source locator. Source refs must identify real input objects, not
// an inferred registry.
func (i Identity) Validate() error {
	switch {
	case i.ID == "":
		return fmt.Errorf("%w: node %q", ErrMissingID, i.NodeName)
	case i.NodeName == "" || i.NodeUID == "":
		return fmt.Errorf("%w: device %q", ErrMissingNode, i.ID)
	}
	if !i.Kind.Valid() {
		return fmt.Errorf("%w: %q for device %q", ErrUnknownIdentityKind, i.Kind, i.ID)
	}
	if !i.Plane.Valid() {
		return fmt.Errorf("%w: %q for device %q", ErrUnknownPlane, i.Plane, i.ID)
	}
	if i.Locator == nil {
		return fmt.Errorf("device %q: %w", i.ID, ErrNoLocator)
	}
	if err := i.Locator.Validate(); err != nil {
		return fmt.Errorf("device %q: %w", i.ID, err)
	}
	return nil
}

// Facts are the minimal supported observations, organized per device class
// exactly like the HardwareDevice status: class-specific facts live in the
// optional struct selected by the device's class (accelerator in this PoC),
// and future classes arrive as new sibling structs. Absent facts are
// omitted — nil memory, empty strings — never zero or defaulted values.
type Facts struct {
	// Accelerator carries accelerator-class facts; nil when no accelerator
	// facts were observed.
	Accelerator *AcceleratorFacts
}

// AcceleratorFacts are the observed facts of one whole accelerator (GPU).
type AcceleratorFacts struct {
	// Vendor is the device vendor when the source publishes it. Neither
	// the classic GFD labels nor the NVIDIA DRA attributes carry vendor,
	// so it is omitted in this PoC.
	Vendor string
	// Model is the observed device model.
	Model string
	// Memory is the observed device memory in bytes; nil when the source
	// does not publish it.
	Memory *resource.Quantity
}

// Validate reports whether the facts respect the absent-never-zero
// contract: a published memory must not be zero, and absent facts must be
// omitted rather than defaulted.
func (f Facts) Validate() error {
	if f.Accelerator == nil {
		return nil
	}
	if f.Accelerator.Memory != nil && f.Accelerator.Memory.IsZero() {
		return ErrZeroMemory
	}
	return nil
}

// Enumeration is the result of enumerating nodes from one source. The
// outcome applies to the whole request; callers needing per-node outcomes
// enumerate one node at a time.
type Enumeration struct {
	// Outcome is Success for an authoritative snapshot (possibly empty)
	// or Unknown for errors, missing APIs, forbidden reads and incomplete
	// pools.
	Outcome Outcome
	// Identities are the discovered identities. Empty with Success is an
	// authoritative empty snapshot.
	Identities []Identity
	// Err is the classified cause; it is set iff Outcome is Unknown.
	Err error
}

// Authoritative reports whether the enumeration is an authoritative
// snapshot: Success prunes absent devices, Unknown preserves all prior
// state.
func (e Enumeration) Authoritative() bool {
	return e.Outcome == OutcomeSuccess
}

// Validate checks the enumeration invariants: Success carries no error,
// Unknown carries a classified cause and no identities (a partially
// visible snapshot is unknown, never a partial hardware set), and the
// outcome is one of the two known values.
func (e Enumeration) Validate() error {
	switch e.Outcome {
	case OutcomeSuccess:
		if e.Err != nil {
			return fmt.Errorf("%w: %w", ErrSuccessWithCause, e.Err)
		}
	case OutcomeUnknown:
		if e.Err == nil {
			return ErrUnknownWithoutCause
		}
		if len(e.Identities) > 0 {
			return ErrPartialEnumeration
		}
	default:
		return fmt.Errorf("%w: %q", ErrInvalidOutcome, e.Outcome)
	}
	return nil
}

// Observation is a successful single-device observation.
type Observation struct {
	// Identity is the observed device identity.
	Identity Identity
	// Facts are the observed facts; unsupported optional observations
	// remain absent.
	Facts Facts
	// CollectedAt is stamped by the collector's own clock at observation
	// time. It is never copied from a producer and never refreshed
	// without a successful observation.
	CollectedAt time.Time
}

// Validate reports whether the observation is complete: a valid identity,
// contract-respecting facts and a stamped collection time.
func (o Observation) Validate() error {
	if err := o.Identity.Validate(); err != nil {
		return err
	}
	if err := o.Facts.Validate(); err != nil {
		return fmt.Errorf("device %q: %w", o.Identity.ID, err)
	}
	if o.CollectedAt.IsZero() {
		return fmt.Errorf("%w: device %q", ErrUnstampedObservation, o.Identity.ID)
	}
	return nil
}

// Source is the discovery source plugin contract. A source owns its
// locator type, its input reading (via
// injected read-only clients), its node-claim rule and its facts parsing;
// the core folds claiming sources by priority. A source never owns
// Kubernetes inventory writes and never constructs HardwareDevice objects.
type Source interface {
	// ID identifies the source in provenance references (status.sourceRef)
	// and keys its per-node enumerations. It must match the Kind of the
	// locators the source produces.
	ID() string
	// Plane is the allocation plane the source evidences.
	Plane() Plane
	// ClaimsNode reports whether this source claims the node for
	// discovery. The claim rule is source knowledge — for example, the DRA
	// source claims nodes with allow-listed advertisements (even while
	// incomplete), the classic source claims nodes with classic
	// extended-resource evidence plus GFD-style labels. Unclaimed nodes
	// are unknown for this source — never an authoritative empty set.
	ClaimsNode(ctx context.Context, node NodeInfo) bool
	// Enumerate returns the identities the source discovers on the given
	// nodes. The Enumeration is the classified result: Success (possibly
	// with zero identities) is authoritative and may prune; Unknown
	// preserves all prior state. The returned error is reserved for
	// failures that prevent any classification at all — invalid arguments
	// or context cancellation — and callers must treat it as an unknown
	// outcome too.
	Enumerate(ctx context.Context, nodes []NodeInfo) (Enumeration, error)
	// Observe resolves one previously enumerated identity into facts. An
	// error means the observation failed: callers keep the last facts and
	// surface a condition instead of deleting or restamping them.
	Observe(ctx context.Context, id Identity) (Observation, error)
}
