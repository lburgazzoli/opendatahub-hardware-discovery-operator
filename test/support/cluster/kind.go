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

package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
)

// kindInstance is the only Instance implementation: the two-plane
// demonstration runs on run-owned Kind clusters exclusively (no external
// or k3s backend).
type kindInstance struct {
	opts           options
	provider       Provider
	name           string
	kubeconfigPath string
	baseDir        string
	cfg            *rest.Config
	cli            client.Client
}

var _ Instance = (*kindInstance)(nil)

// newKindCluster creates the run-owned cluster. injected may be nil (real
// provider); unit tests pass a fake and never touch a container runtime.
func newKindCluster(ctx context.Context, injected Provider, opts ...Option) (Instance, error) {
	o := defaults()
	for _, opt := range opts {
		if opt != nil {
			opt.apply(&o)
		}
	}
	if err := o.validate(); err != nil {
		return nil, fmt.Errorf("invalid cluster options: %w", err)
	}

	provider := injected
	if provider == nil {
		p, err := newKindProvider(o.runtime)
		if err != nil {
			return nil, fmt.Errorf("creating kind provider: %w", err)
		}
		provider = p
	}

	runID, err := newRunID()
	if err != nil {
		return nil, fmt.Errorf("generating run id: %w", err)
	}

	baseDir, err := os.MkdirTemp("", "hw-disc-kind-")
	if err != nil {
		return nil, fmt.Errorf("creating run-private kubeconfig directory: %w", err)
	}

	c := &kindInstance{
		opts:           o,
		provider:       provider,
		name:           namePrefix + runID,
		kubeconfigPath: filepath.Join(baseDir, "kubeconfig"),
		baseDir:        baseDir,
	}

	spec := Spec{
		Name:           c.name,
		NodeImage:      o.nodeImage,
		KubeconfigPath: c.kubeconfigPath,
		WaitForReady:   o.waitForReady,
		FeatureGates:   o.featureGates,
	}

	o.logFn("creating run-owned kind cluster %s (node image %s, runtime %s, kubeconfig %s)",
		c.name, o.nodeImage, o.runtime, c.kubeconfigPath)

	if err := runBounded(ctx, o.createTimeout, fmt.Sprintf("creating kind cluster %s", c.name),
		func() error { return provider.Create(spec) },
	); err != nil {
		// Diagnostics first, then teardown of whatever was created, then
		// removal of the private kubeconfig. All errors are surfaced.
		createErr := fmt.Errorf("kind create failed: %w", err)
		return nil, errors.Join(createErr, c.teardown())
	}

	cfg, err := loadKubeconfig(c.kubeconfigPath)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("loading run-private kubeconfig: %w", err), c.teardown())
	}

	factory := o.clientFactory
	if factory == nil {
		factory = newControllerRuntimeClient
	}
	cli, err := factory(cfg)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("creating cluster client: %w", err), c.teardown())
	}

	c.cfg = cfg
	c.cli = cli

	return c, nil
}

func (c *kindInstance) Config() *rest.Config {
	if c == nil || c.cfg == nil {
		return nil
	}
	return rest.CopyConfig(c.cfg)
}

func (c *kindInstance) Client() client.Client {
	if c == nil {
		return nil
	}
	return c.cli
}

func (c *kindInstance) Name() string {
	if c == nil {
		return ""
	}
	return c.name
}

func (c *kindInstance) KubeconfigPath() string {
	if c == nil {
		return ""
	}
	return c.kubeconfigPath
}

// Setup waits until the API server answers and every node is Ready. It
// installs nothing (no cert-manager, no addons, no operator).
func (c *kindInstance) Setup(ctx context.Context) error {
	if c == nil || c.cli == nil {
		return fmt.Errorf("cluster is not initialized")
	}

	if err := waitForNodesReady(ctx, c.cli, c.opts.setupTimeout); err != nil {
		return fmt.Errorf("waiting for cluster readiness: %w", err)
	}

	c.opts.logFn("kind cluster %s is ready", c.name)
	return nil
}

// Stop destroys exactly the run-owned cluster and removes the private
// kubeconfig. The caller context is intentionally not used to bound the
// teardown: cleanup always runs with its own independent bound so that a
// cancelled or timed-out test context cannot leak the cluster. Cleanup
// errors are propagated, never swallowed.
func (c *kindInstance) Stop(_ context.Context) error {
	if c == nil {
		return nil
	}
	return c.teardown()
}

// LoadImage imports a locally built image into every node of the run-owned
// cluster through the bounded provider wrapper (the pinned kind library's
// image path is context-less, like Create/Delete).
func (c *kindInstance) LoadImage(ctx context.Context, image string) error {
	if c == nil {
		return fmt.Errorf("cluster is not initialized")
	}
	if image == "" {
		return fmt.Errorf("image reference must not be empty")
	}

	return runBounded(ctx, c.opts.loadTimeout,
		fmt.Sprintf("loading image %s into cluster %s", image, c.name),
		func() error { return c.provider.LoadImage(c.name, image) })
}

// CollectDiagnostics writes kind node logs and cluster events into dir.
// Bounded independently of the (possibly dead) caller context.
func (c *kindInstance) CollectDiagnostics(_ context.Context, dir string) error {
	if c == nil || dir == "" {
		return nil
	}

	diagCtx, cancel := context.WithTimeout(context.Background(), c.opts.deleteTimeout)
	defer cancel()

	var errs []error
	if err := runBounded(diagCtx, c.opts.deleteTimeout,
		fmt.Sprintf("collecting kind logs for %s", c.name),
		func() error { return c.provider.CollectLogs(c.name, dir) },
	); err != nil {
		errs = append(errs, err)
	}

	if c.cli != nil {
		if err := dumpEvents(diagCtx, c.cli, filepath.Join(dir, "events.yaml")); err != nil {
			errs = append(errs, err)
		}
	}

	c.opts.logFn("diagnostics for %s written under %s", c.name, dir)
	return errors.Join(errs...)
}

// teardown deletes the run-owned cluster with an independent bounded
// context and removes the run-private kubeconfig directory.
func (c *kindInstance) teardown() error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), c.opts.deleteTimeout)
	defer cancel()

	var errs []error
	if err := runBounded(cleanupCtx, c.opts.deleteTimeout,
		fmt.Sprintf("deleting kind cluster %s", c.name),
		func() error { return c.provider.Delete(c.name, c.kubeconfigPath) },
	); err != nil {
		errs = append(errs, fmt.Errorf("kind delete failed: %w", err))
	}

	if err := os.RemoveAll(c.baseDir); err != nil {
		errs = append(errs, fmt.Errorf("removing run-private kubeconfig directory %s: %w", c.baseDir, err))
	}

	return errors.Join(errs...)
}

// loadKubeconfig builds a REST config from the run-private kubeconfig file
// written by kind. The ambient kubeconfig and current context are never
// consulted: clientcmd.RESTConfigFromKubeConfig parses the bytes as a
// standalone document with no fallback to $KUBECONFIG or $HOME/.kube.
// A per-request timeout bounds every API call made through this config
// (the harness waits by polling, never by hanging calls).
func loadKubeconfig(path string) (*rest.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading kubeconfig %s: %w", path, err)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(data)
	if err != nil {
		return nil, fmt.Errorf("parsing kubeconfig %s: %w", path, err)
	}
	cfg.Timeout = requestTimeout
	return cfg, nil
}

// requestTimeout bounds each API request issued through the harness config.
const requestTimeout = 32 * time.Second

// newControllerRuntimeClient builds the standard direct client for the
// harness scheme (core Kubernetes APIs including resource.k8s.io/v1, plus
// the project's own API group for the e2e scenarios).
func newControllerRuntimeClient(cfg *rest.Config) (client.Client, error) {
	scheme, err := newScheme()
	if err != nil {
		return nil, err
	}
	cli, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("creating controller-runtime client: %w", err)
	}
	return cli, nil
}

func newScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering client-go scheme: %w", err)
	}
	if err := infrav1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering infrastructure API scheme: %w", err)
	}
	return scheme, nil
}

// newRunID returns a random run identifier; cluster names are unique per
// construction and never reused between runs.
func newRunID() (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// waitForNodesReady polls until at least one node exists and all nodes
// report Ready. Bounded by both ctx and timeout.
func waitForNodesReady(ctx context.Context, cli client.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var lastErr error
	for {
		nodes := &corev1.NodeList{}
		if err := cli.List(ctx, nodes); err != nil {
			lastErr = fmt.Errorf("listing nodes: %w", err)
		} else if len(nodes.Items) == 0 {
			lastErr = fmt.Errorf("no nodes found")
		} else {
			allReady := true
			for i := range nodes.Items {
				if !nodeReady(&nodes.Items[i]) {
					allReady = false
					lastErr = fmt.Errorf("node %s is not ready", nodes.Items[i].Name)
				}
			}
			if allReady {
				return nil
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for nodes ready: %w", lastErr)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for nodes ready: %w (last: %v)", ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func nodeReady(node *corev1.Node) bool {
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == corev1.NodeReady {
			return node.Status.Conditions[i].Status == corev1.ConditionTrue
		}
	}
	return false
}
