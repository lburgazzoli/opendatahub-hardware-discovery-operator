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

// Package hardwaredevice contains the per-device observation controller.
//
// The HardwareDevice controller is the only writer of HardwareDevice status
// (via the /status subresource): it resolves the identity and source locator
// recorded in spec, observes the device, and writes the supported facts with
// collector-clock provenance. It never rewrites its own spec and never creates
// replacement identity objects. Real reconciliation logic lands with task 007;
// this file preserves the Kubebuilder scaffold for wiring.
package hardwaredevice

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
)

// HardwareDeviceReconciler reconciles a HardwareDevice object.
type HardwareDeviceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=infrastructure.platform.opendatahub.io,resources=hardwaredevices,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.platform.opendatahub.io,resources=hardwaredevices/status,verbs=get;update;patch

// Reconcile is part of the main Kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/reconcile
func (r *HardwareDeviceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	// TODO(user): your logic here

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *HardwareDeviceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1alpha1.HardwareDevice{}).
		Named("hardwaredevice").
		Complete(r)
}
