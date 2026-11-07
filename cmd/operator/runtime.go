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

package operator

import (
	"crypto/tls"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	inventorycontroller "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/inventory"

	fwmanager "github.com/opendatahub-io/odh-platform-utilities/framework/manager"
)

// leaderElectionID is the unique Lease identity of this manager within
// its namespace.
const leaderElectionID = "hardware-discovery.platform.opendatahub.io"

// newScheme builds the manager scheme: the client-go types (Nodes and
// the DRA source's ResourceSlices decode through it on direct API
// reads) and the infrastructure APIs. Scheme knowledge creates no
// informer: only the watched kinds (HardwareDiscovery, HardwareDevice,
// Node) get caches, so an absent or forbidden optional API never blocks
// manager startup.
func newScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(infrastructurev1alpha1.AddToScheme(scheme))
	return scheme
}

// managerOptions derives the controller-runtime manager options from
// the CLI options and the resolved namespace. The cache stays
// unrestricted — no default namespaces and no per-object label
// selectors — because the singleton's node selector is reconcile-time
// logic that can change without a restart; hardcoding it into the
// cache would freeze discovery on the first selector.
func managerOptions(options Options, namespace string) ctrl.Options {
	// HTTP/2 is disabled unless explicitly enabled: the HTTP/2 stream
	// cancellation and rapid reset exposures (GHSA-qppj-fm5r-hxr3,
	// GHSA-4374-p667-p6c8) do not apply to HTTP/1.1 serving.
	var tlsOpts []func(*tls.Config)
	if !options.EnableHTTP2 {
		tlsOpts = append(tlsOpts, func(config *tls.Config) {
			config.NextProtos = []string{"http/1.1"}
		})
	}

	metricsOptions := metricsserver.Options{
		BindAddress:   options.MetricsBindAddress,
		SecureServing: options.SecureMetrics,
		TLSOpts:       tlsOpts,
	}
	if options.SecureMetrics {
		// Protect the metrics endpoint with authn/authz; the RBAC live
		// in config/rbac (metrics_auth_role).
		metricsOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	return ctrl.Options{
		Scheme:  newScheme(),
		Metrics: metricsOptions,
		// The webhook server is constructed disabled (port -1): this
		// service registers no webhooks and starts nothing
		// certificate-dependent.
		WebhookServer:           webhook.NewServer(webhook.Options{Port: options.WebhookPort, TLSOpts: tlsOpts}),
		HealthProbeBindAddress:  options.HealthProbeBindAddress,
		LeaderElection:          options.LeaderElect,
		LeaderElectionID:        leaderElectionID,
		LeaderElectionNamespace: namespace,
		// Inventory scans are serialized per kind (one singleton, one
		// writer) while the device controller keeps the global
		// concurrency default.
		Controller: inventorycontroller.ManagerControllerOptions(ctrlconfig.Controller{}),
	}
}

// run starts the standalone manager: one controller-runtime manager
// wrapped by the framework manager, both discovery controllers
// registered through their framework-builder setup paths, health and
// readiness probes, and signal-driven shutdown (the manager runs on
// ctrl.SetupSignalHandler's context; the controllers' builder
// registration performs no context-bound work).
func run(options Options) error {
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&options.Logger)))

	namespace, err := options.resolveNamespace()
	if err != nil {
		return err
	}

	config, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("operator: reading kubeconfig: %w", err)
	}

	ctrlManager, err := ctrl.NewManager(config, managerOptions(options, namespace))
	if err != nil {
		return fmt.Errorf("operator: creating manager: %w", err)
	}

	// The framework manager hands both registrations the cache-coherent
	// client (unstructured Get/List matching the builders' unstructured
	// watches); everything else delegates to the controller-runtime
	// manager unchanged.
	manager := fwmanager.New(ctrlManager)

	if err := RegisterControllers(manager); err != nil {
		return err
	}

	if err := manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("operator: registering health check: %w", err)
	}
	if err := manager.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("operator: registering ready check: %w", err)
	}

	return manager.Start(ctrl.SetupSignalHandler())
}
