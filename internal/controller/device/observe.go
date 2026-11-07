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

package device

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/classic"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/dra"
)

// config is the validated singleton configuration of one observation.
type config struct {
	class    infrastructurev1alpha1.DeviceClass
	selector labels.Selector
	sources  discovery.Sources
	maxStale time.Duration
}

// evaluateConfig validates the singleton spec and builds the observation
// configuration: class, node selection, the source registry and the
// heartbeat bound. Node selection defaults to every node; a nil or empty
// LabelSelector matches all eligible nodes.
func evaluateConfig(
	single *infrastructurev1alpha1.HardwareDiscovery,
	factory SourceFactory,
) (config, error) {
	cfg := config{
		maxStale: durationOr(single.Spec.MaxStale.Duration, defaultMaxStale),
		class:    single.Spec.Class,
	}
	selector, err := nodeSelector(single.Spec.NodeSelector)
	if err != nil {
		return cfg, fmt.Errorf("spec.nodeSelector is invalid: %w", err)
	}
	cfg.selector = selector

	sources, err := factory(single.Spec.Sources)
	if err != nil {
		return cfg, err
	}
	cfg.sources = sources
	return cfg, nil
}

// nodeSelector resolves the singleton's node selection. An omitted or
// empty selector matches all nodes: metav1.LabelSelectorAsSelector
// alone would turn an explicit empty selector into "nothing", which is
// not the approved default.
func nodeSelector(spec *metav1.LabelSelector) (labels.Selector, error) {
	if spec == nil || len(spec.MatchLabels) == 0 && len(spec.MatchExpressions) == 0 {
		return labels.Everything(), nil
	}
	return metav1.LabelSelectorAsSelector(spec)
}

// durationOr returns d when positive, otherwise the fallback.
func durationOr(d time.Duration, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

// standDown and its zero-write semantics are gone: the eligibility
// revalidation lives in ineligibility (action.go) and reports a
// diagnostic Observed=False instead, so the framework derives
// Ready=False rather than retaining a misleading old Ready=True. The
// writeObservation/reportObservationFailure status writers are gone
// too: the observation action mutates the typed instance and its
// condition manager, and the framework reconciler's status apply is
// the only status writer.

// resolveIdentity reconstructs the discovery identity the spec records
// and resolves the source that must observe it. The locator mapping is
// the single place where the API's source arm meets the source DTO.
func resolveIdentity(
	device *infrastructurev1alpha1.HardwareDevice,
	cfg config,
) (discovery.Source, discovery.Identity, error) {
	source := sourceByID(cfg.sources, string(device.Spec.Source.Kind))
	if source == nil {
		return nil, discovery.Identity{}, fmt.Errorf(
			"device: source %q of hardware device %q is not registered",
			device.Spec.Source.Kind, device.Name)
	}

	locator, err := locatorFromSpec(device.Spec.Source)
	if err != nil {
		return nil, discovery.Identity{}, fmt.Errorf(
			"device: hardware device %q has an invalid source locator: %w", device.Name, err)
	}

	identity := discovery.Identity{
		ID:       device.Spec.Identity.ID,
		Kind:     discovery.IdentityKind(device.Spec.Identity.Kind),
		NodeName: device.Spec.Host.Name,
		NodeUID:  device.Spec.Host.UID,
		Plane:    source.Plane(),
		Locator:  locator,
	}
	if err := identity.Validate(); err != nil {
		// The CEL rules (exactly one matching source arm, immutable
		// identity/host, classic node equals host.name) make a malformed
		// spec unreachable through the API; surfacing it as a reconcile
		// error keeps it loud instead of silently skipping.
		return nil, discovery.Identity{}, fmt.Errorf(
			"device: hardware device %q has an invalid identity: %w", device.Name, err)
	}
	return source, identity, nil
}

// locatorFromSpec maps the spec's source arm to the source locator of
// its kind.
func locatorFromSpec(spec infrastructurev1alpha1.DeviceSource) (discovery.Locator, error) {
	switch {
	case spec.Kind == infrastructurev1alpha1.DeviceSourceKindDRA && spec.DRA != nil:
		return dra.Locator{Driver: spec.DRA.Driver, Pool: spec.DRA.Pool, Device: spec.DRA.Device}, nil
	case spec.Kind == infrastructurev1alpha1.DeviceSourceKindClassic && spec.Classic != nil:
		return classic.Locator{Node: spec.Classic.Node, Index: spec.Classic.Index}, nil
	default:
		return nil, fmt.Errorf(
			"source kind %q with a missing or mismatched arm", spec.Kind)
	}
}

// sourceByID finds the registered source of one kind.
func sourceByID(sources discovery.Sources, id string) discovery.Source {
	for _, src := range sources.List() {
		if src.ID() == id {
			return src
		}
	}
	return nil
}

// sourceRef renders the provenance of one identity:
// "<source-kind>/<locator ref>", for example
// dra/gpu.nvidia.com/<pool> or classic/<node>. It is replaced, never
// merged, on an observed source change.
func sourceRef(kind string, locator discovery.Locator) string {
	return kind + "/" + locator.Ref()
}
