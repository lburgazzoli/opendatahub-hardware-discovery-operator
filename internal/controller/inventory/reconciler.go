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

// Package inventory contains the singleton HardwareDiscovery controller
// (design doc §6/§10): it turns the singleton's discovery configuration
// into a set of owned HardwareDevice identity specs, prunes devices after
// authoritative disappearance, deliberate selector removal or node-grace
// expiry, and reports the singleton status through the framework
// reconciler. It is the only writer of HardwareDevice specs and metadata;
// device observations (the /status subresource) belong to the per-device
// controller.
package inventory

import (
	"context"
	"fmt"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	fwconditions "github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	fwhandlers "github.com/opendatahub-io/odh-platform-utilities/framework/controller/handlers"
	fwreconciler "github.com/opendatahub-io/odh-platform-utilities/framework/controller/reconciler"
)

const (
	// SingletonName is the only legal HardwareDiscovery instance name
	// (enforced by the CRD's root-schema CEL rule).
	SingletonName = "cluster"

	// ControllerName is the controller/field-manager/recorder identity of
	// this controller: the framework builder's instance name.
	ControllerName = "inventory"

	// AnnotationNodeOrphanedAt persists the grace start of a device whose
	// hosting node is absent or was recreated (gate decision D5). The
	// deadline lives in Kubernetes state so controller restarts cannot
	// reset it; recovery removes it.
	AnnotationNodeOrphanedAt = discovery.LabelPrefix + "/node-orphaned-at"

	// classAccelerator is the only device class enabled in this PoC.
	classAccelerator = "accelerator"

	// defaultRescanPeriod bounds the periodic rescan that runs even
	// without input events (design doc §10.1).
	defaultRescanPeriod = 5 * time.Minute

	// defaultNodeGracePeriod is how long devices of an absent or
	// UID-changed node are retained before pruning.
	defaultNodeGracePeriod = 15 * time.Minute
)

// Compile-time proof that the singleton satisfies the framework platform
// object contract (fwapi.PlatformObject) through its embedded status.
var _ fwapi.PlatformObject = &infrastructurev1alpha1.HardwareDiscovery{}

// SingletonGroupKind is the GroupKind of the reconciled HardwareDiscovery
// singleton; it keys the manager's per-kind scan serialization.
var SingletonGroupKind = schema.GroupKind{
	Group: infrastructurev1alpha1.GroupVersion.Group,
	Kind:  "HardwareDiscovery",
}

// Controller reconciles the fixed HardwareDiscovery singleton named
// "cluster" through the framework builder. One reconciliation is one
// bounded scan: resolve the selected nodes, enumerate the registered
// sources per node, prune what authoritative evidence no longer covers,
// and materialize what it newly covers. The framework derives and
// persists the singleton status; discovery-specific child lifecycle stays
// in the scan action.
type Controller struct {
	// Sources builds the ordered source registry for one singleton
	// configuration. It is re-evaluated on every scan so configuration
	// changes take effect without restart. Required.
	Sources SourceFactory
	// Clock is the collector clock used for grace bookkeeping. Defaults
	// to time.Now; tests inject a fake.
	Clock func() time.Time
}

// +kubebuilder:rbac:groups=infrastructure.platform.opendatahub.io,resources=hardwarediscoveries,verbs=get;list;watch

// +kubebuilder:rbac:groups=infrastructure.platform.opendatahub.io,resources=hardwarediscoveries/status,verbs=get;update;patch

// +kubebuilder:rbac:groups=infrastructure.platform.opendatahub.io,resources=hardwaredevices,verbs=get;list;watch;create;update;patch;delete

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// The DRA source reads ResourceSlices through its injected reader with
// scan-driven direct reads (no informer, no watch).
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceslices,verbs=get;list

// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// NewReconciler builds and registers the inventory controller through the
// framework builder (ReconcilerFor → ReconcilerBuilder → Build) and
// returns its framework reconciler. Ready aggregates Degraded (healthy
// when False) and the framework's default ProvisioningSucceeded (healthy
// when True); the post-status hook finalizes the derived Ready condition
// without overriding an unhealthy value. No finalizer, render, deploy, GC
// or dynamic-ownership actions are registered, so the reconciler never
// adds a finalizer to the singleton. The event recorder is obtained from
// the manager under the controller name before the scan dependencies are
// wired; no raw reconciler construction is needed to access it.
func (c *Controller) NewReconciler(ctx context.Context, mgr ctrl.Manager) (*fwreconciler.Reconciler, error) {
	if c.Sources == nil {
		return nil, fmt.Errorf("inventory: source factory must not be nil")
	}

	// The builder converts the primary and secondary watches to the
	// unstructured cache representation itself; typed objects are
	// supplied here only for GVK resolution.
	b := fwreconciler.ReconcilerFor(
		mgr,
		&infrastructurev1alpha1.HardwareDiscovery{},
		ctrlbuilder.WithPredicates(predicate.GenerationChangedPredicate{}),
	).
		WithInstanceName(ControllerName).
		WithConditions(fwconditions.Dependent(
			fwapi.ConditionType(ConditionTypeDegraded),
			fwconditions.HealthyWhenFalse,
		)).
		WithPostStatusFn(postStatus).
		WithAction(scanAction(scanDeps{
			recorder: mgr.GetEventRecorder(ControllerName),
			sources:  c.Sources,
			clock:    c.Clock,
		})).
		// The inventory controller never writes device status, so
		// status-only device updates (another writer) must not
		// re-trigger scans; spec changes always bump the generation.
		Watches(&infrastructurev1alpha1.HardwareDevice{},
			fwreconciler.WithPredicates(predicate.GenerationChangedPredicate{}),
			fwreconciler.WithEventHandler(fwhandlers.ToNamed(SingletonName)),
		).
		// Node evidence and lifecycle changes, filtered by
		// nodePredicate; node selection and plane resolution are
		// re-evaluated in reconcile logic, never at the cache.
		Watches(&corev1.Node{},
			fwreconciler.WithPredicates(nodePredicate(mgr.GetScheme())),
			fwreconciler.WithEventHandler(fwhandlers.ToNamed(SingletonName)),
		)

	r, err := b.Build(ctx)
	if err != nil {
		return nil, fmt.Errorf("inventory: building framework reconciler: %w", err)
	}

	return r, nil
}

// SetupWithManager registers the inventory controller through the
// framework builder: the primary HardwareDiscovery watch on spec
// generation changes, and two explicit non-owned secondary watches —
// HardwareDevice generation/create/delete events and Node
// evidence/lifecycle events — both routed to the singleton "cluster"
// with the framework's ToNamed handler. Source inputs are read
// scan-driven, never watched. Scans are serialized through the manager's
// per-kind concurrency (ManagerControllerOptions), not a controller
// option: one singleton, one writer.
func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
	// The pinned framework Build accepts a context for future use only;
	// registration itself performs no context-bound work.
	_, err := c.NewReconciler(context.Background(), mgr)
	return err
}

// ManagerControllerOptions stamps the manager's controller options with
// the inventory scan serialization: one singleton, one writer, so
// concurrent reconciles cannot interleave inventory writes. The positive
// per-kind value takes precedence over the manager's global concurrency
// default in controller-runtime v0.25.2, leaving every other controller
// (including the device controller) at the global default. Unrelated
// settings, including per-kind entries of other controllers, are
// preserved.
func ManagerControllerOptions(base ctrlconfig.Controller) ctrlconfig.Controller {
	concurrency := make(map[string]int, len(base.GroupKindConcurrency)+1)
	maps.Copy(concurrency, base.GroupKindConcurrency)
	concurrency[SingletonGroupKind.String()] = 1

	base.GroupKindConcurrency = concurrency
	return base
}
