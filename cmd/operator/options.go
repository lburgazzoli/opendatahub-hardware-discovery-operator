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
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/pflag"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	// applicationsNamespaceEnv is the namespace the ODH orchestrator
	// injects into its module controllers. This service's CRDs are
	// cluster-scoped, so the value only places the leader-election
	// Lease; it never narrows a cache or a watch.
	applicationsNamespaceEnv = "APPLICATIONS_NAMESPACE"

	// serviceAccountNamespacePath is the in-cluster namespace file: the
	// namespace of the pod the service runs in.
	serviceAccountNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// Options carries the operator subcommand's configuration. Every field
// is bound to exactly one canonical generated-chart flag.
type Options struct {
	// MetricsBindAddress is the metrics endpoint bind address; "0"
	// disables the endpoint (optional diagnostics stay off unless the
	// deployment opts in).
	MetricsBindAddress string
	// SecureMetrics serves the metrics endpoint via HTTPS with
	// authn/authz filtering when true.
	SecureMetrics bool
	// HealthProbeBindAddress is the health/readiness probe endpoint
	// bind address.
	HealthProbeBindAddress string
	// WebhookPort is the webhook server port; -1 disables the server —
	// this service registers no webhooks.
	WebhookPort int
	// LeaderElect enables leader election: exactly one active manager
	// per namespace holds the Lease.
	LeaderElect bool
	// Namespace places the leader-election Lease. Empty resolves
	// through APPLICATIONS_NAMESPACE, then the in-cluster pod
	// namespace; resolution must succeed — no silent "default".
	Namespace string
	// EnableHTTP2 re-enables HTTP/2 for the metrics server; it is
	// disabled by default (HTTP/2 rapid-reset exposure).
	EnableHTTP2 bool
	// Logger carries the zap logger configuration (production mode by
	// default; --zap-* flags bind to it).
	Logger zap.Options
}

// newOptions returns the operator defaults: leader election on, probes
// on :8081, no webhook server, no metrics endpoint and no debug
// logging unless the deployment opts in.
func newOptions() Options {
	return Options{
		MetricsBindAddress:     "0",
		SecureMetrics:          true,
		HealthProbeBindAddress: ":8081",
		WebhookPort:            -1,
		LeaderElect:            true,
	}
}

// bind registers the canonical flags on the flag set.
func (o *Options) bind(flags *pflag.FlagSet) {
	flags.StringVar(&o.MetricsBindAddress, "metrics-bind-address", o.MetricsBindAddress,
		"The address the metrics endpoint binds to. Use :8443 for HTTPS or :8080 for HTTP, "+
			"or leave as 0 to disable the metrics service.")
	flags.BoolVar(&o.SecureMetrics, "metrics-secure", o.SecureMetrics,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flags.StringVar(&o.HealthProbeBindAddress, "health-probe-bind-address", o.HealthProbeBindAddress,
		"The address the probe endpoint binds to.")
	flags.IntVar(&o.WebhookPort, "webhook-port", o.WebhookPort,
		"Port the webhook server listens on. Set -1 to disable the webhook server.")
	flags.BoolVar(&o.LeaderElect, "leader-elect", o.LeaderElect,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flags.StringVar(&o.Namespace, "namespace", o.Namespace,
		"Namespace of the leader-election lease. Defaults to APPLICATIONS_NAMESPACE, "+
			"then the in-cluster pod namespace.")
	flags.BoolVar(&o.EnableHTTP2, "enable-http2", o.EnableHTTP2,
		"If set, HTTP/2 will be enabled for the metrics server")
	// The zap flags bind to the standard library flag set; AddGoFlagSet
	// merges them into the command's pflag set.
	zapFlags := flag.NewFlagSet("logging", flag.ContinueOnError)
	o.Logger.BindFlags(zapFlags)
	flags.AddGoFlagSet(zapFlags)
}

// resolveNamespace resolves the leader-election namespace by explicit
// precedence: the --namespace flag, the ODH APPLICATIONS_NAMESPACE
// injection, then the in-cluster pod namespace. It fails loudly when
// nothing resolves: a Lease must never silently land in "default".
func (o Options) resolveNamespace() (string, error) {
	if o.Namespace != "" {
		return o.Namespace, nil
	}
	if injected := os.Getenv(applicationsNamespaceEnv); injected != "" {
		return injected, nil
	}
	if data, err := os.ReadFile(serviceAccountNamespacePath); err == nil {
		if namespace := strings.TrimSpace(string(data)); namespace != "" {
			return namespace, nil
		}
	}
	return "", fmt.Errorf(
		"operator: no namespace for the leader-election lease: pass --namespace, set %s or run in a pod",
		applicationsNamespaceEnv)
}
