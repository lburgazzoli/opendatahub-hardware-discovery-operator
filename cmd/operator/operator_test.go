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
	"testing"

	. "github.com/onsi/gomega"

	resourcev1 "k8s.io/api/resource/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	inventorycontroller "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/inventory"
)

// TestOptionsDefaults proves the operator defaults: leader election on,
// probes on :8081, the webhook server disabled, optional metrics
// diagnostics off and production logging.
func TestOptionsDefaults(t *testing.T) {
	g := NewWithT(t)

	options := newOptions()
	g.Expect(options.LeaderElect).To(BeTrue(), "leader election is enabled by default")
	g.Expect(options.HealthProbeBindAddress).To(Equal(":8081"))
	g.Expect(options.WebhookPort).To(Equal(-1), "the webhook server is disabled by default")
	g.Expect(options.MetricsBindAddress).To(Equal("0"), "the metrics endpoint is opt-in")
	g.Expect(options.SecureMetrics).To(BeTrue())
	g.Expect(options.Logger.Development).To(BeFalse(), "production logging by default")
}

// TestChartFlagAcceptance proves the command accepts the canonical
// generated-chart flags — including the disabled webhook port — and
// that they land on the options value the flags bind to.
func TestChartFlagAcceptance(t *testing.T) {
	g := NewWithT(t)

	options := newOptions()
	command := newCommand(&options)
	g.Expect(command.Flags().Parse([]string{
		"--leader-elect",
		"--metrics-bind-address=:8443",
		"--metrics-secure=true",
		"--health-probe-bind-address=:8081",
		"--webhook-port=-1",
		"--zap-log-level=debug",
	})).To(Succeed(), "the chart-rendered argument list must parse")

	g.Expect(options.LeaderElect).To(BeTrue())
	g.Expect(options.MetricsBindAddress).To(Equal(":8443"))
	g.Expect(options.SecureMetrics).To(BeTrue())
	g.Expect(options.HealthProbeBindAddress).To(Equal(":8081"))
	g.Expect(options.WebhookPort).To(Equal(-1))

	// The merged zap flags parse too.
	g.Expect(options.Logger.Development).To(BeFalse())
}

// TestManagerOptions proves the CLI options map onto the manager
// options: explicit leader-election identity and namespace, the
// disabled webhook server, secure opt-in metrics, the preserved
// per-kind inventory serialization — even under a larger global
// concurrency default — and an unrestricted cache whose selectors are
// reconcile-time logic, never frozen discovery inputs.
func TestManagerOptions(t *testing.T) {
	g := NewWithT(t)

	options := newOptions()
	options.MetricsBindAddress = ":8443"
	managed := managerOptions(options, "apps-namespace")

	g.Expect(managed.LeaderElection).To(BeTrue())
	g.Expect(managed.LeaderElectionID).To(Equal(leaderElectionID))
	g.Expect(managed.LeaderElectionID).NotTo(BeEmpty())
	g.Expect(managed.LeaderElectionNamespace).To(Equal("apps-namespace"))
	g.Expect(managed.HealthProbeBindAddress).To(Equal(":8081"))

	// The disabled-webhook path: a negative port means the server never
	// starts and nothing certificate-dependent boots.
	server, ok := managed.WebhookServer.(*webhook.DefaultServer)
	g.Expect(ok).To(BeTrue(), "the manager uses the standard webhook server")
	g.Expect(server.Options.Port).To(Equal(-1))

	g.Expect(managed.Metrics.BindAddress).To(Equal(":8443"))
	g.Expect(managed.Metrics.SecureServing).To(BeTrue())
	g.Expect(managed.Metrics.FilterProvider).NotTo(BeNil(),
		"secure metrics serve behind authn/authz filtering")

	// Inventory scans stay serialized per kind even when the runtime
	// runs a larger global concurrency default.
	g.Expect(managed.Controller.GroupKindConcurrency).To(HaveKeyWithValue(singletonGroupKindKey, 1))
	withGlobal := managed.Controller
	withGlobal.MaxConcurrentReconciles = 10
	rederived := inventorycontroller.ManagerControllerOptions(withGlobal)
	g.Expect(rederived.GroupKindConcurrency).To(HaveKeyWithValue(singletonGroupKindKey, 1))
	g.Expect(rederived.MaxConcurrentReconciles).To(Equal(10))

	// The cache is unrestricted: node selection is re-evaluated on
	// every scan, so a selector change must not strand informers.
	g.Expect(managed.Cache.DefaultNamespaces).To(BeEmpty())
	g.Expect(managed.Cache.ByObject).To(BeEmpty())

	// Scheme knowledge without informer dependencies: the DRA source
	// decodes ResourceSlices on direct API reads, but nothing watches
	// them.
	g.Expect(newScheme().Recognizes(resourcev1.SchemeGroupVersion.WithKind("ResourceSlice"))).To(BeTrue())
}

// TestResolveNamespace proves the leader-election namespace precedence:
// the explicit flag, then the ODH APPLICATIONS_NAMESPACE injection,
// then the in-cluster pod namespace — and a loud failure when nothing
// resolves, never a silent "default".
func TestResolveNamespace(t *testing.T) {
	g := NewWithT(t)

	t.Setenv(applicationsNamespaceEnv, "")
	_, err := newOptions().resolveNamespace()
	g.Expect(err).To(HaveOccurred(), "no flag, no injection and no pod namespace must fail loudly")

	t.Setenv(applicationsNamespaceEnv, "odh-applications")
	namespace, err := newOptions().resolveNamespace()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(namespace).To(Equal("odh-applications"))

	explicit := newOptions()
	explicit.Namespace = "explicit-namespace"
	namespace, err = explicit.resolveNamespace()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(namespace).To(Equal("explicit-namespace"),
		"the explicit flag wins over the injected namespace")
}
