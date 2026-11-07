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

// Package device contains the per-device observation controller (design
// doc §6.3): it resolves the identity and source locator recorded in a
// HardwareDevice spec, revalidates them against the current cluster
// state, observes exactly that device, and writes only its status
// through the /status subresource. It never writes the primary spec,
// never creates or deletes objects, and never re-enumerates counts into
// new devices. Facts are whole-GPU inventory — never health and never
// free allocation capacity.
package device

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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/inventory"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
)

const (
	// ControllerName is the controller-runtime registration name.
	ControllerName = "device"

	// ConditionTypeObserved reports the outcome of the last observation
	// attempt: collector observations, not hardware health and not free
	// allocation capacity (design doc §4.3).
	ConditionTypeObserved = "Observed"
)

// Condition reasons of the device observation condition (design doc
// §4.3/§6.5). They describe the collector, never the hardware.
const (
	// reasonObserved reports a successful observation.
	reasonObserved = "Observed"
	// reasonSourceUnavailable reports an observation that could not be
	// completed (design doc §6.5 device-level umbrella reason).
	reasonSourceUnavailable = "SourceUnavailable"
	// reasonSourceForbidden reports a forbidden source read.
	reasonSourceForbidden = "SourceForbidden"
	// reasonPoolIncomplete reports an incomplete DRA pool snapshot.
	reasonPoolIncomplete = "PoolIncomplete"
)

// defaultMaxStale is the heartbeat bound for unchanged facts when the
// singleton omits spec.maxStale (design doc §5.1).
const defaultMaxStale = 10 * time.Minute

// SourceFactory builds the ordered source registry for one singleton
// configuration. It is the same contract (and the same production
// constructor, inventory.NewSourceFactory) the inventory controller
// uses, so both controllers resolve devices through identically
// configured sources; the device controller never constructs its own
// source semantics.
type SourceFactory = inventory.SourceFactory

// Reconciler reconciles one HardwareDevice. It is the only writer of
// HardwareDevice status; specs, labels and lifecycle belong to the
// inventory controller. One reconciliation observes at most one device:
// revalidate the spec against current config and cluster state, observe
// through the source, and persist fresh facts — or retain everything and
// report the failure. The collector clock lives in the sources (injected
// at construction); every time comparison in this controller is derived
// from observation stamps, never from a wall clock read here.

// Compile-time proof that the device satisfies the framework platform
// object contract (fwapi.PlatformObject) through its embedded status.
var _ fwapi.PlatformObject = &infrastructurev1alpha1.HardwareDevice{}

type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Sources builds the source registry for the current singleton
	// configuration. It is re-evaluated on every reconcile so source
	// enablement and settings transitions take effect without restart.
	// Required.
	Sources SourceFactory
}

// +kubebuilder:rbac:groups=infrastructure.platform.opendatahub.io,resources=hardwaredevices,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.platform.opendatahub.io,resources=hardwaredevices/status,verbs=get;update;patch

// The singleton is read to respect the current class, node selector,
// source enablement and maxStale during configuration transitions.
// +kubebuilder:rbac:groups=infrastructure.platform.opendatahub.io,resources=hardwarediscoveries,verbs=get;list;watch

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// The DRA source reads ResourceSlices through its injected reader with
// scan-driven direct reads (no informer, no watch).
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceslices,verbs=get;list

// Reconcile observes one device. Errors are reserved for transient
// read/write failures (normal backoff); observation problems surface as
// the Observed condition, and stand-downs (deselected, different
// incarnation, disabled source) are plain successes without writes.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	device := &infrastructurev1alpha1.HardwareDevice{}
	if err := r.Get(ctx, req.NamespacedName, device); err != nil {
		if apierrors.IsNotFound(err) {
			// A deleted device is reconcile success, never recreation.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("device: fetching hardware device %q: %w", req.Name, err)
	}
	if !device.DeletionTimestamp.IsZero() {
		// The object is going away; observing it cannot help anyone.
		return ctrl.Result{}, nil
	}

	single := &infrastructurev1alpha1.HardwareDiscovery{}
	if err := r.Get(ctx, client.ObjectKey{Name: inventory.SingletonName}, single); err != nil {
		if apierrors.IsNotFound(err) {
			// Reset path: the owner reference lets real garbage
			// collection remove the children. Nothing to observe
			// against.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("device: fetching singleton %q: %w", inventory.SingletonName, err)
	}

	cfg, err := evaluateConfig(single, r.Sources)
	if err != nil {
		// An invalid singleton configuration is not trustworthy: stand
		// down without touching device status. The inventory controller
		// reports the condition; the config watch re-enqueues every
		// device once it is fixed, and this requeue is the safety net.
		logger.V(1).Info("standing down: singleton configuration is invalid", "error", err)
		return ctrl.Result{RequeueAfter: defaultMaxStale}, nil
	}

	standDown, err := r.standDown(ctx, device, cfg)
	if err != nil {
		return ctrl.Result{}, err
	}
	if standDown {
		// A stale queued request must never freshen a deselected or
		// different-incarnation device: no observation, no write. The
		// inventory controller owns the lifecycle; watches re-enqueue
		// this device when the situation changes.
		logger.V(1).Info("standing down: device is not currently discoverable",
			"device", device.Name)
		return ctrl.Result{}, nil
	}

	source, identity, err := resolveIdentity(device, cfg)
	if err != nil {
		return ctrl.Result{}, err
	}

	observation, err := source.Observe(ctx, identity)
	if err != nil {
		return r.reportObservationFailure(ctx, device, err)
	}
	if err := observation.Validate(); err != nil {
		// A contract-violating observation is a failed observation,
		// never a trusted partial one.
		return r.reportObservationFailure(ctx, device, err)
	}

	return r.writeObservation(ctx, device, cfg, identity, observation)
}

// SetupWithManager registers the device controller. It watches devices
// (spec generation changes; this controller's own /status writes never
// bump the generation, so they cannot loop), the singleton (a
// configuration change re-evaluates every device) and Nodes (evidence
// changes re-observe the devices hosted there). Source inputs are read
// scan-driven, never watched.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Sources == nil {
		return fmt.Errorf("device: source factory must not be nil")
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		For(&infrastructurev1alpha1.HardwareDevice{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&infrastructurev1alpha1.HardwareDiscovery{},
			handler.EnqueueRequestsFromMapFunc(r.allDevices),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.devicesOnNode),
			builder.WithPredicates(nodeEvidencePredicate())).
		Complete(r)
}

// allDevices maps a singleton configuration change to every device:
// class, selector, source enablement and maxStale transitions must be
// re-evaluated everywhere.
func (r *Reconciler) allDevices(ctx context.Context, _ client.Object) []reconcile.Request {
	devices := &infrastructurev1alpha1.HardwareDeviceList{}
	if err := r.List(ctx, devices); err != nil {
		log.FromContext(ctx).Error(err, "device: listing hardware devices for configuration change")
		return nil
	}
	return deviceRequests(devices.Items)
}

// devicesOnNode maps a Node evidence change to the devices hosted on
// that node. The service-written node label is the index; the full node
// name always remains in spec.host.name.
func (r *Reconciler) devicesOnNode(ctx context.Context, node client.Object) []reconcile.Request {
	devices := &infrastructurev1alpha1.HardwareDeviceList{}
	if err := r.List(ctx, devices,
		client.MatchingLabels{discovery.LabelNode: discovery.NodeLabelValue(node.GetName())}); err != nil {
		log.FromContext(ctx).Error(err, "device: listing hardware devices for node change",
			"node", node.GetName())
		return nil
	}
	return deviceRequests(devices.Items)
}

// deviceRequests renders list items as reconcile requests.
func deviceRequests(items []infrastructurev1alpha1.HardwareDevice) []reconcile.Request {
	requests := make([]reconcile.Request, 0, len(items))
	for i := range items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: items[i].Name},
		})
	}
	return requests
}
