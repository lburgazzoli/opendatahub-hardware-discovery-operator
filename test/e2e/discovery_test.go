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

// TestHardwareDiscoveryDemonstration is the task-011 two-plane
// demonstration: the actual service chart — real ServiceAccount, RBAC and
// leader election, never a host-side admin manager substitute — running
// against the driver-free classic and DRA simulator inputs on run-owned
// Kind clusters. Every scenario observes behavior through objects in the
// cluster; the only test-process controller is the incarnation-race
// manager (incarnation_race_test.go), which runs the production
// controller registration on the chart's own ServiceAccount.
package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubernetes "k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"

	jq "github.com/lburgazzoli/gomega-matchers/pkg/matchers/jq"
	k8s "github.com/lburgazzoli/gomega-matchers/pkg/matchers/k8s"
	condition "github.com/lburgazzoli/gomega-matchers/pkg/matchers/k8s/condition"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"

	infrav1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	devicecontroller "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/device"
	inventorycontroller "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/inventory"
	classicsource "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/classic"
	drasource "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/dra"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/test/support/cluster"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/test/support/helm"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/test/support/simulator"
)

// Contract values asserted by the scenarios, resolved from the exported
// API definitions — the framework status contract, the API's device
// class, allocation plane, identity-kind, source-kind and outcome enums,
// the controllers' condition types and the sources' registered IDs —
// instead of literal strings. The condition reasons have no exported
// identifiers (the controllers keep them package-private), so the
// scenarios state the design-doc values themselves.
const (
	conditionTypeReady    = string(fwapi.ConditionTypeReady)
	conditionTypeDegraded = string(fwapi.ConditionTypeDegraded)
	conditionTypeObserved = devicecontroller.ConditionTypeObserved

	planeClassic = infrav1alpha1.AllocationPlaneClassic
	planeDRA     = infrav1alpha1.AllocationPlaneDRA

	sourceKindClassic = classicsource.SourceID
	sourceKindDRA     = drasource.SourceID

	reasonObserved             = "Observed"
	reasonSourceUnavailable    = "SourceUnavailable"
	reasonPoolIncomplete       = "PoolIncomplete"
	reasonInvalidConfiguration = "InvalidConfiguration"
)

// Service chart identity. The reviewed chart pins fullnameOverride, so
// every rendered object name is fixed regardless of the release name.
const (
	// chartRelease is the Helm release name of the service installation.
	chartRelease = "hardware-discovery"

	// chartNamespace is the service install namespace.
	chartNamespace = "opendatahub-hardware-discovery-operator-system"

	// controllerDeployment is the rendered manager Deployment name.
	controllerDeployment = "opendatahub-hardware-discovery-operator-controller-manager"

	// controllerServiceAccount is the rendered manager ServiceAccount.
	controllerServiceAccount = "opendatahub-hardware-discovery-operator-controller-manager"

	// singletonName is the only legal HardwareDiscovery instance name.
	singletonName = inventorycontroller.SingletonName

	// chartDir is the reviewed service chart, relative to the test
	// package's working directory.
	chartDir = "../../dist/chart"

	// evidenceDir holds the run's evidence documents (gitignored).
	evidenceDir = "../../dist/e2e"
)

// Environment keys carrying the locally built service image; set by
// make/e2e.mk, which builds the image before running the suite.
const (
	imageRepositoryEnvKey = "E2E_CONTROLLER_IMAGE_REPOSITORY"
	imageTagEnvKey        = "E2E_CONTROLLER_IMAGE_TAG"

	defaultImageRepository = "localhost/opendatahub-hardware-discovery-operator"
	defaultImageTag        = "e2e"
)

// Scenario timings (task 011): short rescan and heartbeat bounds keep
// inventory recreation and status refresh observable without fixed
// sleeps; the node grace period stays at its default (no node-deletion
// scenario runs).
const (
	scenarioRescanPeriod = 10 * time.Second
	scenarioMaxStale     = 15 * time.Second
)

// GFD-style node labels (approved simulator/test profile): the scenario
// data a real classic cluster's GFD publishes. The keys and values are
// frozen by the profile; the expected identities are derived in the test,
// never through the implementation's helpers.
const (
	gfdLabelProduct = "nvidia.com/gpu.product"
	gfdLabelMemory  = "nvidia.com/gpu.memory"
	gfdLabelCount   = "nvidia.com/gpu.count"
)

// demonstrationTimeout bounds one complete profile run: cluster creation,
// simulator and service installs, all scenarios, the incarnation race
// (classic only), garbage collection and the deliberate uninstall.
const demonstrationTimeout = 35 * time.Minute

// discoveryEvidenceTimeout bounds Eventually waits for controller-driven
// convergence: scan, rescan, heartbeat and watch propagation.
const discoveryEvidenceTimeout = 3 * time.Minute

// discoveryPollInterval is the observation poll of the Eventually waits.
const discoveryPollInterval = 2 * time.Second

// TestHardwareDiscoveryDemonstration runs the two-plane demonstration:
// the classic profile first, then DRA, each in its own run-owned cluster.
func TestHardwareDiscoveryDemonstration(t *testing.T) {
	t.Log("two-plane hardware discovery demonstration (task 011):",
		"the service chart reconciling simulator inputs on run-owned kind clusters")

	for _, profile := range []simulator.Profile{simulator.ProfileClassic, simulator.ProfileDRA} {
		t.Run(profile.String(), func(t *testing.T) {
			demonstrateDiscovery(t, profile)
		})
	}
}

// demonstrateDiscovery drives one profile's demonstration end to end.
func demonstrateDiscovery(t *testing.T, profile simulator.Profile) {
	t.Helper()
	g := NewWithT(t)

	// Package-level async assertions (Eventually with a context) report
	// through Gomega's global fail handler; bind it to this subtest so a
	// failure fails the right test.
	RegisterTestingT(t)

	// Give controller-runtime a logger before the first client exists:
	// without one it warns once and then swallows every log line —
	// including the in-process race manager's reconcile errors. Errors
	// and warnings go to stderr; routine info stays muted.
	ctrl.SetLogger(logr.FromSlogHandler(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelWarn})))

	runtime := selectRuntime(t)
	imageRepository, imageTag := serviceImage(t, runtime)

	ctx, cancel := context.WithTimeout(context.Background(), demonstrationTimeout)
	defer cancel()

	clusterOpts := []cluster.Option{
		cluster.WithRuntime(runtime),
		cluster.WithLogFn(t.Logf),
	}
	inst, err := cluster.New(ctx, clusterOpts...)
	g.Expect(err).NotTo(HaveOccurred(), "creating the run-owned kind cluster must succeed")

	t.Cleanup(func() {
		if t.Failed() {
			collectDiagnostics(t, inst, profile)
		}
		if stopErr := inst.Stop(context.Background()); stopErr != nil {
			t.Errorf("destroying the run-owned cluster %s: %v", inst.Name(), stopErr)
		}
	})

	g.Expect(inst.Setup(ctx)).To(Succeed(), "the cluster nodes must become ready")

	cli := inst.Client()
	runner, err := helm.NewRunner(inst.KubeconfigPath())
	g.Expect(err).NotTo(HaveOccurred())

	// The service image is cargo in the host runtime; loading it into the
	// exact run-owned cluster is the only registry-free delivery path.
	imageRef := imageRepository + ":" + imageTag
	g.Expect(inst.LoadImage(ctx, imageRef)).To(Succeed(),
		"the locally built service image must load into the run-owned cluster")

	t.Logf("installing simulator profile %s on cluster %s", profile, inst.Name())
	g.Expect(simulator.Setup(ctx, cli, runner, profile)).To(Succeed(),
		"the frozen approved simulator profile must install cleanly")

	nodes := &corev1.NodeList{}
	g.Expect(cli.List(ctx, nodes)).To(Succeed())
	g.Expect(nodes.Items).NotTo(BeEmpty())

	// The classic plane's discovery input: GFD-style node labels applied
	// as scenario data (the simulator publishes only the extended
	// resource through its fake device plugin).
	if profile == simulator.ProfileClassic {
		gfdLabels := map[string]string{
			gfdLabelProduct: frozenGpuProduct,
			gfdLabelMemory:  strconv.Itoa(frozenGpuMemoryMi),
			gfdLabelCount:   strconv.Itoa(frozenGpuCount),
		}
		for i := range nodes.Items {
			g.Expect(updateNodeLabels(ctx, cli, nodes.Items[i].Name, gfdLabels, nil)).To(Succeed(),
				"applying the GFD scenario labels to node %s", nodes.Items[i].Name)
		}
	}

	// The service runs from its reviewed chart with the built image: real
	// ServiceAccount, RBAC and leader election, installed through Helm
	// only (never raw Kustomize).
	chartRef, err := helm.LocalChartRef(chartDir)
	g.Expect(err).NotTo(HaveOccurred(), "the reviewed chart must exist at dist/chart (run `make helm-chart`)")
	imageValues, err := helm.ControllerImageValues(imageRepository, imageTag)
	g.Expect(err).NotTo(HaveOccurred())
	t.Logf("installing the service chart %s on cluster %s", chartRef, inst.Name())
	g.Expect(runner.Install(ctx, helm.InstallOptions{
		Release:   chartRelease,
		Chart:     chartRef,
		Namespace: chartNamespace,
		Values:    imageValues,
	})).To(Succeed(), "the service chart must install cleanly with its readiness wait")
	waitForControllerDeployment(t, ctx, cli)

	// The generated CRD constraints are validated through real API
	// requests against this API server, never a fake-client claim.
	verifySingletonValidation(t, ctx, cli)

	// Minimal legal singleton first, so the schema defaults are observed
	// on a real create; the scenario timings follow as an update.
	single := createSingleton(t, ctx, cli)
	verifySingletonDefaults(t, ctx, cli, single)
	configureSingleton(t, ctx, cli, profile)

	// Discovery scenarios. The incarnation race (classic) is deliberately
	// last among the device scenarios: it stops the in-cluster controller
	// and finishes with the test-process manager.
	switch profile {
	case simulator.ProfileClassic:
		verifyClassicDiscovery(t, ctx, cli, nodes)
		verifyNoDuplicateDevices(t, ctx, cli, nodes, profile)
		verifyDeviceIdentityImmutability(t, ctx, cli)
		verifyHeartbeatRefreshStability(t, ctx, cli)
		verifyClassicFieldRemoval(t, ctx, cli, &nodes.Items[0])
		verifySelectorPruneAndReselect(t, ctx, cli, nodes, profile)
		verifyServerVersionAndStatusIsolation(t, ctx, cli, inst)
		verifyIncarnationRace(t, ctx, inst, cli, nodes)
	case simulator.ProfileDRA:
		verifyDRADiscovery(t, ctx, cli, nodes)
		verifyNoDuplicateDevices(t, ctx, cli, nodes, profile)
		verifyDeviceIdentityImmutability(t, ctx, cli)
		verifyHeartbeatRefreshStability(t, ctx, cli)
		verifyDRAFactMutation(t, ctx, cli, &nodes.Items[0])
		verifyDRAIncompletePool(t, ctx, cli, &nodes.Items[0])
		verifySelectorPruneAndReselect(t, ctx, cli, nodes, profile)
		verifyServerVersionAndStatusIsolation(t, ctx, cli, inst)
	}

	writeProfileEvidence(t, ctx, cli, profile, inst)

	// Deleting the singleton is the documented reset path: the service's
	// owned devices are garbage-collected through their controller
	// reference, without any controller cooperation.
	verifySingletonDeletionGC(t, ctx, cli)

	// Uninstalling the release must retain the discovery CRDs (the chart
	// ships helm.sh/resource-policy: keep on both CRDs).
	verifyHelmUninstallRetainsCRDs(t, ctx, cli, runner)
}

// serviceImage resolves the locally built service image reference and
// verifies its presence in the host container runtime. A missing image is
// a deterministic prerequisite failure with the remedy in the message,
// never a skip.
func serviceImage(t *testing.T, runtime cluster.Runtime) (string, string) {
	t.Helper()

	repository := strings.TrimSpace(os.Getenv(imageRepositoryEnvKey))
	if repository == "" {
		repository = defaultImageRepository
	}
	tag := strings.TrimSpace(os.Getenv(imageTagEnvKey))
	if tag == "" {
		tag = defaultImageTag
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, string(runtime), "image", "inspect",
		repository+":"+tag, "--format", "{{.Id}}")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("e2e prerequisite failed: service image %s:%s is not present in the %s runtime: %v: %s\n"+
			"build it first (make e2e-image, or run the suite through make test-e2e / make demo)",
			repository, tag, runtime, err, strings.TrimSpace(string(out)))
	} else {
		t.Logf("service image %s:%s available (%s)",
			repository, tag, strings.TrimSpace(string(out)))
	}

	return repository, tag
}

// waitForControllerDeployment observes the chart-rendered Deployment
// becoming available (Helm already waited; this is the cluster-side
// observation through the API).
func waitForControllerDeployment(t *testing.T, ctx context.Context, cli client.Client) {
	t.Helper()

	key := client.ObjectKey{Namespace: chartNamespace, Name: controllerDeployment}
	Eventually(ctx, func(g Gomega) {
		deployment := &appsv1.Deployment{}
		g.Expect(cli.Get(ctx, key, deployment)).To(Succeed())
		g.Expect(deployment.Status.AvailableReplicas).To(Equal(int32(1)),
			"the chart-rendered manager deployment must be available")
	}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed())
}

// verifySingletonValidation proves the generated CRD constraints through
// real create requests that the API server must reject: the singleton
// name rule, an invalid node selector and an invalid timing value.
func verifySingletonValidation(t *testing.T, ctx context.Context, cli client.Client) {
	t.Helper()
	g := NewWithT(t)

	// Only "cluster" is a legal HardwareDiscovery name (root-schema CEL).
	renamed := &infrav1alpha1.HardwareDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "not-cluster"},
		Spec:       infrav1alpha1.HardwareDiscoverySpec{Class: infrav1alpha1.DeviceClassAccelerator},
	}
	err := cli.Create(ctx, renamed)
	g.Expect(err).To(HaveOccurred(), "a HardwareDiscovery named differently must be rejected")
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "the rejection must be a schema/CEL invalid error")
	g.Expect(err.Error()).To(ContainSubstring("must be named 'cluster'"))

	// An invalid node selector operator is a controller-level rejection,
	// not an API-level one: CRDs receive LabelSelector structural
	// validation only — operator semantics are compiled-in validation of
	// built-in types — and the design deliberately requires no webhook.
	// The create is therefore accepted, and the controller must classify
	// the configuration as invalid instead.
	invalidSelector := singletonManifest(map[string]any{
		"class": string(infrav1alpha1.DeviceClassAccelerator),
		"nodeSelector": map[string]any{
			"matchExpressions": []any{map[string]any{
				"key": "example.com/select", "operator": "NotAnOperator", "values": []any{"v"},
			}},
		},
	})
	g.Expect(cli.Create(ctx, invalidSelector)).To(Succeed(),
		"an invalid node selector must be accepted by the API server (structural validation only, no webhook)")

	Eventually(ctx, func(g Gomega) {
		stored := &infrav1alpha1.HardwareDiscovery{}
		g.Expect(cli.Get(ctx, client.ObjectKey{Name: singletonName}, stored)).To(Succeed())
		g.Expect(stored).To(WithTransform(k8s.Conditions(), ContainElement(And(
			condition.Is(conditionTypeDegraded, string(metav1.ConditionTrue)),
			condition.HasReason(reasonInvalidConfiguration),
			WithTransform(jq.Extract(`.message`),
				ContainSubstring("spec.nodeSelector is invalid")),
		))))
	}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed(),
		"the controller must reject the invalid node selector at reconcile time")

	// Remove the invalid singleton so the minimal-singleton flow starts
	// from a clean slate.
	g.Expect(cli.Delete(ctx, invalidSelector)).To(Succeed())
	Eventually(ctx, func(g Gomega) {
		err := cli.Get(ctx, client.ObjectKey{Name: singletonName}, &infrav1alpha1.HardwareDiscovery{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the deleted singleton must be gone")
	}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed())

	// A rescanPeriod outside the accepted pattern is rejected.
	invalidTiming := singletonManifest(map[string]any{
		"class":        string(infrav1alpha1.DeviceClassAccelerator),
		"rescanPeriod": "not-a-duration",
	})
	err = cli.Create(ctx, invalidTiming)
	g.Expect(err).To(HaveOccurred(), "an invalid rescanPeriod must be rejected")
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue())
}

// singletonManifest builds an unstructured HardwareDiscovery "cluster"
// body from a raw spec map, for API requests whose values cannot be
// expressed through the typed client.
func singletonManifest(spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": infrav1alpha1.GroupVersion.String(),
		"kind":       "HardwareDiscovery",
		"metadata":   map[string]any{"name": singletonName},
		"spec":       spec,
	}}
}

// createSingleton creates the legal minimal singleton: class only, every
// other field defaulted by the CRD.
func createSingleton(t *testing.T, ctx context.Context, cli client.Client) *infrav1alpha1.HardwareDiscovery {
	t.Helper()
	g := NewWithT(t)

	single := &infrav1alpha1.HardwareDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: singletonName},
		Spec:       infrav1alpha1.HardwareDiscoverySpec{Class: infrav1alpha1.DeviceClassAccelerator},
	}
	g.Expect(cli.Create(ctx, single)).To(Succeed(), "the minimal legal singleton must be accepted")
	return single
}

// verifySingletonDefaults reads the singleton back through the API and
// asserts the schema defaults the CRD applies on creation.
func verifySingletonDefaults(t *testing.T, ctx context.Context, cli client.Client, single *infrav1alpha1.HardwareDiscovery) {
	t.Helper()
	g := NewWithT(t)

	stored := &infrav1alpha1.HardwareDiscovery{}
	g.Expect(cli.Get(ctx, client.ObjectKey{Name: singletonName}, stored)).To(Succeed())

	g.Expect(stored.Spec.MaxStale.Duration).To(Equal(10*time.Minute), "default maxStale")
	g.Expect(stored.Spec.RescanPeriod.Duration).To(Equal(5*time.Minute), "default rescanPeriod")
	g.Expect(stored.Spec.NodeGracePeriod.Duration).To(Equal(15*time.Minute), "default nodeGracePeriod")
	// The source arms stay absent: API-server defaulting fills fields of
	// present objects but never materializes an omitted optional object.
	// "Enabled by default" is behavioral — a nil arm means enabled,
	// matching the schema default — and is proven by discovery itself:
	// devices appear on both profiles with no explicit source config,
	// and the DRA profile's explicit disablement below proves the arm is
	// honored when present.
	g.Expect(stored.Spec.Sources.Classic).To(BeNil(),
		"an omitted classic source arm must stay absent (defaulting is behavioral, not schema-materialized)")
	g.Expect(stored.Spec.Sources.DRA).To(BeNil(),
		"an omitted DRA source arm must stay absent (defaulting is behavioral, not schema-materialized)")
}

// configureSingleton updates the singleton to the scenario
// configuration: bounded rescan and heartbeat periods, and for the DRA
// profile the explicit classic-source disablement that makes the plane
// exclusivity observable.
func configureSingleton(t *testing.T, ctx context.Context, cli client.Client, profile simulator.Profile) {
	t.Helper()
	g := NewWithT(t)

	single := &infrav1alpha1.HardwareDiscovery{}
	g.Expect(cli.Get(ctx, client.ObjectKey{Name: singletonName}, single)).To(Succeed())

	single.Spec.RescanPeriod = metav1.Duration{Duration: scenarioRescanPeriod}
	single.Spec.MaxStale = metav1.Duration{Duration: scenarioMaxStale}
	if profile == simulator.ProfileDRA {
		// The arm is written explicitly: the minimal singleton's source
		// arms are absent (nil), and the update must materialize the
		// classic arm to disable it.
		disabled := false
		single.Spec.Sources.Classic = &infrav1alpha1.ClassicSourceConfig{Enabled: &disabled}
	}

	g.Expect(cli.Update(ctx, single)).To(Succeed(), "the scenario configuration must be accepted")
}

// classicDeviceName independently derives the expected object name of a
// classic device: the synthetic node-scoped identity
// "<node>-gpu-<index>" published by the approved profile.
func classicDeviceName(node string, index int) string {
	return node + "-gpu-" + strconv.Itoa(index)
}

// classicExpectedNames derives the full expected device name set of the
// classic profile: gpu.count identities per labeled node.
func classicExpectedNames(nodes *corev1.NodeList) []string {
	names := make([]string, 0, len(nodes.Items)*frozenGpuCount)
	for i := range nodes.Items {
		for index := range frozenGpuCount {
			names = append(names, classicDeviceName(nodes.Items[i].Name, index))
		}
	}
	return names
}

// verifyClassicDiscovery proves the default all-node classic discovery:
// one nodeIndex identity per gpu.count index, homogeneous facts, classic
// provenance and the deployed framework status contract.
func verifyClassicDiscovery(t *testing.T, ctx context.Context, cli client.Client, nodes *corev1.NodeList) {
	t.Helper()
	g := NewWithT(t)

	// Plane exclusivity of the classic profile: no DRA advertisement
	// exists anywhere in the cluster.
	slices, err := simulator.ListResourceSlices(ctx, cli)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(slices).To(BeEmpty(), "the classic profile publishes no ResourceSlices")

	expected := classicExpectedNames(nodes)
	Eventually(ctx, func(g Gomega) {
		devices := &infrav1alpha1.HardwareDeviceList{}
		g.Expect(cli.List(ctx, devices)).To(Succeed())
		g.Expect(deviceNames(devices.Items)).To(ConsistOf(expected),
			"every gpu.count index of every labeled node must become exactly one device")
	}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed())

	for i := range nodes.Items {
		node := &nodes.Items[i]
		for index := range frozenGpuCount {
			verifyClassicDevice(t, ctx, cli, node, index)
		}
	}

	verifySingletonSummaries(t, ctx, cli, nodes, planeClassic)
}

// verifyClassicDevice asserts one classic device's identity contract,
// observed facts and framework status.
func verifyClassicDevice(
	t *testing.T,
	ctx context.Context,
	cli client.Client,
	node *corev1.Node,
	index int,
) {
	t.Helper()
	g := NewWithT(t)

	name := classicDeviceName(node.Name, index)
	expectedMemory := resource.MustParse(strconv.Itoa(frozenGpuMemoryMi) + "Mi")

	dev := &infrav1alpha1.HardwareDevice{}
	Eventually(ctx, func(g Gomega) {
		g.Expect(cli.Get(ctx, client.ObjectKey{Name: name}, dev)).To(Succeed())
		verifyDeviceStatus(g, dev, expectedDeviceFacts{
			model:     frozenGpuProduct,
			memory:    &expectedMemory,
			plane:     planeClassic,
			sourceRef: sourceKindClassic + "/" + node.Name,
		})
	}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed(),
		"classic device %s must converge to the frozen facts", name)

	g.Expect(dev.Spec.Class).To(Equal(infrav1alpha1.DeviceClassAccelerator))
	g.Expect(dev.Spec.Identity.Kind).To(Equal(infrav1alpha1.DeviceIdentityKindNodeIndex),
		"classic identities are nodeIndex")
	g.Expect(dev.Spec.Identity.ID).To(Equal(name))
	g.Expect(dev.Spec.Host.Name).To(Equal(node.Name))
	g.Expect(dev.Spec.Host.UID).To(Equal(node.UID))
	g.Expect(dev.Spec.Source.Kind).To(Equal(infrav1alpha1.DeviceSourceKindClassic))
	g.Expect(dev.Spec.Source.Classic).NotTo(BeNil())
	g.Expect(dev.Spec.Source.Classic.Node).To(Equal(node.Name))
	g.Expect(dev.Spec.Source.Classic.Index).To(Equal(int32(index)))
	g.Expect(dev.OwnerReferences).NotTo(BeEmpty(), "devices are owned by the singleton for garbage collection")
}

// expectedDeviceFacts is the expected observation outcome of one device.
type expectedDeviceFacts struct {
	model     string
	memory    *resource.Quantity
	plane     infrav1alpha1.AllocationPlane
	sourceRef string
}

// verifyDeviceStatus asserts the deployed framework status contract of
// one device against expected facts: Ready/Observed conditions, matching
// condition generations, custom fact survival and no finalizers.
func verifyDeviceStatus(g Gomega, dev *infrav1alpha1.HardwareDevice, expected expectedDeviceFacts) {
	// The framework conditions come out of the k8s extractor as
	// map-backed documents, so the status values are plain strings and
	// condition.Is/HasReason and jq.Matchf compose on the very same
	// elements.
	g.Expect(dev).To(WithTransform(k8s.Conditions(), ContainElements(
		condition.Is(conditionTypeReady, string(metav1.ConditionTrue)),
		And(
			condition.Is(conditionTypeObserved, string(metav1.ConditionTrue)),
			condition.HasReason(reasonObserved),
			jq.Matchf(`.observedGeneration == %d`, dev.Generation),
		),
	)))

	g.Expect(dev.Status.AllocationPlane).To(Equal(expected.plane))
	g.Expect(dev.Status.SourceRef).To(Equal(expected.sourceRef))
	g.Expect(dev.Status.CollectedAt).NotTo(BeNil(), "a successful observation stamps collectedAt")

	g.Expect(dev.Status.Accelerator).NotTo(BeNil(), "accelerator-class facts are present after observation")
	g.Expect(dev.Status.Accelerator.Model).To(Equal(expected.model))
	if expected.memory == nil {
		g.Expect(dev.Status.Accelerator.Memory).To(BeNil(),
			"device %s memory fact must be absent", dev.Name)
	} else {
		g.Expect(dev.Status.Accelerator.Memory).NotTo(BeNil())
		g.Expect(dev.Status.Accelerator.Memory.Cmp(*expected.memory)).To(Equal(0),
			"device %s memory fact", dev.Name)
	}

	g.Expect(dev.Finalizers).To(BeEmpty(), "the reconciler never adds device finalizers")
}

// conditionOf returns the condition of the given type, or nil.
func conditionOf(conditions []fwapi.Condition, conditionType string) *fwapi.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// deviceNames renders the name set of a device list.
func deviceNames(devices []infrav1alpha1.HardwareDevice) []string {
	names := make([]string, 0, len(devices))
	for i := range devices {
		names = append(names, devices[i].Name)
	}
	return names
}

// verifyNoDuplicateDevices proves the absence of duplicate objects: the
// expected count with unique names.
func verifyNoDuplicateDevices(
	t *testing.T,
	ctx context.Context,
	cli client.Client,
	nodes *corev1.NodeList,
	profile simulator.Profile,
) {
	t.Helper()
	g := NewWithT(t)

	expectedCount := len(nodes.Items) * frozenGpuCount
	if profile == simulator.ProfileDRA {
		expectedCount = expectedDRADeviceCount(t, ctx, cli, nodes)
	}

	devices := &infrav1alpha1.HardwareDeviceList{}
	g.Expect(cli.List(ctx, devices)).To(Succeed())
	names := deviceNames(devices.Items)
	g.Expect(names).To(HaveLen(expectedCount), "no duplicate device objects may exist")
	g.Expect(unique(names)).To(HaveLen(len(names)), "device names are unique")
}

// unique removes duplicates from names (for the uniqueness assertion).
func unique(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	result := make([]string, 0, len(names))
	for _, name := range names {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return result
}

// verifyDeviceIdentityImmutability proves the identity immutability CEL
// rule through a real rejected update.
func verifyDeviceIdentityImmutability(t *testing.T, ctx context.Context, cli client.Client) {
	t.Helper()
	g := NewWithT(t)

	dev := &infrav1alpha1.HardwareDevice{}
	g.Expect(cli.Get(ctx, client.ObjectKey{Name: firstDeviceName(t, ctx, cli)}, dev)).To(Succeed())

	dev.Spec.Identity.ID = "mutated-identity"
	err := cli.Update(ctx, dev)
	g.Expect(err).To(HaveOccurred(), "an identity mutation must be rejected")
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue())
	g.Expect(err.Error()).To(ContainSubstring("immutable"))
}

// firstDeviceName returns any device name (deterministically the first
// by list order).
func firstDeviceName(t *testing.T, ctx context.Context, cli client.Client) string {
	t.Helper()
	g := NewWithT(t)

	devices := &infrav1alpha1.HardwareDeviceList{}
	g.Expect(cli.List(ctx, devices)).To(Succeed())
	g.Expect(devices.Items).NotTo(BeEmpty())
	return devices.Items[0].Name
}

// verifyHeartbeatRefreshStability proves stable device UID, spec and
// generation across a status-only refresh: the heartbeat (maxStale) keeps
// rewriting status while the identity contract never moves. This is not
// claimed as a proof of write efficiency — that is the instrumented unit
// check.
func verifyHeartbeatRefreshStability(t *testing.T, ctx context.Context, cli client.Client) {
	t.Helper()
	g := NewWithT(t)

	name := firstDeviceName(t, ctx, cli)
	before := &infrav1alpha1.HardwareDevice{}
	g.Expect(cli.Get(ctx, client.ObjectKey{Name: name}, before)).To(Succeed())
	g.Expect(before.Status.CollectedAt).NotTo(BeNil(), "the device must be observed before the check")

	Eventually(ctx, func(g Gomega) {
		after := &infrav1alpha1.HardwareDevice{}
		g.Expect(cli.Get(ctx, client.ObjectKey{Name: name}, after)).To(Succeed())
		g.Expect(after.Status.CollectedAt).NotTo(BeNil())
		g.Expect(after.Status.CollectedAt.After(before.Status.CollectedAt.Time)).To(BeTrue(),
			"the heartbeat must refresh collectedAt within the maxStale bound")
	}).WithTimeout(3*scenarioMaxStale).WithPolling(discoveryPollInterval).Should(Succeed(),
		"device %s heartbeat refresh", name)

	after := &infrav1alpha1.HardwareDevice{}
	g.Expect(cli.Get(ctx, client.ObjectKey{Name: name}, after)).To(Succeed())
	g.Expect(after.UID).To(Equal(before.UID), "status-only refresh keeps the UID")
	g.Expect(after.Generation).To(Equal(before.Generation), "status-only refresh keeps the generation")
	g.Expect(after.Spec).To(Equal(before.Spec), "status-only refresh keeps the spec")
}

// verifyClassicFieldRemoval proves the SSA field-removal sequence: a
// successful observation stops publishing the memory fact — the
// previously owned status field is actually removed while model survives
// and collectedAt advances — and a subsequent failed observation (a
// malformed memory label) retains the remaining fields without
// restamping collectedAt.
func verifyClassicFieldRemoval(t *testing.T, ctx context.Context, cli client.Client, node *corev1.Node) {
	t.Helper()
	g := NewWithT(t)

	name := classicDeviceName(node.Name, 0)
	expectedMemory := resource.MustParse(strconv.Itoa(frozenGpuMemoryMi) + "Mi")

	// Baseline: model and memory observed.
	baseline := &infrav1alpha1.HardwareDevice{}
	g.Expect(cli.Get(ctx, client.ObjectKey{Name: name}, baseline)).To(Succeed())
	g.Expect(baseline.Status.Accelerator.Memory).NotTo(BeNil())
	g.Expect(baseline.Status.Accelerator.Memory.Cmp(expectedMemory)).To(Equal(0))

	// Step 1: the publisher stops publishing the memory fact.
	g.Expect(updateNodeLabels(ctx, cli, node.Name, nil, []string{gfdLabelMemory})).To(Succeed(),
		"removing the gpu.memory scenario label")

	var removedAt time.Time
	Eventually(ctx, func(g Gomega) {
		dev := &infrav1alpha1.HardwareDevice{}
		g.Expect(cli.Get(ctx, client.ObjectKey{Name: name}, dev)).To(Succeed())
		g.Expect(dev.Status.Accelerator).NotTo(BeNil())
		g.Expect(dev.Status.Accelerator.Memory).To(BeNil(),
			"the previously owned memory field must be removed from the persisted status, not merely omitted")
		g.Expect(dev.Status.Accelerator.Model).To(Equal(frozenGpuProduct),
			"the still-published model fact survives")
		g.Expect(dev.Status.CollectedAt).NotTo(BeNil())
		g.Expect(dev.Status.CollectedAt.After(baseline.Status.CollectedAt.Time)).To(BeTrue(),
			"a successful observation advances collectedAt")
		removedAt = dev.Status.CollectedAt.Time
	}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed())

	// Step 2: a present but malformed memory label is an observation
	// failure — unknown input, never a silently zeroed fact.
	g.Expect(updateNodeLabels(ctx, cli, node.Name,
		map[string]string{gfdLabelMemory: "not-a-number"}, nil)).To(Succeed())

	Eventually(ctx, func(g Gomega) {
		dev := &infrav1alpha1.HardwareDevice{}
		g.Expect(cli.Get(ctx, client.ObjectKey{Name: name}, dev)).To(Succeed())
		g.Expect(dev).To(WithTransform(k8s.Conditions(), And(
			ContainElement(And(
				condition.Is(conditionTypeObserved, string(metav1.ConditionFalse)),
				condition.HasReason(reasonSourceUnavailable),
			)),
			ContainElement(condition.Is(conditionTypeReady, string(metav1.ConditionFalse))),
		)))
		g.Expect(dev.Status.Accelerator).NotTo(BeNil())
		g.Expect(dev.Status.Accelerator.Model).To(Equal(frozenGpuProduct),
			"a failed observation retains the remaining facts")
		g.Expect(dev.Status.Accelerator.Memory).To(BeNil())
		g.Expect(dev.Status.CollectedAt).NotTo(BeNil())
		g.Expect(dev.Status.CollectedAt.Time.Equal(removedAt)).To(BeTrue(),
			"a failed observation never restamps collectedAt")
	}).WithTimeout(3*scenarioMaxStale).WithPolling(discoveryPollInterval).Should(Succeed(),
		"device %s malformed-label observation failure", name)

	// Step 3: restore the fact and observe recovery.
	g.Expect(updateNodeLabels(ctx, cli, node.Name,
		map[string]string{gfdLabelMemory: strconv.Itoa(frozenGpuMemoryMi)}, nil)).To(Succeed())

	Eventually(ctx, func(g Gomega) {
		dev := &infrav1alpha1.HardwareDevice{}
		g.Expect(cli.Get(ctx, client.ObjectKey{Name: name}, dev)).To(Succeed())
		verifyDeviceStatus(g, dev, expectedDeviceFacts{
			model:     frozenGpuProduct,
			memory:    &expectedMemory,
			plane:     planeClassic,
			sourceRef: sourceKindClassic + "/" + node.Name,
		})
	}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed(),
		"device %s must recover the memory fact", name)
}

// expectedDRAIdentities derives the expected device set of one DRA node
// from the published ResourceSlice evidence — the input, never the
// service's own derivation: for each whole-GPU device the UUID attribute
// is the identity, and the object name is the DNS slug of that UUID.
type expectedDRAIdentity struct {
	name   string
	id     string
	device string
	memory *resource.Quantity
	model  string
}

// draExpectedIdentities derives the expected identities of one node from
// its current complete snapshot.
func draExpectedIdentities(
	t *testing.T,
	ctx context.Context,
	cli client.Client,
	node *corev1.Node,
) []expectedDRAIdentity {
	t.Helper()
	g := NewWithT(t)

	snapshot, err := simulator.WaitForResourceSlices(ctx, cli, simulator.DRADriver, node.Name)
	g.Expect(err).NotTo(HaveOccurred(), "ResourceSlices for pool %s", node.Name)
	g.Expect(snapshot.Complete).To(BeTrue())

	identities := make([]expectedDRAIdentity, 0, frozenGpuCount)
	for i := range snapshot.Slices {
		for j := range snapshot.Slices[i].Spec.Devices {
			device := snapshot.Slices[i].Spec.Devices[j]
			uuidAttr := device.Attributes["gpu.nvidia.com/uuid"].StringValue
			g.Expect(uuidAttr).NotTo(BeNil(), "every advertised whole GPU carries a UUID")
			uuid := *uuidAttr

			productAttr := device.Attributes["gpu.nvidia.com/productName"].StringValue
			g.Expect(productAttr).NotTo(BeNil())
			identity := expectedDRAIdentity{
				name:   strings.ToLower(uuid),
				id:     uuid,
				device: device.Name,
				model:  *productAttr,
			}
			if capacity, ok := device.Capacity["memory"]; ok {
				memory := capacity.Value.DeepCopy()
				identity.memory = &memory
			}
			identities = append(identities, identity)
		}
	}
	g.Expect(identities).To(HaveLen(frozenGpuCount))
	return identities
}

// expectedDRADeviceCount returns the total expected device count of the
// DRA profile across all nodes.
func expectedDRADeviceCount(t *testing.T, ctx context.Context, cli client.Client, nodes *corev1.NodeList) int {
	t.Helper()

	total := 0
	for i := range nodes.Items {
		total += len(draExpectedIdentities(t, ctx, cli, &nodes.Items[i]))
	}
	return total
}

// verifyDRADiscovery proves the DRA plane: classic source disabled,
// observations from native v1 whole-GPU advertisements without any GFD
// label or classic allocatable evidence, with the input evidence
// captured.
func verifyDRADiscovery(t *testing.T, ctx context.Context, cli client.Client, nodes *corev1.NodeList) {
	t.Helper()
	g := NewWithT(t)

	// Plane exclusivity of the DRA profile: no GFD labels and no classic
	// extended resource exist — the observations cannot come from the
	// classic source.
	for i := range nodes.Items {
		node := &nodes.Items[i]
		g.Expect(node.Labels).NotTo(HaveKey(gfdLabelProduct), "no GFD product label on node %s", node.Name)
		g.Expect(node.Labels).NotTo(HaveKey(gfdLabelCount), "no GFD count label on node %s", node.Name)
		_, inAllocatable := node.Status.Allocatable[corev1.ResourceName("nvidia.com/gpu")]
		_, inCapacity := node.Status.Capacity[corev1.ResourceName("nvidia.com/gpu")]
		g.Expect(inAllocatable || inCapacity).To(BeFalse(),
			"no classic extended resource on node %s", node.Name)
	}

	// The explicit discovery check before any scenario: the stable API is
	// served (never a beta group, never a fake CRD).
	available, err := cluster.ResourceAPIAvailable(cli)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(available).To(BeTrue(), "stable resource.k8s.io/v1 must be served by the pinned baseline")

	// Gate-free DRA profile (task 011): the service never reads
	// DeviceClasses, so no feature gate is enabled and the alpha
	// extendedResourceName field is not asserted.
	class, err := simulator.GetDeviceClass(ctx, cli)
	g.Expect(err).NotTo(HaveOccurred(), "the chart-rendered gpu.nvidia.com DeviceClass must exist")
	g.Expect(class.Name).To(Equal(simulator.DeviceClassName))

	expected := make([]string, 0, len(nodes.Items)*frozenGpuCount)
	for i := range nodes.Items {
		node := &nodes.Items[i]
		for _, identity := range draExpectedIdentities(t, ctx, cli, node) {
			expected = append(expected, identity.name)
		}
	}

	Eventually(ctx, func(g Gomega) {
		devices := &infrav1alpha1.HardwareDeviceList{}
		g.Expect(cli.List(ctx, devices)).To(Succeed())
		g.Expect(deviceNames(devices.Items)).To(ConsistOf(expected),
			"every advertised whole-GPU UUID must become exactly one device")
	}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed())

	for i := range nodes.Items {
		node := &nodes.Items[i]
		for _, identity := range draExpectedIdentities(t, ctx, cli, node) {
			verifyDRADevice(t, ctx, cli, node, identity)
		}
	}

	verifySingletonSummaries(t, ctx, cli, nodes, planeDRA)
}

// verifyDRADevice asserts one DRA device's identity contract, observed
// facts and framework status.
func verifyDRADevice(
	t *testing.T,
	ctx context.Context,
	cli client.Client,
	node *corev1.Node,
	identity expectedDRAIdentity,
) {
	t.Helper()
	g := NewWithT(t)

	dev := &infrav1alpha1.HardwareDevice{}
	Eventually(ctx, func(g Gomega) {
		g.Expect(cli.Get(ctx, client.ObjectKey{Name: identity.name}, dev)).To(Succeed())
		verifyDeviceStatus(g, dev, expectedDeviceFacts{
			model:     identity.model,
			memory:    identity.memory,
			plane:     planeDRA,
			sourceRef: sourceKindDRA + "/" + simulator.DRADriver + "/" + node.Name,
		})
	}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed(),
		"DRA device %s must converge to the advertised facts", identity.name)

	g.Expect(dev.Spec.Class).To(Equal(infrav1alpha1.DeviceClassAccelerator))
	g.Expect(dev.Spec.Identity.Kind).To(Equal(infrav1alpha1.DeviceIdentityKindUUID),
		"DRA identities are vendor UUIDs")
	g.Expect(dev.Spec.Identity.ID).To(Equal(identity.id))
	g.Expect(dev.Spec.Host.Name).To(Equal(node.Name))
	g.Expect(dev.Spec.Host.UID).To(Equal(node.UID))
	g.Expect(dev.Spec.Source.Kind).To(Equal(infrav1alpha1.DeviceSourceKindDRA))
	g.Expect(dev.Spec.Source.DRA).NotTo(BeNil())
	g.Expect(dev.Spec.Source.DRA.Driver).To(Equal(simulator.DRADriver))
	g.Expect(dev.Spec.Source.DRA.Pool).To(Equal(node.Name))
	g.Expect(dev.Spec.Source.DRA.Device).To(Equal(identity.device))
	g.Expect(dev.OwnerReferences).NotTo(BeEmpty(), "devices are owned by the singleton for garbage collection")
}

// verifyDRAFactMutation proves the service-level status refresh through
// the gate-approved DRA publication protocol: mutate the topology input,
// restart the publisher, wait for the complete changed snapshot, then
// observe the changed facts in device status.
func verifyDRAFactMutation(t *testing.T, ctx context.Context, cli client.Client, node *corev1.Node) {
	t.Helper()
	g := NewWithT(t)

	previous, err := simulator.WaitForResourceSlices(ctx, cli, simulator.DRADriver, node.Name)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(previous.Complete).To(BeTrue(), "the pre-mutation snapshot must be complete")

	identities := draExpectedIdentities(t, ctx, cli, node)

	g.Expect(simulator.MutateNodeTopology(ctx, cli, node.Name, func(doc map[string]any) {
		doc["gpuMemory"] = mutatedGpuMemoryMi
	})).To(Succeed(), "mutating the topology input must succeed")
	g.Expect(simulator.RestartDRAPlugin(ctx, cli)).To(Succeed(),
		"the dra-plugin pods must be recreatable by their DaemonSet")
	changed, err := simulator.WaitForChangedResourceSlices(
		ctx, cli, simulator.DRADriver, node.Name, previous)
	g.Expect(err).NotTo(HaveOccurred(), "the changed ResourceSlice snapshot must appear")
	g.Expect(changed.Complete).To(BeTrue())
	t.Logf("fact mutation republished (ResourceSlice generation %d)", changed.Generation)

	expectedMemory := resource.MustParse(strconv.Itoa(mutatedGpuMemoryMi) + "Mi")
	for _, identity := range identities {
		Eventually(ctx, func(g Gomega) {
			dev := &infrav1alpha1.HardwareDevice{}
			g.Expect(cli.Get(ctx, client.ObjectKey{Name: identity.name}, dev)).To(Succeed())
			verifyDeviceStatus(g, dev, expectedDeviceFacts{
				model:     identity.model,
				memory:    &expectedMemory,
				plane:     planeDRA,
				sourceRef: sourceKindDRA + "/" + simulator.DRADriver + "/" + node.Name,
			})
		}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed(),
			"DRA device %s must refresh to the mutated memory", identity.name)
	}
}

// verifyDRAIncompletePool proves the bounded incomplete-pool regression:
// after a successful observation, a test-only ResourceSlice mutation
// makes the pool incomplete; existing facts, identities and collectedAt
// are retained while Ready=False/Observed=False with PoolIncomplete on
// both the device and the singleton; restoring complete evidence
// recovers readiness.
func verifyDRAIncompletePool(t *testing.T, ctx context.Context, cli client.Client, node *corev1.Node) {
	t.Helper()
	g := NewWithT(t)

	snapshot, err := simulator.WaitForResourceSlices(ctx, cli, simulator.DRADriver, node.Name)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(snapshot.Complete).To(BeTrue())
	originalCount := snapshot.ResourceSliceCount
	g.Expect(originalCount).To(BeNumerically(">", 0))

	identities := draExpectedIdentities(t, ctx, cli, node)

	// Baseline identity and facts of every device of the pool.
	baseline := make(map[string]infrav1alpha1.HardwareDevice, len(identities))
	for _, identity := range identities {
		dev := &infrav1alpha1.HardwareDevice{}
		g.Expect(cli.Get(ctx, client.ObjectKey{Name: identity.name}, dev)).To(Succeed())
		g.Expect(dev.Status.Accelerator).NotTo(BeNil())
		baseline[identity.name] = *dev
	}

	// The incomplete pool: a declared slice count above the published
	// slices. The input stays stable — no plugin restart happens while
	// the mutation is in effect.
	g.Expect(simulator.SetPoolSliceCount(ctx, cli, simulator.DRADriver, node.Name,
		originalCount+1)).To(Succeed(), "making the pool snapshot incomplete")

	retention := func(g Gomega, identity expectedDRAIdentity) {
		dev := &infrav1alpha1.HardwareDevice{}
		g.Expect(cli.Get(ctx, client.ObjectKey{Name: identity.name}, dev)).To(Succeed())
		g.Expect(dev).To(WithTransform(k8s.Conditions(), And(
			ContainElement(And(
				condition.Is(conditionTypeObserved, string(metav1.ConditionFalse)),
				condition.HasReason(reasonPoolIncomplete),
			)),
			ContainElement(condition.Is(conditionTypeReady, string(metav1.ConditionFalse))),
		)))
		g.Expect(dev.UID).To(Equal(baseline[identity.name].UID), "identities are retained")
		g.Expect(dev.Spec.Identity.ID).To(Equal(baseline[identity.name].Spec.Identity.ID))
		g.Expect(dev.Status.Accelerator).NotTo(BeNil())
		g.Expect(dev.Status.Accelerator.Model).
			To(Equal(baseline[identity.name].Status.Accelerator.Model), "facts are retained")
		g.Expect(dev.Status.CollectedAt).NotTo(BeNil())
		g.Expect(dev.Status.CollectedAt.Time.Equal(baseline[identity.name].Status.CollectedAt.Time)).
			To(BeTrue(), "an unknown snapshot never restamps collectedAt")
	}

	for _, identity := range identities {
		Eventually(ctx, func(g Gomega) { retention(g, identity) }).
			WithTimeout(3*scenarioMaxStale).WithPolling(discoveryPollInterval).Should(Succeed(),
			"DRA device %s must report the incomplete pool while retaining its facts", identity.name)
	}

	// The singleton agrees with the source outcome.
	Eventually(ctx, func(g Gomega) {
		single := &infrav1alpha1.HardwareDiscovery{}
		g.Expect(cli.Get(ctx, client.ObjectKey{Name: singletonName}, single)).To(Succeed())
		g.Expect(single).To(WithTransform(k8s.Conditions(), And(
			ContainElement(condition.Is(conditionTypeReady, string(metav1.ConditionFalse))),
			ContainElement(And(
				condition.Is(conditionTypeDegraded, string(metav1.ConditionTrue)),
				condition.HasReason(reasonPoolIncomplete),
			)),
		)))
	}).WithTimeout(3*scenarioMaxStale).WithPolling(discoveryPollInterval).Should(Succeed(),
		"the singleton must degrade on the incomplete pool")

	// Restore the complete evidence and observe recovery.
	g.Expect(simulator.SetPoolSliceCount(ctx, cli, simulator.DRADriver, node.Name,
		originalCount)).To(Succeed(), "restoring the pool slice count")

	for _, identity := range identities {
		Eventually(ctx, func(g Gomega) {
			dev := &infrav1alpha1.HardwareDevice{}
			g.Expect(cli.Get(ctx, client.ObjectKey{Name: identity.name}, dev)).To(Succeed())
			verifyDeviceStatus(g, dev, expectedDeviceFacts{
				model:     identity.model,
				memory:    identity.memory,
				plane:     planeDRA,
				sourceRef: sourceKindDRA + "/" + simulator.DRADriver + "/" + node.Name,
			})
			g.Expect(dev.Status.CollectedAt.After(baseline[identity.name].Status.CollectedAt.Time)).
				To(BeTrue(), "the recovered observation restamps collectedAt")
		}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed(),
			"DRA device %s must recover readiness", identity.name)
	}

	Eventually(ctx, func(g Gomega) {
		single := &infrav1alpha1.HardwareDiscovery{}
		g.Expect(cli.Get(ctx, client.ObjectKey{Name: singletonName}, single)).To(Succeed())
		g.Expect(single).To(WithTransform(k8s.Conditions(), And(
			ContainElement(condition.Is(conditionTypeReady, string(metav1.ConditionTrue))),
			ContainElement(condition.Is(conditionTypeDegraded, string(metav1.ConditionFalse))),
		)))
	}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed(),
		"the singleton must recover with the source")
}

// verifySingletonSummaries asserts the singleton's per-node inventory
// overview and its healthy status contract.
func verifySingletonSummaries(
	t *testing.T,
	ctx context.Context,
	cli client.Client,
	nodes *corev1.NodeList,
	plane infrav1alpha1.AllocationPlane,
) {
	t.Helper()

	Eventually(ctx, func(g Gomega) {
		single := &infrav1alpha1.HardwareDiscovery{}
		g.Expect(cli.Get(ctx, client.ObjectKey{Name: singletonName}, single)).To(Succeed())

		// The whole status contract in one composed assertion: the phase
		// and the per-node summaries are matched field-scoped through jq
		// on the typed object (the k8s converter unwraps it), and the
		// conditions come through the k8s extractor where the condition
		// matchers apply. Exactly one summary per selected node — no
		// missing, no extra — each carrying the node's uid, the profile's
		// plane, the frozen device count and a Success outcome.
		summaryMatchers := make([]types.GomegaMatcher, 0, len(nodes.Items)+1)
		summaryMatchers = append(summaryMatchers,
			jq.Matchf(`.status.nodeSummaries | length == %d`, len(nodes.Items)))
		for i := range nodes.Items {
			node := &nodes.Items[i]
			summaryMatchers = append(summaryMatchers, jq.Matchf(
				`any(.status.nodeSummaries[]; .name == %q and .uid == %q and .plane == %q `+
					`and .deviceCount == %d and .outcome == "Success")`,
				node.Name, string(node.UID), string(plane), frozenGpuCount))
		}
		g.Expect(single).To(And(And(summaryMatchers...),
			WithTransform(k8s.Conditions(), And(
				ContainElement(And(
					condition.Is(conditionTypeReady, string(metav1.ConditionTrue)),
					jq.Matchf(`.observedGeneration == %d`, single.Generation),
				)),
				ContainElement(condition.Is(conditionTypeDegraded, string(metav1.ConditionFalse))),
			)),
		))
	}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed())
}

// verifySelectorPruneAndReselect proves owned pruning on a restrictive
// node selection and full rediscovery on reselection.
func verifySelectorPruneAndReselect(
	t *testing.T,
	ctx context.Context,
	cli client.Client,
	nodes *corev1.NodeList,
	profile simulator.Profile,
) {
	t.Helper()
	g := NewWithT(t)

	// A selector no node satisfies: the deselected devices are pruned
	// immediately (never retained for the node grace period).
	g.Expect(updateSingletonNodeSelector(ctx, cli, map[string]string{
		"e2e.platform.opendatahub.io/select": "absent",
	})).To(Succeed(), "applying a restrictive node selector")

	Eventually(ctx, func(g Gomega) {
		devices := &infrav1alpha1.HardwareDeviceList{}
		g.Expect(cli.List(ctx, devices)).To(Succeed())
		g.Expect(devices.Items).To(BeEmpty(), "deselected devices must be pruned")
	}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed())

	// Reselecting the nodes through the simulator's node-pool label
	// rediscovers the full device set.
	g.Expect(updateSingletonNodeSelector(ctx, cli, map[string]string{
		simulator.NodePoolLabelKey: simulator.DefaultNodePool,
	})).To(Succeed(), "reselecting the nodes")

	expected := classicExpectedNames(nodes)
	if profile == simulator.ProfileDRA {
		expected = make([]string, 0, len(nodes.Items)*frozenGpuCount)
		for i := range nodes.Items {
			for _, identity := range draExpectedIdentities(t, ctx, cli, &nodes.Items[i]) {
				expected = append(expected, identity.name)
			}
		}
	}

	Eventually(ctx, func(g Gomega) {
		devices := &infrav1alpha1.HardwareDeviceList{}
		g.Expect(cli.List(ctx, devices)).To(Succeed())
		g.Expect(deviceNames(devices.Items)).To(ConsistOf(expected),
			"reselected nodes must rediscover their full device sets")
	}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed())
}

// verifyServerVersionAndStatusIsolation pins the demonstration to the
// approved API server baseline and proves spec/status separation through
// the real API: a status write through the main resource endpoint is
// accepted but not persisted.
func verifyServerVersionAndStatusIsolation(
	t *testing.T,
	ctx context.Context,
	cli client.Client,
	inst cluster.Instance,
) {
	t.Helper()
	g := NewWithT(t)

	// The pinned baseline serves the exact node image's version.
	clientset, err := kubernetes.NewForConfig(inst.Config())
	g.Expect(err).NotTo(HaveOccurred())
	version, err := clientset.Discovery().ServerVersion()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(version.GitVersion).To(HavePrefix("v1.34.11"),
		"the demonstration runs against the pinned 1.34.11 API server, have %s", version.GitVersion)
	t.Logf("server version: %s (%s)", version.GitVersion, version.Platform)

	// Spec/status separation through the real API: observedGeneration is
	// a stable status field on the singleton (its generation never
	// changes here), so a probe value written through the main resource
	// endpoint being dropped proves the status payload is discarded.
	stored := &infrav1alpha1.HardwareDiscovery{}
	g.Expect(cli.Get(ctx, client.ObjectKey{Name: singletonName}, stored)).To(Succeed())
	probe := stored.Status.ObservedGeneration + 1000
	stored.Status.ObservedGeneration = probe
	g.Expect(cli.Update(ctx, stored)).To(Succeed(),
		"a main-resource update is accepted (its status payload is dropped)")
	after := &infrav1alpha1.HardwareDiscovery{}
	g.Expect(cli.Get(ctx, client.ObjectKey{Name: singletonName}, after)).To(Succeed())
	g.Expect(after.Status.ObservedGeneration).ToNot(Equal(probe),
		"status written through the main endpoint must not be persisted")
}

// unstructuredFor fetches one infrastructure object as unstructured.
func unstructuredFor(
	t *testing.T,
	ctx context.Context,
	cli client.Client,
	kind string,
	name string,
) *unstructured.Unstructured {
	t.Helper()
	g := NewWithT(t)

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(infrav1alpha1.GroupVersion.WithKind(kind))
	g.Expect(cli.Get(ctx, client.ObjectKey{Name: name}, u)).To(Succeed())
	return u
}

// verifySingletonDeletionGC proves garbage collection on singleton
// deletion: the owned devices disappear through their controller
// reference.
func verifySingletonDeletionGC(t *testing.T, ctx context.Context, cli client.Client) {
	t.Helper()
	g := NewWithT(t)

	single := &infrav1alpha1.HardwareDiscovery{}
	g.Expect(cli.Get(ctx, client.ObjectKey{Name: singletonName}, single)).To(Succeed())
	g.Expect(cli.Delete(ctx, single)).To(Succeed(), "deleting the singleton must be accepted")

	Eventually(ctx, func(g Gomega) {
		devices := &infrav1alpha1.HardwareDeviceList{}
		g.Expect(cli.List(ctx, devices)).To(Succeed())
		g.Expect(devices.Items).To(BeEmpty(),
			"the owned devices must be garbage-collected after singleton deletion")
	}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed())
}

// verifyHelmUninstallRetainsCRDs uninstalls the service release and
// proves the discovery CRDs are retained.
func verifyHelmUninstallRetainsCRDs(
	t *testing.T,
	ctx context.Context,
	cli client.Client,
	runner *helm.Runner,
) {
	t.Helper()
	g := NewWithT(t)

	g.Expect(runner.Uninstall(ctx, chartRelease, chartNamespace)).To(Succeed(),
		"the service release must uninstall cleanly")

	for _, crdName := range []string{
		"hardwarediscoveries.infrastructure.platform.opendatahub.io",
		"hardwaredevices.infrastructure.platform.opendatahub.io",
	} {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition",
		})
		g.Expect(cli.Get(ctx, client.ObjectKey{Name: crdName}, crd)).To(Succeed(),
			"the discovery CRD %s must be retained after helm uninstall", crdName)
	}
}

// updateNodeLabels applies label changes to one node through a merge
// patch: set adds or replaces keys, remove deletes them.
func updateNodeLabels(
	ctx context.Context,
	cli client.Client,
	nodeName string,
	set map[string]string,
	remove []string,
) error {
	node := &corev1.Node{}
	if err := cli.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		return fmt.Errorf("getting node %s: %w", nodeName, err)
	}

	patch := client.MergeFrom(node.DeepCopy())
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	for key, value := range set {
		node.Labels[key] = value
	}
	for _, key := range remove {
		delete(node.Labels, key)
	}
	if err := cli.Patch(ctx, node, patch); err != nil {
		return fmt.Errorf("patching node %s labels: %w", nodeName, err)
	}
	return nil
}

// updateSingletonNodeSelector replaces the singleton's node selector.
func updateSingletonNodeSelector(ctx context.Context, cli client.Client, matchLabels map[string]string) error {
	single := &infrav1alpha1.HardwareDiscovery{}
	if err := cli.Get(ctx, client.ObjectKey{Name: singletonName}, single); err != nil {
		return fmt.Errorf("getting the singleton: %w", err)
	}
	single.Spec.NodeSelector = &metav1.LabelSelector{MatchLabels: matchLabels}
	if err := cli.Update(ctx, single); err != nil {
		return fmt.Errorf("updating the singleton node selector: %w", err)
	}
	return nil
}

// profileEvidence is the recorded evidence document of one profile run.
type profileEvidence struct {
	Profile       string           `json:"profile"`
	Cluster       string           `json:"cluster"`
	ServerVersion string           `json:"serverVersion"`
	Nodes         []nodeEvidence   `json:"nodes"`
	Devices       []deviceEvidence `json:"devices"`
	Race          *raceEvidence    `json:"race,omitempty"`
}

// nodeEvidence records one node's published input evidence.
type nodeEvidence struct {
	Name    string `json:"name"`
	UID     string `json:"uid"`
	Plane   string `json:"plane,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// deviceEvidence records one discovered device's identity and observed
// facts.
type deviceEvidence struct {
	Name       string       `json:"name"`
	UID        string       `json:"uid"`
	IdentityID string       `json:"identityId"`
	Kind       string       `json:"identityKind"`
	Node       string       `json:"node"`
	Class      string       `json:"class"`
	Plane      string       `json:"plane,omitempty"`
	Model      string       `json:"model,omitempty"`
	Memory     string       `json:"memory,omitempty"`
	SourceRef  string       `json:"sourceRef,omitempty"`
	Collected  *metav1.Time `json:"collectedAt,omitempty"`
	Phase      string       `json:"phase,omitempty"`
	Ready      string       `json:"ready,omitempty"`
	Observed   string       `json:"observed,omitempty"`
}

// writeProfileEvidence persists the run's evidence document under the
// gitignored dist/e2e directory.
func writeProfileEvidence(
	t *testing.T,
	ctx context.Context,
	cli client.Client,
	profile simulator.Profile,
	inst cluster.Instance,
) {
	t.Helper()

	clientset, err := kubernetes.NewForConfig(inst.Config())
	if err != nil {
		t.Logf("evidence: building clientset: %v", err)
		return
	}
	version, err := clientset.Discovery().ServerVersion()
	if err != nil {
		t.Logf("evidence: reading server version: %v", err)
		return
	}

	evidence := profileEvidence{
		Profile:       profile.String(),
		Cluster:       inst.Name(),
		ServerVersion: version.GitVersion,
	}

	nodes := &corev1.NodeList{}
	if err := cli.List(ctx, nodes); err == nil {
		for i := range nodes.Items {
			evidence.Nodes = append(evidence.Nodes, nodeEvidence{
				Name: nodes.Items[i].Name,
				UID:  string(nodes.Items[i].UID),
			})
		}
	}

	devices := &infrav1alpha1.HardwareDeviceList{}
	if err := cli.List(ctx, devices); err != nil {
		t.Logf("evidence: listing devices: %v", err)
		return
	}
	for i := range devices.Items {
		dev := &devices.Items[i]
		entry := deviceEvidence{
			Name:       dev.Name,
			UID:        string(dev.UID),
			IdentityID: dev.Spec.Identity.ID,
			Kind:       string(dev.Spec.Identity.Kind),
			Node:       dev.Spec.Host.Name,
			Class:      string(dev.Spec.Class),
			Plane:      string(dev.Status.AllocationPlane),
			SourceRef:  dev.Status.SourceRef,
			Collected:  dev.Status.CollectedAt,
			Phase:      dev.Status.Phase,
		}
		if dev.Status.Accelerator != nil {
			entry.Model = dev.Status.Accelerator.Model
			if dev.Status.Accelerator.Memory != nil {
				entry.Memory = dev.Status.Accelerator.Memory.String()
			}
		}
		if ready := conditionOf(dev.Status.Conditions, conditionTypeReady); ready != nil {
			entry.Ready = string(ready.Status)
		}
		if observed := conditionOf(dev.Status.Conditions, conditionTypeObserved); observed != nil {
			entry.Observed = string(observed.Status)
		}
		evidence.Devices = append(evidence.Devices, entry)
	}

	if profile == simulator.ProfileClassic {
		evidence.Race = lastRaceEvidence()
	}

	data, err := yaml.Marshal(evidence)
	if err != nil {
		t.Logf("evidence: marshaling: %v", err)
		return
	}

	path := filepath.Join(evidenceDir, profile.String()+"-"+inst.Name()+".yaml")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Logf("evidence: creating %s: %v", evidenceDir, err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Logf("evidence: writing %s: %v", path, err)
		return
	}
	t.Logf("evidence written to %s", path)
}
