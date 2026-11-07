//go:build e2e

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

// Package e2e contains the build-tagged end-to-end suites.
//
// Task 010 ships the harness verification: it proves, against real
// run-owned Kind clusters, that the cluster lifecycle, the Helm runner and
// both frozen Run:ai fake-gpu-operator simulator profiles work — including
// the gate-approved DRA fact-mutation protocol — and that everything the
// runs created is destroyed afterwards.
//
// This is explicitly NOT the two-plane discovery demonstration: no service
// chart is installed and no HardwareDevice output is asserted here. The
// service scenarios are added by task 011, once the reviewed Helm chart
// exists, and must not be confused with this facility check.
package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/gomega"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/test/support/cluster"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/test/support/helm"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/test/support/simulator"
)

const (
	// runtimeEnvKey selects the container runtime backing the run-owned
	// Kind clusters (podman by default, docker as the accepted override).
	// It is set by make/e2e.mk from CONTAINER_TOOL.
	runtimeEnvKey = "E2E_RUNTIME"

	// profileTimeout bounds one complete profile run: cluster creation,
	// simulator install, evidence waits, the fact-mutation protocol and
	// the deliberate uninstall.
	profileTimeout = 25 * time.Minute

	// diagnosticsTimeout bounds failure diagnostics collection, which uses
	// its own context so a dead scenario context cannot hide evidence.
	diagnosticsTimeout = 2 * time.Minute

	// frozen simulator pool facts (design doc §9.1 shared values).
	frozenGpuProduct  = "Tesla-K80"
	frozenGpuMemoryMi = 11441
	frozenGpuCount    = 2

	// mutatedGpuMemoryMi is the fact-mutation scenario's new gpuMemory.
	mutatedGpuMemoryMi = 16384
)

// TestSimulatorHarnessVerification runs both frozen simulator profiles
// sequentially, each in its own run-owned Kind cluster. Both profiles run
// gate-free: the service never reads DeviceClasses, so the alpha
// DRAExtendedResource feature gate stays off and the DeviceClass's
// extendedResourceName field (silently dropped by the pinned 1.34 API
// server without the gate) is not asserted.
func TestSimulatorHarnessVerification(t *testing.T) {
	t.Log("simulator harness verification (task 010): cluster + Helm + FGO profile lifecycle;",
		"this is not the two-plane discovery demonstration (task 011)")

	for _, profile := range []simulator.Profile{simulator.ProfileClassic, simulator.ProfileDRA} {
		t.Run(profile.String(), func(t *testing.T) {
			verifySimulatorProfile(t, profile)
		})
	}
}

// verifySimulatorProfile drives one isolated profile run end to end.
func verifySimulatorProfile(t *testing.T, profile simulator.Profile) {
	t.Helper()
	g := NewWithT(t)

	runtime := selectRuntime(t)

	ctx, cancel := context.WithTimeout(context.Background(), profileTimeout)
	defer cancel()

	clusterOpts := []cluster.Option{
		cluster.WithRuntime(runtime),
		cluster.WithLogFn(t.Logf),
	}

	inst, err := cluster.New(ctx, clusterOpts...)
	g.Expect(err).NotTo(HaveOccurred(), "creating the run-owned kind cluster must succeed")

	// Cleanup is constructed immediately after creation, before any
	// assertion: a failed run collects diagnostics first, then destroys
	// exactly the run-owned cluster with an independent context.
	t.Cleanup(func() {
		if t.Failed() {
			collectDiagnostics(t, inst, profile)
		}
		if stopErr := inst.Stop(context.Background()); stopErr != nil {
			t.Errorf("destroying the run-owned cluster %s: %v", inst.Name(), stopErr)
		}
	})

	g.Expect(inst.Setup(ctx)).To(Succeed(), "the cluster nodes must become ready")

	runner, err := helm.NewRunner(inst.KubeconfigPath())
	g.Expect(err).NotTo(HaveOccurred())

	cli := inst.Client()

	t.Logf("installing simulator profile %s on cluster %s (kubeconfig %s)",
		profile, inst.Name(), inst.KubeconfigPath())
	g.Expect(simulator.Setup(ctx, cli, runner, profile)).To(Succeed(),
		"the frozen approved simulator profile must install cleanly")

	nodes := &corev1.NodeList{}
	g.Expect(cli.List(ctx, nodes)).To(Succeed())
	g.Expect(nodes.Items).NotTo(BeEmpty())

	// Every labeled node must receive its per-node topology ConfigMap with
	// the frozen pool facts: this is the classic plane's discovery input
	// and the DRA plugin's enumeration source.
	for i := range nodes.Items {
		node := &nodes.Items[i]
		topology, topoErr := simulator.WaitForNodeTopology(ctx, cli, node.Name)
		g.Expect(topoErr).NotTo(HaveOccurred(), "topology ConfigMap for node %s", node.Name)
		g.Expect(topology.GpuProduct).To(Equal(frozenGpuProduct))
		g.Expect(topology.GpuMemoryMi).To(Equal(frozenGpuMemoryMi))
		g.Expect(topology.GPUIDs).To(HaveLen(frozenGpuCount))
		for _, id := range topology.GPUIDs {
			g.Expect(id).To(HavePrefix("GPU-"), "synthetic FGO identities are GPU-<uuid5>")
		}
	}

	switch profile {
	case simulator.ProfileClassic:
		verifyClassicProfile(t, ctx, cli)
		verifyLoadImage(t, ctx, inst)
	case simulator.ProfileDRA:
		verifyDRAProfile(t, ctx, cli, nodes)
	}

	// The uninstall facility is part of the harness and is proven here; the
	// cluster itself is destroyed by the registered cleanup afterwards.
	g.Expect(simulator.Teardown(ctx, cli, runner)).To(Succeed(),
		"the simulator release must uninstall cleanly")
	installed, relErr := simulator.ReleaseInstalled(ctx, cli)
	g.Expect(relErr).NotTo(HaveOccurred())
	g.Expect(installed).To(BeFalse(), "no simulator release secrets may remain after uninstall")
}

// verifyClassicProfile checks the classic profile's fake components.
func verifyClassicProfile(t *testing.T, ctx context.Context, cli client.Client) {
	t.Helper()
	g := NewWithT(t)

	// The fake classic device plugin runs on every node labeled by the
	// profile (chart DaemonSet selector nvidia.com/gpu.deploy.device-plugin).
	g.Expect(simulator.WaitForPodsReady(ctx, cli,
		client.MatchingLabels{"component": simulator.DevicePluginPodComponentLabel}, 1),
	).To(Succeed(), "the fake classic device plugin must become ready on the labeled nodes")
}

// verifyLoadImage live-proves the image-loading facility: an image that is
// guaranteed to exist in the host runtime (the pinned Kind node image, used
// here only as cargo) is saved and imported into every node container of
// the exact run-owned cluster. No registry push or pull is involved.
func verifyLoadImage(t *testing.T, ctx context.Context, inst cluster.Instance) {
	t.Helper()
	g := NewWithT(t)

	g.Expect(inst.LoadImage(ctx, cluster.DefaultNodeImage)).To(Succeed(),
		"loading a local image into the exact run-owned cluster must succeed")
}

// verifyDRAProfile checks the DRA profile's evidence and runs the frozen
// fact-mutation protocol.
func verifyDRAProfile(t *testing.T, ctx context.Context, cli client.Client, nodes *corev1.NodeList) {
	t.Helper()
	g := NewWithT(t)

	// Explicit discovery check before any DRA scenario: the pinned
	// baseline must serve the stable API (never a beta group, never a fake
	// CRD).
	available, err := cluster.ResourceAPIAvailable(cli)
	g.Expect(err).NotTo(HaveOccurred(), "checking resource.k8s.io/v1 discovery must not error")
	g.Expect(available).To(BeTrue(), "stable resource.k8s.io/v1 must be served by the pinned baseline")

	// The chart renders the gpu.nvidia.com DeviceClass on resource.k8s.io/v1.
	// The alpha extendedResourceName field is not asserted and no feature
	// gate is enabled: the service never reads DeviceClasses, and without
	// the gate the pinned 1.34 API server silently drops that field (an
	// accepted, verified-live behavior).
	class, err := simulator.GetDeviceClass(ctx, cli)
	g.Expect(err).NotTo(HaveOccurred(), "the chart-rendered gpu.nvidia.com DeviceClass must exist")
	g.Expect(class.Name).To(Equal(simulator.DeviceClassName))

	// The DRA plugin pods must be ready; they enumerate at startup only.
	g.Expect(simulator.WaitForPodsReady(ctx, cli,
		client.MatchingLabels{"app.kubernetes.io/component": simulator.DRAPodComponentLabel},
		len(nodes.Items)),
	).To(Succeed(), "the fake DRA kubelet plugin must become ready on the labeled nodes")

	// Each node publishes one pool (pool name = node name) with a complete
	// snapshot carrying the frozen facts.
	for i := range nodes.Items {
		node := &nodes.Items[i]
		snapshot, snapErr := simulator.WaitForResourceSlices(ctx, cli, simulator.DRADriver, node.Name)
		g.Expect(snapErr).NotTo(HaveOccurred(), "ResourceSlices for pool %s", node.Name)
		verifySnapshotFacts(t, snapshot, frozenGpuProduct, frozenGpuMemoryMi)
	}

	// The gate-approved fact-mutation protocol: update the topology input,
	// restart the affected dra-plugin pods, wait for the complete changed
	// ResourceSlice snapshot.
	verifyFactMutationProtocol(t, ctx, cli, &nodes.Items[0])
}

// verifyFactMutationProtocol executes the frozen DRA fact-mutation protocol
// on one node and verifies the changed facts in the republished snapshot.
func verifyFactMutationProtocol(
	t *testing.T,
	ctx context.Context,
	cli client.Client,
	node *corev1.Node,
) {
	t.Helper()
	g := NewWithT(t)

	previous, err := simulator.WaitForResourceSlices(ctx, cli, simulator.DRADriver, node.Name)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(previous.Complete).To(BeTrue(), "the pre-mutation snapshot must be complete")

	// Step 1: update the topology input — facts only, identity untouched.
	g.Expect(simulator.MutateNodeTopology(ctx, cli, node.Name, func(doc map[string]any) {
		doc["gpuMemory"] = mutatedGpuMemoryMi
	})).To(Succeed(), "mutating the topology input must succeed")

	// The mutated input must be visible in the source evidence before the
	// plugin is restarted.
	topology, err := simulator.GetNodeTopology(ctx, cli, node.Name)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(topology.GpuMemoryMi).To(Equal(mutatedGpuMemoryMi))
	g.Expect(topology.GPUIDs).To(HaveLen(frozenGpuCount), "a facts mutation must not change identity")

	// Step 2: restart the affected dra-plugin pods. The native FGO plugin
	// enumerates once at startup; it does not republish on ConfigMap
	// updates.
	g.Expect(simulator.RestartDRAPlugin(ctx, cli)).To(Succeed(),
		"the dra-plugin pods must be recreatable by their DaemonSet")

	// Step 3: wait for the complete changed snapshot (higher generation or
	// in-place device change, plus the full resourceSliceCount).
	changed, err := simulator.WaitForChangedResourceSlices(
		ctx, cli, simulator.DRADriver, node.Name, previous)
	g.Expect(err).NotTo(HaveOccurred(), "the changed ResourceSlice snapshot must appear")
	g.Expect(changed.Complete).To(BeTrue())
	t.Logf("fact mutation republished (ResourceSlice generation %d)", changed.Generation)

	// The republished facts carry the mutated memory.
	verifySnapshotFacts(t, changed, frozenGpuProduct, mutatedGpuMemoryMi)
}

// verifySnapshotFacts asserts the frozen pool facts on a complete snapshot.
func verifySnapshotFacts(
	t *testing.T,
	snapshot simulator.PoolSnapshot,
	product string,
	memoryMi int,
) {
	t.Helper()
	g := NewWithT(t)

	g.Expect(snapshot.Complete).To(BeTrue(), "pool %s must satisfy its resourceSliceCount", snapshot.Pool)
	g.Expect(snapshot.Generation).To(BeNumerically(">", 0))

	devices := 0
	for i := range snapshot.Slices {
		for j := range snapshot.Slices[i].Spec.Devices {
			device := &snapshot.Slices[i].Spec.Devices[j]
			devices++

			g.Expect(device.Name).To(HavePrefix("gpu-"), "device names are lowercased GPU-<uuid>")
			g.Expect(device.Attributes["gpu.nvidia.com/type"].StringValue).To(HaveValue(Equal("gpu")))
			g.Expect(device.Attributes["gpu.nvidia.com/productName"].StringValue).
				To(HaveValue(Equal(product)))
			g.Expect(device.Attributes["gpu.nvidia.com/uuid"].StringValue).NotTo(BeNil())

			capacity, ok := device.Capacity["memory"]
			g.Expect(ok).To(BeTrue(), "device %s must advertise memory capacity", device.Name)
			expected := resource.NewQuantity(int64(memoryMi)*1024*1024, resource.BinarySI)
			g.Expect(capacity.Value.Cmp(*expected)).To(Equal(0),
				"device %s memory must be %d MiB in bytes", device.Name, memoryMi)
		}
	}
	g.Expect(devices).To(Equal(frozenGpuCount), "pool %s must expose the frozen gpuCount", snapshot.Pool)
}

// selectRuntime resolves and verifies the container runtime prerequisite.
// Missing prerequisites are deterministic failures, never skips.
func selectRuntime(t *testing.T) cluster.Runtime {
	t.Helper()

	value := strings.TrimSpace(os.Getenv(runtimeEnvKey))
	if value == "" {
		value = string(cluster.DefaultRuntime)
	}

	runtime := cluster.Runtime(value)
	if runtime != cluster.RuntimePodman && runtime != cluster.RuntimeDocker {
		t.Fatalf("unsupported %s=%q (supported: %s, %s)",
			runtimeEnvKey, value, cluster.RuntimePodman, cluster.RuntimeDocker)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, string(runtime), "info", "--format", "{{.Version.Version}}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("e2e prerequisite failed: container runtime %q is not usable: %v: %s\n"+
			"the isolated simulator-profile execution requires a working container runtime; "+
			"this is a deterministic failure, not a skip", runtime, err, strings.TrimSpace(string(out)))
	}
	t.Logf("container runtime %s available (version %s)",
		runtime, strings.TrimSpace(string(out)))

	return runtime
}

// collectDiagnostics gathers cluster logs, events, pod logs and simulator
// source snapshots into a temporary directory before the failed run's
// cluster is destroyed. The path is reported through t.Logf.
func collectDiagnostics(t *testing.T, inst cluster.Instance, profile simulator.Profile) {
	t.Helper()

	dir, err := os.MkdirTemp("", "hw-disc-e2e-"+profile.String()+"-")
	if err != nil {
		t.Logf("could not create diagnostics directory: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), diagnosticsTimeout)
	defer cancel()

	var errs []error
	if diagErr := inst.CollectDiagnostics(ctx, dir); diagErr != nil {
		errs = append(errs, diagErr)
	}
	if logErr := cluster.CollectPodLogs(ctx, inst.Config(), simulator.Namespace, dir); logErr != nil {
		errs = append(errs, logErr)
	}
	if snapErr := simulator.CollectSnapshot(ctx, inst.Client(), dir); snapErr != nil {
		errs = append(errs, snapErr)
	}

	if len(errs) > 0 {
		t.Logf("diagnostics for %s partially collected in %s: %v", inst.Name(), dir, errs)
		return
	}
	t.Logf("diagnostics for %s collected in %s (pod logs under %s)",
		inst.Name(), dir, filepath.Join(dir, "pod-logs"))
}
