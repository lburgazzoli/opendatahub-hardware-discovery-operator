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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	fwconditions "github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	fwreconciler "github.com/opendatahub-io/odh-platform-utilities/framework/controller/reconciler"
	fwresources "github.com/opendatahub-io/odh-platform-utilities/framework/resources"
)

const (
	// SingletonName is the only legal HardwareDiscovery instance name
	// (enforced by the CRD's root-schema CEL rule).
	SingletonName = "cluster"

	// ControllerName is the controller-runtime registration name.
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

// Controller reconciles the fixed HardwareDiscovery singleton named
// "cluster" through the framework reconciler. One reconciliation is one
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

// NewReconciler builds the framework reconciler for the singleton with
// the inventory scan action attached. Ready aggregates Degraded
// (healthy when False) and the framework's ProvisioningSucceeded
// (healthy when True); the post-status hook finalizes the derived Ready
// condition without overriding an unhealthy value. No finalizer,
// render, deploy, GC or dynamic-ownership actions are registered, so the
// reconciler never adds a finalizer to the singleton.
func (c *Controller) NewReconciler(mgr ctrl.Manager) (*fwreconciler.Reconciler, error) {
	if c.Sources == nil {
		return nil, fmt.Errorf("inventory: source factory must not be nil")
	}

	aggregator, err := fwconditions.NewAggregator(
		fwapi.ConditionType(ConditionTypeReady),
		fwconditions.Dependent(fwapi.ConditionType(ConditionTypeDegraded), fwconditions.HealthyWhenFalse),
		fwconditions.Dependent(fwapi.ConditionTypeProvisioningSucceeded, fwconditions.HealthyWhenTrue),
	)
	if err != nil {
		return nil, fmt.Errorf("inventory: building condition aggregator: %w", err)
	}

	r, err := fwreconciler.NewReconciler(
		mgr,
		ControllerName,
		&infrastructurev1alpha1.HardwareDiscovery{},
		fwreconciler.WithConditionAggregator(aggregator),
		fwreconciler.WithPostStatusFn(postStatus),
	)
	if err != nil {
		return nil, fmt.Errorf("inventory: building framework reconciler: %w", err)
	}

	r.AddAction(scanAction(scanDeps{
		recorder: r.Recorder,
		sources:  c.Sources,
		clock:    c.Clock,
	}))

	return r, nil
}

// SetupWithManager registers the inventory controller. It watches the
// singleton (spec generation changes), the owned inventory (spec/lifecycle
// changes map back to the singleton) and Nodes (label and extended-resource
// evidence changes map to the singleton); source inputs are read
// scan-driven, never watched. Every cached GVK is watched as an
// unstructured object matching the framework client's cache
// representation, so no second informer per GVK is created. Scans are
// serialized: one singleton, one writer.
func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
	r, err := c.NewReconciler(mgr)
	if err != nil {
		return err
	}

	watches, err := newWatchSet(mgr.GetScheme())
	if err != nil {
		return err
	}

	b := ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		For(watches.singleton.object, builder.WithPredicates(watches.singleton.predicate)).
		Watches(watches.devices.object, watches.devices.handler, builder.WithPredicates(watches.devices.predicate)).
		Watches(watches.nodes.object, watches.nodes.handler, builder.WithPredicates(watches.nodes.predicate)).
		// One singleton, one writer: concurrent reconciles cannot
		// interleave inventory writes.
		WithOptions(controller.Options{MaxConcurrentReconciles: 1})

	return b.Complete(r)
}

// watchRegistration is one watch handed to the controller-runtime
// builder: the watched object representation, the event handler and the
// event predicate.
type watchRegistration struct {
	object    client.Object
	handler   handler.EventHandler
	predicate predicate.Predicate
}

// watchSet is the controller's complete registration set. The singleton
// is the primary (For) watch; children and Nodes are mapped watches.
type watchSet struct {
	singleton watchRegistration
	devices   watchRegistration
	nodes     watchRegistration
}

// newWatchSet derives the registration set for a scheme. Every watched
// GVK resolves to an unstructured object so the watches populate the
// same informer cache the framework client reads through.
func newWatchSet(scheme *runtime.Scheme) (watchSet, error) {
	singleton, err := unstructuredWatch(scheme, &infrastructurev1alpha1.HardwareDiscovery{})
	if err != nil {
		return watchSet{}, err
	}
	devices, err := unstructuredWatch(scheme, &infrastructurev1alpha1.HardwareDevice{})
	if err != nil {
		return watchSet{}, err
	}
	nodes, err := unstructuredWatch(scheme, &corev1.Node{})
	if err != nil {
		return watchSet{}, err
	}

	return watchSet{
		singleton: watchRegistration{
			object: singleton,
			// Spec changes always bump the singleton's generation.
			predicate: predicate.GenerationChangedPredicate{},
		},
		devices: watchRegistration{
			object:  devices,
			handler: handler.EnqueueRequestsFromMapFunc(singletonRequest),
			// The inventory controller never writes device status, so
			// status-only updates (another writer) must not re-trigger
			// scans; spec changes always bump the generation.
			predicate: predicate.GenerationChangedPredicate{},
		},
		nodes: watchRegistration{
			object:  nodes,
			handler: handler.EnqueueRequestsFromMapFunc(singletonRequest),
			// Node evidence and lifecycle changes, filtered by
			// nodePredicate; node selection and plane resolution are
			// re-evaluated in reconcile logic, never at the cache.
			predicate: nodePredicate(),
		},
	}, nil
}

// unstructuredWatch resolves an object's GVK in the scheme and returns
// the unstructured watch representation of that GVK.
func unstructuredWatch(scheme *runtime.Scheme, object client.Object) (client.Object, error) {
	gvk, err := apiutil.GVKForObject(object, scheme)
	if err != nil {
		return nil, fmt.Errorf("inventory: resolving watch gvk of %T: %w", object, err)
	}
	return fwresources.GvkToUnstructured(gvk), nil
}

// singletonRequest maps every child or Node event to the singleton
// request; node selection and plane resolution are re-evaluated in
// reconcile logic, never filtered at the cache.
func singletonRequest(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: SingletonName},
	}}
}
