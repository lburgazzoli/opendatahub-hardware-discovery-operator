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

package inventory

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/classic"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/dra"
)

// materialize turns the desired identities into owned HardwareDevice
// specs. An existing owned device with the exact same device key keeps
// its object: its spec, labels and grace annotation are reconciled with
// no unconditional writes. A new incarnation resolves the deterministic
// object name; a name held by a foreign object is never adopted, and a
// name still held by a prior incarnation of the same id waits for the
// grace period to prune the old object before the fresh one is created.
func (r *Reconciler) materialize(
	ctx context.Context,
	single *infrastructurev1alpha1.HardwareDiscovery,
	cfg config,
	state *inventoryState,
	desired map[deviceKey]discovery.Identity,
	issues *issueLog,
) error {
	taken := state.takenNames()
	for _, key := range sortedDesiredKeys(desired) {
		identity := desired[key]
		if existing := state.ownedByKey(key); existing != nil {
			if err := r.ensureDevice(ctx, existing, identity, cfg.class); err != nil {
				return err
			}
			continue
		}

		resolution := resolveDeviceName(state, identity, taken)
		switch {
		case resolution.collides:
			issues.add(reasonNameCollision, priorityNameCollision,
				"hardware device name %q is held by a foreign object; identity %q is skipped",
				discovery.DeviceName(identity.ID), identity.ID)
			r.eventf(single, corev1.EventTypeWarning, reasonNameCollision, "Skipped",
				"derived hardware device name %q collides with a foreign object", discovery.DeviceName(identity.ID))
		case resolution.deferred:
			// The prior incarnation of this id still holds the
			// deterministic name within its grace window; recreation
			// happens once grace prunes the old object.
			r.eventf(single, corev1.EventTypeNormal, "DeviceRecreationDeferred", "Deferred",
				"identity %q waits for the node grace of its prior incarnation", identity.ID)
		default:
			if err := r.createDevice(ctx, single, state, identity, resolution.name, cfg.class, taken); err != nil {
				return err
			}
		}
	}
	return nil
}

// nameResolution is the outcome of deriving an object name for a new
// identity.
type nameResolution struct {
	// name is the object name to create.
	name string
	// deferred reports a name still held by a prior incarnation of the
	// same id (node replacement within grace).
	deferred bool
	// collides reports a name held by an object this controller does not
	// own; it is never adopted.
	collides bool
}

// resolveDeviceName derives the deterministic object name of a new
// identity given the names already taken. The base name is a function of
// the identity id only; a base held by another of our identities gets the
// deterministic hash suffix, and a base held by a prior incarnation of
// the same id defers creation until grace prunes it.
func resolveDeviceName(
	state *inventoryState,
	identity discovery.Identity,
	taken map[string]string,
) nameResolution {
	base := discovery.DeviceName(identity.ID)
	holder, exists := state.all[base]
	if !exists {
		return nameResolution{name: base}
	}
	if _, ours := state.ours[base]; !ours {
		return nameResolution{collides: true}
	}
	if holder.Spec.Identity.ID == identity.ID {
		// Same id, different device key: the prior incarnation (for
		// example the same UUID on a replaced node) still owns the name.
		return nameResolution{deferred: true}
	}
	// Slug collision between two different ids of ours: disambiguate
	// with the deterministic hash suffix.
	hashed := discovery.ResolveDeviceName(identity.ID, taken)
	if _, alsoTaken := state.all[hashed]; alsoTaken {
		return nameResolution{collides: true}
	}
	return nameResolution{name: hashed}
}

// createDevice creates a new owned HardwareDevice with the singleton
// controller reference. Creation goes through the primary endpoint only;
// the device starts with no status — the device controller is its only
// writer.
func (r *Reconciler) createDevice(
	ctx context.Context,
	single *infrastructurev1alpha1.HardwareDiscovery,
	state *inventoryState,
	identity discovery.Identity,
	name string,
	class string,
	taken map[string]string,
) error {
	spec, err := deviceSpec(class, identity)
	if err != nil {
		return err
	}
	device := &infrastructurev1alpha1.HardwareDevice{
		ObjectMeta: deviceObjectMeta(name, class, identity.NodeName),
		Spec:       spec,
	}
	if err := controllerutil.SetControllerReference(single, device, r.Scheme); err != nil {
		return fmt.Errorf("inventory: setting controller reference on hardware device %q: %w", name, err)
	}
	if err := r.Create(ctx, device); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// A concurrent writer took the name; requeue re-evaluates.
			return fmt.Errorf("inventory: hardware device %q already exists", name)
		}
		return fmt.Errorf("inventory: creating hardware device %q: %w", name, err)
	}
	state.add(device)
	taken[name] = identity.ID
	r.eventf(device, corev1.EventTypeNormal, "DeviceCreated", "Created",
		"created hardware device for identity %q on node %q", identity.ID, identity.NodeName)
	return nil
}

// ensureDevice reconciles an existing owned device with its desired
// identity: spec, service-written labels and the grace annotation
// (recovery removes it). Nothing is written when everything already
// matches. Updates go through the primary endpoint only and never touch
// the status subresource.
func (r *Reconciler) ensureDevice(
	ctx context.Context,
	existing *infrastructurev1alpha1.HardwareDevice,
	identity discovery.Identity,
	class string,
) error {
	spec, err := deviceSpec(class, identity)
	if err != nil {
		return err
	}

	changed := false
	if !apiequality.Semantic.DeepEqual(existing.Spec, spec) {
		existing.Spec = spec
		changed = true
	}
	if merged, labelsChanged := mergeDeviceLabels(existing.Labels, class, identity.NodeName); labelsChanged {
		existing.Labels = merged
		changed = true
	}
	if _, orphaned := existing.Annotations[AnnotationNodeOrphanedAt]; orphaned {
		delete(existing.Annotations, AnnotationNodeOrphanedAt)
		changed = true
	}
	if !changed {
		return nil
	}

	if err := r.Update(ctx, existing); err != nil {
		return fmt.Errorf("inventory: updating hardware device %q: %w", existing.Name, err)
	}
	r.eventf(existing, corev1.EventTypeNormal, "DeviceUpdated", "Updated",
		"reconciled hardware device spec for identity %q", identity.ID)
	return nil
}

// mergeDeviceLabels sets the service-written label keys to their
// deterministic values, preserving every other label. It reports whether
// anything changed.
func mergeDeviceLabels(existing map[string]string, class, node string) (map[string]string, bool) {
	merged := existing
	if merged == nil {
		merged = make(map[string]string, 2)
	}
	changed := false
	for key, value := range discovery.DeviceLabels(class, node) {
		if merged[key] != value {
			merged[key] = value
			changed = true
		}
	}
	return merged, changed
}

// deviceObjectMeta builds the metadata of a new device.
func deviceObjectMeta(name, class, node string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:   name,
		Labels: discovery.DeviceLabels(class, node),
	}
}

// deviceSpec maps a discovered identity to the HardwareDevice spec. The
// locator mapping is the single place where the source DTO meets the API
// types.
func deviceSpec(class string, identity discovery.Identity) (infrastructurev1alpha1.HardwareDeviceSpec, error) {
	source, err := deviceSource(identity.Locator)
	if err != nil {
		return infrastructurev1alpha1.HardwareDeviceSpec{}, err
	}
	return infrastructurev1alpha1.HardwareDeviceSpec{
		Class: class,
		Identity: infrastructurev1alpha1.DeviceIdentity{
			Kind: string(identity.Kind),
			ID:   identity.ID,
		},
		Host: infrastructurev1alpha1.DeviceHost{
			Name: identity.NodeName,
			UID:  identity.NodeUID,
		},
		Source: source,
	}, nil
}

// deviceSource maps a source locator to the spec.source arm of its kind.
func deviceSource(locator discovery.Locator) (infrastructurev1alpha1.DeviceSource, error) {
	switch typed := locator.(type) {
	case dra.Locator:
		return infrastructurev1alpha1.DeviceSource{
			Kind: dra.SourceID,
			DRA: &infrastructurev1alpha1.DRASource{
				Driver: typed.Driver,
				Pool:   typed.Pool,
				Device: typed.Device,
			},
		}, nil
	case classic.Locator:
		return infrastructurev1alpha1.DeviceSource{
			Kind: classic.SourceID,
			Classic: &infrastructurev1alpha1.ClassicSource{
				Node:  typed.Node,
				Index: typed.Index,
			},
		}, nil
	default:
		return infrastructurev1alpha1.DeviceSource{},
			fmt.Errorf("inventory: unsupported source locator kind %q", locator.Kind())
	}
}

// sortedDesiredKeys returns the desired keys in a deterministic order so
// name assignment within one pass is stable: device keys order by id,
// then kind, then node UID.
func sortedDesiredKeys(desired map[deviceKey]discovery.Identity) []deviceKey {
	keys := make([]deviceKey, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b deviceKey) int {
		if c := strings.Compare(a.id, b.id); c != 0 {
			return c
		}
		if c := strings.Compare(string(a.kind), string(b.kind)); c != 0 {
			return c
		}
		return strings.Compare(string(a.nodeUID), string(b.nodeUID))
	})
	return keys
}
