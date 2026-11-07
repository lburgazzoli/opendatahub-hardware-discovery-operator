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
// state, observes exactly that device, and derives its readiness through
// the framework reconciler. It never writes the primary spec, never
// creates or deletes objects, and never re-enumerates counts into new
// devices; the framework's status apply is the only status writer. Facts
// are whole-GPU inventory — never health and never free allocation
// capacity.
package device

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/inventory"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	fwclient "github.com/opendatahub-io/odh-platform-utilities/framework/client"
	fwconditions "github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	fwreconciler "github.com/opendatahub-io/odh-platform-utilities/framework/controller/reconciler"
)

const (
	// ControllerName is the controller/field-manager/recorder identity of
	// this controller: the framework builder's instance name.
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
	// reasonConfigurationMissing reports an observation attempted with
	// no singleton to observe against.
	reasonConfigurationMissing = "ConfigurationMissing"
	// reasonInvalidConfiguration reports an untrustworthy singleton
	// configuration or a malformed device spec.
	reasonInvalidConfiguration = "InvalidConfiguration"
	// reasonClassMismatch reports a device whose class moved outside the
	// singleton's discovered class.
	reasonClassMismatch = "ClassMismatch"
	// reasonSourceDisabled reports a device whose source is disabled in
	// the current singleton configuration.
	reasonSourceDisabled = "SourceDisabled"
	// reasonNodeNotFound reports a device whose hosting node is absent.
	reasonNodeNotFound = "NodeNotFound"
	// reasonNodeUIDMismatch reports a device whose hosting node was
	// recreated under the same name.
	reasonNodeUIDMismatch = "NodeUIDMismatch"
	// reasonNodeDeselected reports a device whose hosting node is no
	// longer selected by the singleton node selector.
	reasonNodeDeselected = "NodeDeselected"
	// reasonSourceForbidden reports a forbidden source read.
	reasonSourceForbidden = "SourceForbidden"
	// reasonPoolIncomplete reports an incomplete DRA pool snapshot.
	reasonPoolIncomplete = "PoolIncomplete"
	// reasonSourceUnavailable reports an observation that could not be
	// completed (design doc §6.5 device-level umbrella reason).
	reasonSourceUnavailable = "SourceUnavailable"
)

// defaultMaxStale is the heartbeat bound for unchanged facts when the
// singleton omits spec.maxStale or cannot be read (design doc §5.1).
const defaultMaxStale = 10 * time.Minute

// SourceFactory builds the ordered source registry for one singleton
// configuration. It is the same contract (and the same production
// constructor, inventory.NewSourceFactory) the inventory controller
// uses, so both controllers resolve devices through identically
// configured sources; the device controller never constructs its own
// source semantics.
type SourceFactory = inventory.SourceFactory

// Compile-time proof that the device satisfies the framework platform
// object contract (fwapi.PlatformObject) through its embedded status.
var _ fwapi.PlatformObject = &infrastructurev1alpha1.HardwareDevice{}

// Controller reconciles one HardwareDevice through the framework
// builder. One reconciliation observes at most one device: revalidate
// the spec against current config and cluster state, observe through
// the source, and record fresh facts — or retain everything and report
// the failure as the Observed condition. The collector clock lives in
// the sources (injected at construction); every time comparison in this
// controller is derived from observation stamps, never from a wall
// clock read here.
type Controller struct {
	// Sources builds the ordered source registry for one singleton
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

// The framework reconciler emits reconciliation and provisioning
// events on the reconciled device.
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// NewReconciler builds and registers the device controller through the
// framework builder (ReconcilerFor → ReconcilerBuilder → Build) and
// returns its framework reconciler. Ready aggregates Observed and the
// framework's default ProvisioningSucceeded (both healthy when True),
// so a scheduled-success reconcile whose observation failed still
// yields Ready=False and phase Not Ready; the post-status hook
// finalizes the derived Ready condition without overriding an
// unhealthy value. No finalizer, render, deploy, GC or
// dynamic-ownership actions are registered: the reconciler never adds a
// finalizer, and a deleting device is an early framework no-op.
func (c *Controller) NewReconciler(ctx context.Context, mgr ctrl.Manager) (*fwreconciler.Reconciler, error) {
	if c.Sources == nil {
		return nil, fmt.Errorf("device: source factory must not be nil")
	}

	// The watch mappers list devices through the framework-coherent
	// client — the same unstructured-converting representation the
	// reconciler itself reads through, so mapper lists and reconcile
	// reads hit one cache representation per GVK.
	mapperClient := mgr.GetClient()
	if _, ok := mapperClient.(*fwclient.Client); !ok {
		mapperClient = fwclient.New(mapperClient)
	}

	// The builder converts the primary and secondary watches to the
	// unstructured cache representation itself; typed objects are
	// supplied here only for GVK resolution.
	b := fwreconciler.ReconcilerFor(
		mgr,
		&infrastructurev1alpha1.HardwareDevice{},
		ctrlbuilder.WithPredicates(predicate.GenerationChangedPredicate{}),
	).
		WithInstanceName(ControllerName).
		WithConditions(fwconditions.Dependent(
			fwapi.ConditionType(ConditionTypeObserved),
			fwconditions.HealthyWhenTrue,
		)).
		WithPostStatusFn(postStatus).
		WithAction(observeAction(observeDeps{
			sources: c.Sources,
		})).
		// A singleton configuration change re-evaluates every device:
		// class, selector, source enablement and maxStale transitions
		// must reach all of them. The mapper, not a fixed-name handler,
		// routes these events because the device set is dynamic.
		Watches(&infrastructurev1alpha1.HardwareDiscovery{},
			fwreconciler.WithPredicates(predicate.GenerationChangedPredicate{}),
			fwreconciler.WithEventMapper(allDevices(mapperClient)),
		).
		// Node evidence and lifecycle changes re-observe the devices
		// hosted there; node selection and plane resolution are
		// re-evaluated in reconcile logic, never at the cache.
		Watches(&corev1.Node{},
			fwreconciler.WithPredicates(nodeEvidencePredicate(mgr.GetScheme())),
			fwreconciler.WithEventMapper(devicesOnNode(mapperClient)),
		)

	r, err := b.Build(ctx)
	if err != nil {
		return nil, fmt.Errorf("device: building framework reconciler: %w", err)
	}

	return r, nil
}

// SetupWithManager registers the device controller through the framework
// builder: the primary HardwareDevice watch on spec generation changes
// (this controller's own /status writes never bump the generation, so
// they cannot loop), the singleton configuration watch mapped to every
// device, and the Node evidence watch mapped to the devices hosted on
// the changed node. Source inputs are read scan-driven, never watched.
func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
	// The pinned framework Build accepts a context for future use only;
	// registration itself performs no context-bound work.
	_, err := c.NewReconciler(context.Background(), mgr)
	return err
}

// allDevices maps a singleton configuration change to every device:
// class, selector, source enablement and maxStale transitions must be
// re-evaluated everywhere.
func allDevices(mapperClient client.Client) handler.MapFunc {
	return func(ctx context.Context, _ client.Object) []reconcile.Request {
		devices := &infrastructurev1alpha1.HardwareDeviceList{}
		if err := mapperClient.List(ctx, devices); err != nil {
			log.FromContext(ctx).Error(err, "device: listing hardware devices for configuration change")
			return nil
		}
		return deviceRequests(devices.Items)
	}
}

// devicesOnNode maps a Node evidence change to the devices hosted on
// that node. The service-written node label is the index; the full node
// name always remains in spec.host.name. The watched object is the
// unstructured cache representation, so only its name is read here.
func devicesOnNode(mapperClient client.Client) handler.MapFunc {
	return func(ctx context.Context, node client.Object) []reconcile.Request {
		devices := &infrastructurev1alpha1.HardwareDeviceList{}
		if err := mapperClient.List(ctx, devices,
			client.MatchingLabels{discovery.LabelNode: discovery.NodeLabelValue(node.GetName())}); err != nil {
			log.FromContext(ctx).Error(err, "device: listing hardware devices for node change",
				"node", node.GetName())
			return nil
		}
		return deviceRequests(devices.Items)
	}
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
