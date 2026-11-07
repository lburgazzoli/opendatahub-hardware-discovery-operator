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
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	kindcluster "sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"

	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
)

// Spec is the Kind cluster specification the harness requests. It is a
// plain struct (instead of raw kind options) so unit tests can assert the
// exact name, node image and run-private kubeconfig handed to the provider
// without depending on kind option internals.
type Spec struct {
	Name           string
	NodeImage      string
	KubeconfigPath string
	WaitForReady   time.Duration

	// FeatureGates enables Kubernetes feature gates on the control-plane
	// components (kube-apiserver, kube-controller-manager,
	// kube-scheduler). It is empty by default; only frozen profiles whose
	// contract facts require an alpha field opt in. Example: the
	// gpu.nvidia.com DeviceClass extendedResourceName fact on
	// resource.k8s.io/v1 needs DRAExtendedResource, because the API
	// server silently drops that alpha field when the gate is off.
	FeatureGates map[string]bool
}

// Provider is the minimal Kind provider surface the harness depends on.
//
// The methods deliberately take no context, mirroring the pinned kind
// library's own provider API (sigs.k8s.io/kind v0.33.0). Every call is
// therefore executed through runBounded, which enforces a deadline and
// propagates timeouts honestly instead of claiming cancellation.
type Provider interface {
	// Create provisions the cluster described by spec, writing the
	// kubeconfig to spec.KubeconfigPath (never to the ambient kubeconfig).
	Create(spec Spec) error

	// Delete destroys the named cluster. kubeconfigPath is the run-private
	// path used at create time.
	Delete(name, kubeconfigPath string) error

	// CollectLogs writes node/container logs and debug files into dir.
	CollectLogs(name, dir string) error

	// LoadImage imports a locally available image into every node container
	// of the named cluster. The image is saved from the host container
	// runtime (no registry push) and imported into containerd on each node.
	LoadImage(name, image string) error
}

// kindProvider adapts the real sigs.k8s.io/kind provider to Provider.
type kindProvider struct {
	provider *kindcluster.Provider
	runtime  Runtime
}

// newKindProvider builds the real provider for the selected runtime.
// Podman is the default; Docker is the accepted override.
func newKindProvider(runtime Runtime) (Provider, error) {
	var providerOpt kindcluster.ProviderOption
	switch runtime {
	case RuntimePodman:
		providerOpt = kindcluster.ProviderWithPodman()
	case RuntimeDocker:
		providerOpt = kindcluster.ProviderWithDocker()
	default:
		return nil, errUnsupportedRuntime(runtime)
	}

	return &kindProvider{
		provider: kindcluster.NewProvider(providerOpt),
		runtime:  runtime,
	}, nil
}

var _ Provider = (*kindProvider)(nil)

func (p *kindProvider) Create(spec Spec) error {
	// The v1alpha4 cluster config is always used: it carries the node
	// image (single source of truth), optional control-plane feature
	// gates, and the containerd patch below.
	//
	// privileged_without_host_devices stops containerd from injecting
	// every node device into privileged containers. Kind nodes inherit
	// the host /dev, including volatile entries such as USB bus devices;
	// when one re-enumerates away mid-run, creating any privileged pod
	// (the DRA simulator's kubelet plugin) fails at OCI create on the
	// stale bind mount. The simulator and the service never need host
	// devices, so the hermetic setting is also the honest one.
	cfg := &v1alpha4.Cluster{
		Name:  spec.Name,
		Nodes: []v1alpha4.Node{{Role: v1alpha4.ControlPlaneRole, Image: spec.NodeImage}},
		ContainerdConfigPatches: []string{
			`[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
	privileged_without_host_devices = true`,
		},
	}
	if len(spec.FeatureGates) > 0 {
		// Feature gates reach the control-plane components through the
		// cluster config (kind's top-level featureGates map is applied
		// to kube-apiserver, kube-controller-manager and
		// kube-scheduler).
		cfg.FeatureGates = spec.FeatureGates
	}

	createOptions := []kindcluster.CreateOption{
		kindcluster.CreateWithKubeconfigPath(spec.KubeconfigPath),
		kindcluster.CreateWithWaitForReady(spec.WaitForReady),
		kindcluster.CreateWithV1Alpha4Config(cfg),
	}

	return p.provider.Create(spec.Name, createOptions...)
}

func (p *kindProvider) Delete(name, kubeconfigPath string) error {
	return p.provider.Delete(name, kubeconfigPath)
}

func (p *kindProvider) CollectLogs(name, dir string) error {
	return p.provider.CollectLogs(name, dir)
}

// LoadImage saves image from the host container runtime into a temporary
// archive and imports it into every node container of the named cluster
// (the same mechanism the kind CLI uses; the pinned kind library does not
// expose image loading on its Provider). No registry push is involved.
func (p *kindProvider) LoadImage(name, image string) error {
	if image == "" {
		return fmt.Errorf("image reference must not be empty")
	}

	nodeList, err := p.provider.ListInternalNodes(name)
	if err != nil {
		return fmt.Errorf("listing nodes of cluster %s: %w", name, err)
	}
	if len(nodeList) == 0 {
		return fmt.Errorf("no nodes found for cluster %s", name)
	}

	tmp, err := os.CreateTemp("", "hw-disc-image-*.tar")
	if err != nil {
		return fmt.Errorf("creating image archive: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing image archive: %w", err)
	}

	if err := saveImage(string(p.runtime), image, tmpName); err != nil {
		return err
	}

	for _, node := range nodeList {
		if err := importImage(node, image, tmpName); err != nil {
			return err
		}
	}
	return nil
}

// saveImage saves image into dest using the host container runtime.
func saveImage(runtime, image, dest string) error {
	cmd := exec.Command(runtime, "save", "-o", dest, image)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("saving image %s with %s: %w (output: %s)",
			image, runtime, err, strings.TrimSpace(out.String()))
	}
	return nil
}

// importImage imports the archive at path into one node's containerd.
func importImage(node nodes.Node, image, path string) error {
	archive, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening image archive %s: %w", path, err)
	}
	defer func() { _ = archive.Close() }()

	if err := nodeutils.LoadImageArchive(node, archive); err != nil {
		return fmt.Errorf("loading image %s into node %s: %w", image, node.String(), err)
	}
	return nil
}
