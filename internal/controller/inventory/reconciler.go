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
// expiry, and writes the singleton status. It is the only writer of
// HardwareDevice specs and metadata; device observations (the /status
// subresource) belong to the per-device controller.
package inventory

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
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

// Reconciler reconciles the fixed HardwareDiscovery singleton named
// "cluster". It owns the singleton status and the spec/metadata/lifecycle
// of the HardwareDevice children; device observations are a separate
// writer's concern. One reconciliation is one bounded scan: resolve the
// selected nodes, enumerate the registered sources per node, prune what
// authoritative evidence no longer covers, and materialize what it newly
// covers.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder emits lifecycle events on children and configuration
	// issues on the singleton. Defaults to the manager recorder in
	// SetupWithManager; nil disables event emission.
	Recorder recorder.EventRecorder
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

// Reconcile runs one discovery scan for the singleton. Errors are
// reserved for transient read/write failures (normal backoff);
// authoritative evaluation problems surface as conditions instead.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if req.Name != SingletonName {
		// The CEL rule makes other names impossible through the API;
		// anything else is not ours to reconcile.
		return ctrl.Result{}, nil
	}

	single := &infrastructurev1alpha1.HardwareDiscovery{}
	if err := r.Get(ctx, client.ObjectKey{Name: SingletonName}, single); err != nil {
		if apierrors.IsNotFound(err) {
			// Children are garbage collected through the controller
			// reference; there is nothing for this controller to do.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("inventory: fetching singleton %q: %w", SingletonName, err)
	}

	cfg, err := r.evaluateConfig(single)
	if err != nil {
		// Invalid configuration never prunes children: the evaluation is
		// not trustworthy. Surface an actionable condition instead.
		issues := issueLog{}
		issues.add(reasonInvalidConfiguration, priorityInvalidConfiguration,
			"invalid discovery configuration: %v", err)
		if err := r.writeStatus(ctx, single, nil, nil, issues.issues); err != nil {
			return ctrl.Result{}, err
		}
		r.eventf(single, corev1.EventTypeWarning, reasonInvalidConfiguration, "Reconciled",
			"invalid discovery configuration: %v", err)
		return ctrl.Result{RequeueAfter: cfg.rescanPeriod}, nil
	}

	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return ctrl.Result{}, fmt.Errorf("inventory: listing nodes: %w", err)
	}
	devices := &infrastructurev1alpha1.HardwareDeviceList{}
	if err := r.List(ctx, devices); err != nil {
		return ctrl.Result{}, fmt.Errorf("inventory: listing hardware devices: %w", err)
	}

	state := newState(single, devices.Items)
	issues := issueLog{}
	selected := r.selectNodes(ctx, cfg, nodes.Items, &issues)
	if err := ctx.Err(); err != nil {
		// A cancelled scan must never be written as an authoritative
		// partial evaluation.
		return ctrl.Result{}, err
	}
	// The ambiguous id set feeds issue reporting only: ambiguous
	// identities are already rejected from the desired set.
	desired, _ := desiredIdentities(selected, &issues)

	// Deletion before creation: names freed by pruning become available
	// to new incarnations in the same pass.
	requeue, err := r.prune(ctx, cfg, state, selected, desired, nodes.Items)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.materialize(ctx, single, cfg, state, desired, &issues); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.writeStatus(ctx, single, selected, state, issues.issues); err != nil {
		return ctrl.Result{}, err
	}

	logger.V(1).Info("inventory scan complete",
		"nodes", len(selected), "devices", len(state.ours), "issues", len(issues.issues))

	return ctrl.Result{RequeueAfter: requeue}, nil
}

// SetupWithManager registers the inventory controller. It watches the
// singleton (spec generation changes), the owned inventory (spec/lifecycle
// changes map back to the singleton) and Nodes (label and extended-resource
// evidence changes map to the singleton); source inputs are read
// scan-driven, never watched.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Sources == nil {
		return fmt.Errorf("inventory: source factory must not be nil")
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder(ControllerName)
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		For(&infrastructurev1alpha1.HardwareDiscovery{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&infrastructurev1alpha1.HardwareDevice{},
			handler.EnqueueRequestsFromMapFunc(singletonRequest),
			// The inventory controller never writes device status, so
			// status-only updates (another writer) must not re-trigger
			// scans; spec changes always bump the generation.
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(singletonRequest),
			builder.WithPredicates(nodePredicate())).
		// One singleton, one writer: scans are serialized so concurrent
		// reconciles cannot interleave inventory writes.
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// singletonRequest maps every child or Node event to the singleton
// request; node selection and plane resolution are re-evaluated in
// reconcile logic, never filtered at the cache.
func singletonRequest(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: SingletonName},
	}}
}

// now returns the collector clock, defaulting to the system clock.
func (r *Reconciler) now() time.Time {
	if r.Clock == nil {
		return time.Now()
	}
	return r.Clock()
}

// eventf emits an event when a recorder is configured. The action is the
// UpperCamelCase verb of what this controller did to the regarding
// object (events.k8s.io/v1 semantics).
func (r *Reconciler) eventf(obj runtime.Object, eventtype, reason, action, format string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(obj, nil, eventtype, reason, action, format, args...)
}
