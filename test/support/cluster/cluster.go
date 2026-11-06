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

// Package cluster provides the run-isolated Kind cluster lifecycle used by
// the e2e suites (testing.md "Kind harness: adapt the DB architecture
// safely"). It reproduces the DB operator's small Instance abstraction
// (Config/Client/Setup/Stop) with functional options, backed by the pinned
// Go Kind provider (sigs.k8s.io/kind v0.33.0).
//
// Safety properties enforced here, unlike the reference implementation:
//
//   - every cluster gets a unique run name (hw-disc-<run-id>); there is no
//     name option, no shared named cluster and no prefix-wide purge;
//   - the kubeconfig is written to a run-private path and that exact path is
//     handed to every create/delete/subprocess call; the ambient kubeconfig
//     and current context are never read or written;
//   - the pinned node image (kindest/node:v1.34.11) and an explicit runtime
//     (podman by default, docker overridable) are always used;
//   - create/delete run through a bounded wrapper with honest timeout
//     propagation: kind's Go provider API takes no context, so a timed-out
//     call is reported as such and cleanup must still be attempted;
//   - no cert-manager, no operator installation and no other addon is ever
//     installed by this package.
package cluster

import (
	"context"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Instance exposes the minimum cluster primitives needed by tests without
// coupling callers to a specific backend implementation.
type Instance interface {
	// Config returns a copy of the run-private REST configuration.
	Config() *rest.Config

	// Client returns a controller-runtime client bound to the run-private
	// configuration. All API clients in the e2e suites must use it.
	Client() client.Client

	// Setup waits until the cluster API answers and every node is Ready.
	// It installs nothing.
	Setup(ctx context.Context) error

	// Stop destroys exactly the run-owned cluster and removes the private
	// kubeconfig. The caller context is deliberately not used to bound the
	// teardown: cleanup always gets its own independent bound so a failed
	// or cancelled test cannot leak the cluster. Errors are propagated.
	Stop(ctx context.Context) error

	// Name returns the unique run-owned cluster name.
	Name() string

	// KubeconfigPath returns the run-private kubeconfig path. It must be
	// passed (as KUBECONFIG and/or --kubeconfig) to every subprocess that
	// talks to this cluster.
	KubeconfigPath() string

	// LoadImage makes a locally built image available to every node of the
	// run-owned cluster. The image is taken from the host container runtime
	// (podman by default, docker override); no registry push happens. The
	// e2e suite uses this for the local service image before installing its
	// Helm chart.
	LoadImage(ctx context.Context, image string) error

	// CollectDiagnostics writes Kind node logs, cluster events and source
	// object snapshots into dir. It is meant to be called before Stop when
	// a run fails, never as a substitute for a failing assertion.
	CollectDiagnostics(ctx context.Context, dir string) error
}

// New creates a new run-isolated Kind cluster and returns its Instance.
//
// The cluster is created eagerly (matching the DB harness shape): by the
// time New returns, the control plane is up and the private kubeconfig has
// been written. A failed create collects diagnostics first, then deletes
// whatever was created and removes the private kubeconfig.
func New(ctx context.Context, opts ...Option) (Instance, error) {
	return newKindCluster(ctx, nil, opts...)
}

// NewWithProvider is New with an injected Provider. It exists for unit
// tests, which must never create a real cluster; production callers use New.
func NewWithProvider(ctx context.Context, p Provider, opts ...Option) (Instance, error) {
	return newKindCluster(ctx, p, opts...)
}
