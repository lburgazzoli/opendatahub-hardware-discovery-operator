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

// The incarnation-convergence race (status-write
// incarnation semantics): a status write computed against
// one HardwareDevice incarnation can transiently land on a recreated
// object of the same name — a CR /status server-side apply carries no
// optimistic-concurrency precondition and the payload's uid is not a
// guard. The scenario stops the in-cluster controller, runs the
// production controller registration in the test process on the
// chart's own ServiceAccount, holds the first facts-carrying status
// patch inside the transport, deletes and recreates the target device
// underneath it, and proves on the real API server that the released
// patch is accepted and its stale facts are then overwritten by the
// recreated incarnation's own observation within one pass — the
// steady state never describes the stale facts.
package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	. "github.com/onsi/gomega"

	jq "github.com/lburgazzoli/gomega-matchers/pkg/matchers/jq"

	fwmanager "github.com/opendatahub-io/odh-platform-utilities/framework/manager"

	infrav1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	operator "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/cmd/operator"
	inventorycontroller "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/inventory"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/test/support/cluster"
)

// Race scenario data: two successive fact sets for the hosting node's
// GFD product label. Facts X is what the held status patch carries;
// facts Y is what the recreated incarnation must converge to.
const (
	raceFactsXModel = "Tesla-V100"
	raceFactsYModel = "Tesla-T4"
)

// Race phase bounds. Each Eventually phase gets its own generous
// budget; together they must stay inside raceRequestTimeout, because
// the held request survives only that long before the client-side
// timeout aborts it.
const (
	// raceRequestTimeout replaces the harness's 32s per-request bound
	// on the race manager's config: the held patch must outlive the
	// delete/recreate window.
	raceRequestTimeout = 5 * time.Minute

	// racePhaseTimeout bounds one race phase (patch capture, recreation,
	// release observation). The events are controller-driven and take
	// seconds; this is the failure-detection bound, never a sleep.
	racePhaseTimeout = 90 * time.Second

	// raceShutdownTimeout bounds the race manager's graceful stop.
	raceShutdownTimeout = 60 * time.Second

	// raceTokenExpiration bounds the minted ServiceAccount token.
	raceTokenExpiration = int64(3600)
)

// raceEvidence records the incarnation-race outcome for the profile
// evidence document.
type raceEvidence struct {
	Device           string `json:"device"`
	HeldUID          string `json:"heldUid"`
	RecreatedUID     string `json:"recreatedUid"`
	ResponseCode     int    `json:"responseCode"`
	ResponseBody     string `json:"responseBody"`
	TransferredModel string `json:"transferredModel"`
	ConvergedModel   string `json:"convergedModel"`
}

// raceResult carries the last race outcome to the evidence writer.
var raceResult *raceEvidence

// lastRaceEvidence returns the recorded race outcome, nil when the
// profile ran no race.
func lastRaceEvidence() *raceEvidence {
	return raceResult
}

// verifyIncarnationRace runs the deterministic incarnation-convergence
// race on the first node:
//
//  1. the chart's controller Deployment is scaled to zero and its pod
//     is observed gone, so the only service writer is the test-process
//     manager started below;
//  2. facts X (raceFactsXModel) is applied to the node while no
//     controller runs, so no observation has consumed it yet;
//  3. the production controller registration (operator.RegisterControllers)
//     runs in the test process on a manager built from the chart's own
//     ServiceAccount token, with the transport wrapped by a request
//     hold; the harness admin kubeconfig is used only for the scenario
//     steps around the manager, never by it;
//  4. the first status patch whose body carries facts X is captured and
//     held before it reaches the API server — the device controller's
//     worker is blocked mid-apply, which is the whole point;
//  5. the held patch's target device (parsed from the request URL, so
//     the scenario does not depend on reconcile ordering) is deleted by
//     the admin and facts Y is applied to the node; the inventory
//     controller — a separate worker, not blocked — observes the delete
//     event and recreates the device under the same name with a fresh
//     uid;
//  6. the hold is released: the real API server accepts the stale
//     status apply — a CR /status server-side apply carries no
//     optimistic-concurrency precondition and the payload's uid is not
//     a guard (the accepted status-write incarnation semantics, design
//     §6.3 amendment) — so the stale facts X land on the recreated
//     incarnation B, whose creation reconcile then overwrites them with
//     its own facts-Y observation within one pass: convergence, not
//     atomicity. The steady state never describes the stale facts.
func verifyIncarnationRace(
	t *testing.T,
	ctx context.Context,
	inst cluster.Instance,
	cli client.Client,
	nodes *corev1.NodeList,
) {
	t.Helper()
	g := NewWithT(t)

	node := &nodes.Items[0]

	// Step 1: stop the in-cluster controller; the race manager that
	// replaces it runs the very same registration.
	scaleControllerDeployment(t, ctx, cli, 0)
	waitForControllerPodsStopped(t, ctx, cli)
	t.Logf("race: in-cluster controller stopped (deployment %s scaled to 0)", controllerDeployment)

	// Step 2: facts X is unobserved scenario data until the race manager
	// starts. No fixed sleep is needed: the hold itself is the
	// synchronization point that proves the manager observed it.
	g.Expect(updateNodeLabels(ctx, cli, node.Name,
		map[string]string{gfdLabelProduct: raceFactsXModel}, nil)).To(Succeed(),
		"applying the race facts X to node %s", node.Name)

	// Step 3: the chart ServiceAccount's token and the hold-wrapped
	// manager config.
	token := createServiceAccountToken(t, ctx, inst)
	hold := newRequestHold([]byte(raceFactsXModel))
	raceCfg := raceRestConfig(inst.Config(), token, hold)

	raceCtx, cancel := context.WithCancel(ctx)
	managerDone := make(chan error, 1)

	ctrlManager, err := ctrl.NewManager(raceCfg, raceManagerOptions())
	g.Expect(err).NotTo(HaveOccurred(), "building the race manager")
	manager := fwmanager.New(ctrlManager)
	g.Expect(operator.RegisterControllers(manager)).To(Succeed(),
		"the production controller registration must run on the race manager")
	go func() { managerDone <- manager.Start(raceCtx) }()

	// Everything the manager owns is undone in one deterministic order:
	// release any held request so the worker can finish, cancel the
	// manager context, then wait for Start to return.
	defer func() {
		hold.releaseRequest()
		cancel()
		select {
		case runErr := <-managerDone:
			if runErr != nil && !errors.Is(runErr, context.Canceled) {
				t.Errorf("the race manager exited with an error: %v", runErr)
			}
		case <-time.After(raceShutdownTimeout):
			t.Errorf("the race manager did not stop within %s", raceShutdownTimeout)
		}
	}()

	// Step 4: the first facts-X status patch is captured and held.
	var held *heldRequest
	Eventually(ctx, func(g Gomega) {
		held = hold.captured()
		g.Expect(held).NotTo(BeNil())
	}).WithTimeout(racePhaseTimeout).WithPolling(discoveryPollInterval).Should(Succeed(),
		"the race manager must issue the facts-X status patch (the manager start, cache sync and first reconcile are covered by this wait)")

	g.Expect(held.method).To(Equal(http.MethodPatch), "the held request must be the status apply")
	g.Expect(held.path).To(ContainSubstring("/status"), "the held request must target the status subresource")

	// The held patch's device is parsed from the request URL: whichever
	// of the node's devices reconciled first is the race's target A.
	deviceA := &infrav1alpha1.HardwareDevice{}
	g.Expect(cli.Get(ctx, client.ObjectKey{Name: held.device}, deviceA)).To(Succeed(),
		"the held patch's target device must exist while the patch is held")
	g.Expect(deviceA.Spec.Host.Name).To(Equal(node.Name),
		"the held patch must target a device of the facts-X node")

	// The held body is the SSA apply configuration the device controller
	// computed against the old incarnation, asserted field-scoped through
	// jq: it carries the incarnation's uid and the facts-X observation,
	// not merely the marker substring the hold captured on.
	g.Expect(held.body).To(And(
		jq.Matchf(`.metadata.uid == %q`, string(deviceA.UID)),
		jq.Matchf(`.status.accelerator.model == %q`, raceFactsXModel),
	))
	heldUID := deviceA.UID
	t.Logf("race: holding %s %s for device %s (uid %s)",
		held.method, held.path, held.device, heldUID)

	// Step 5: delete the target and move the node to facts Y. The
	// inventory controller (a separate worker) observes the delete
	// event, rescans and recreates the device under the same name.
	g.Expect(cli.Delete(ctx, deviceA)).To(Succeed(),
		"the admin must be able to delete the held patch's target device")
	g.Expect(updateNodeLabels(ctx, cli, node.Name,
		map[string]string{gfdLabelProduct: raceFactsYModel}, nil)).To(Succeed(),
		"applying the race facts Y to node %s", node.Name)

	deviceB := &infrav1alpha1.HardwareDevice{}
	Eventually(ctx, func(g Gomega) {
		g.Expect(cli.Get(ctx, client.ObjectKey{Name: held.device}, deviceB)).To(Succeed())
		g.Expect(deviceB.UID).NotTo(Equal(heldUID),
			"the device must be recreated as a fresh incarnation under the same name")
	}).WithTimeout(racePhaseTimeout).WithPolling(discoveryPollInterval).Should(Succeed(),
		"the inventory controller must recreate the deleted device")

	g.Expect(deviceB.Spec).To(Equal(deviceA.Spec),
		"the recreated device keeps the discovered identity, host and locator spec")
	g.Expect(deviceB.OwnerReferences).NotTo(BeEmpty(),
		"the recreated device stays owned by the singleton")

	// Step 6: release the hold and observe the API server's verdict on
	// the stale apply — the accepted status-write incarnation semantics.
	hold.releaseRequest()

	var responseCode int
	var responseBody string
	Eventually(ctx, func(g Gomega) {
		code, body, responded := hold.result()
		g.Expect(responded).To(BeTrue(), "the released patch must complete")
		responseCode, responseBody = code, body
	}).WithTimeout(racePhaseTimeout).WithPolling(discoveryPollInterval).Should(Succeed(),
		"the released patch must reach the API server and return")

	g.Expect(responseCode).To(Equal(http.StatusOK),
		"the stale-incarnation status apply is accepted (eventual consistency, not atomicity)")
	// The accepted transfer, witnessed by the API server's own response:
	// the persisted object is incarnation B carrying the stale facts X.
	g.Expect(responseBody).To(And(
		jq.Matchf(`.metadata.uid == %q`, string(deviceB.UID)),
		jq.Matchf(`.status.accelerator.model == %q`, raceFactsXModel),
	))
	t.Logf("race: released patch accepted (%d): incarnation B transiently carries the stale facts X",
		responseCode)

	// B's creation reconcile overwrites the transferred status with its
	// own observation within one pass — convergence, not atomicity. The
	// steady state describes the last evaluation of the current
	// incarnation: facts Y, the node's current labels. The memory label
	// is untouched by the race, so the memory fact keeps its frozen
	// value.
	factsY := expectedDeviceFacts{
		model:     raceFactsYModel,
		memory:    ptr.To(resource.MustParse(strconv.Itoa(frozenGpuMemoryMi) + "Mi")),
		plane:     planeClassic,
		sourceRef: sourceKindClassic + "/" + node.Name,
	}
	Eventually(ctx, func(g Gomega) {
		dev := &infrav1alpha1.HardwareDevice{}
		g.Expect(cli.Get(ctx, client.ObjectKey{Name: held.device}, dev)).To(Succeed())
		verifyDeviceStatus(g, dev, factsY)
	}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed(),
		"the recreated device must converge to facts Y through a fresh observation")

	// The sibling device on the same node and every device elsewhere
	// stay healthy: the transient transfer is incarnation-scoped, never
	// a general write failure of the manager.
	verifyRaceAftermath(t, ctx, cli, node.Name, factsY)

	raceResult = &raceEvidence{
		Device:           held.device,
		HeldUID:          string(heldUID),
		RecreatedUID:     string(deviceB.UID),
		ResponseCode:     responseCode,
		ResponseBody:     oneLine(responseBody),
		TransferredModel: raceFactsXModel,
		ConvergedModel:   raceFactsYModel,
	}
	t.Logf("race: incarnation convergence proven on device %s (%s -> %s)",
		held.device, heldUID, deviceB.UID)
}

// verifyRaceAftermath asserts the cluster settles healthy after the
// race: every device is Ready with a successful observation, the race
// node's devices carry facts Y, and the untouched nodes keep the frozen
// facts.
func verifyRaceAftermath(
	t *testing.T,
	ctx context.Context,
	cli client.Client,
	racedNode string,
	factsY expectedDeviceFacts,
) {
	t.Helper()

	Eventually(ctx, func(g Gomega) {
		devices := &infrav1alpha1.HardwareDeviceList{}
		g.Expect(cli.List(ctx, devices)).To(Succeed())
		g.Expect(devices.Items).NotTo(BeEmpty())

		for i := range devices.Items {
			dev := &devices.Items[i]
			if dev.Spec.Host.Name == racedNode {
				// Both the raced device and its sibling converge to the
				// observed facts Y whatever their reconcile ordering was.
				verifyDeviceStatus(g, dev, factsY)
				continue
			}
			// The frozen facts of untouched nodes survive the race.
			frozen := expectedDeviceFacts{
				model:     frozenGpuProduct,
				memory:    ptr.To(resource.MustParse(strconv.Itoa(frozenGpuMemoryMi) + "Mi")),
				plane:     planeClassic,
				sourceRef: sourceKindClassic + "/" + dev.Spec.Host.Name,
			}
			verifyDeviceStatus(g, dev, frozen)
		}
	}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed(),
		"every device must be healthy after the incarnation race")
}

// scaleControllerDeployment sets the chart-rendered manager Deployment's
// replica count.
func scaleControllerDeployment(t *testing.T, ctx context.Context, cli client.Client, replicas int32) {
	t.Helper()
	g := NewWithT(t)

	dep := &appsv1.Deployment{}
	g.Expect(cli.Get(ctx, client.ObjectKey{
		Name:      controllerDeployment,
		Namespace: chartNamespace,
	}, dep)).To(Succeed(), "the chart-rendered controller Deployment must exist")

	dep.Spec.Replicas = ptr.To(replicas)
	g.Expect(cli.Update(ctx, dep)).To(Succeed(),
		"scaling the controller Deployment to %d replicas", replicas)
}

// waitForControllerPodsStopped observes the controller namespace having
// no running pod: the in-cluster service writer is gone.
func waitForControllerPodsStopped(t *testing.T, ctx context.Context, cli client.Client) {
	t.Helper()

	Eventually(ctx, func(g Gomega) {
		pods := &corev1.PodList{}
		g.Expect(cli.List(ctx, pods, client.InNamespace(chartNamespace))).To(Succeed())
		running := 0
		for i := range pods.Items {
			phase := pods.Items[i].Status.Phase
			if phase == corev1.PodPending || phase == corev1.PodRunning {
				running++
			}
		}
		g.Expect(running).To(Equal(0), "no controller pod may stay pending or running")
	}).WithTimeout(discoveryEvidenceTimeout).WithPolling(discoveryPollInterval).Should(Succeed(),
		"the in-cluster controller pod must terminate after the scale-down")
}

// createServiceAccountToken mints a bound token for the chart's
// controller ServiceAccount through the TokenRequest API. The admin
// kubeconfig performs this one scenario step; the manager itself only
// ever holds the ServiceAccount token.
func createServiceAccountToken(t *testing.T, ctx context.Context, inst cluster.Instance) string {
	t.Helper()
	g := NewWithT(t)

	clientset, err := kubernetes.NewForConfig(inst.Config())
	g.Expect(err).NotTo(HaveOccurred())

	request, err := clientset.CoreV1().ServiceAccounts(chartNamespace).CreateToken(ctx,
		controllerServiceAccount,
		&authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{
				ExpirationSeconds: ptr.To(raceTokenExpiration),
			},
		},
		metav1.CreateOptions{},
	)
	g.Expect(err).NotTo(HaveOccurred(),
		"the admin must be able to mint a token for the chart's ServiceAccount")
	g.Expect(request.Status.Token).NotTo(BeEmpty(),
		"the TokenRequest must return a bearer token")
	return request.Status.Token
}

// raceRestConfig derives the race manager's config from the harness
// kubeconfig: the client-certificate admin identity is replaced by the
// chart ServiceAccount's bearer token, the per-request timeout is
// raised so the held patch survives the race window, and every
// transport is wrapped by the request hold.
func raceRestConfig(base *rest.Config, token string, hold *requestHold) *rest.Config {
	cfg := rest.CopyConfig(base)

	cfg.CertFile = ""
	cfg.KeyFile = ""
	cfg.CertData = nil
	cfg.KeyData = nil
	cfg.BearerToken = token
	cfg.BearerTokenFile = ""

	cfg.Timeout = raceRequestTimeout
	cfg.WrapTransport = hold.wrap

	return cfg
}

// raceManagerOptions mirrors the production manager construction
// (cmd/operator runtime and registration): the same scheme (client-go
// plus the infrastructure APIs), the same inventory scan serialization,
// the production controller registration. Only the process-local
// concerns differ — no leader election (a single short-lived manager),
// no metrics endpoint, no webhook server, no probes.
func raceManagerOptions() ctrl.Options {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(infrav1alpha1.AddToScheme(scheme))

	return ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		WebhookServer:          webhook.NewServer(webhook.Options{Port: -1}),
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Controller:             inventorycontroller.ManagerControllerOptions(ctrlconfig.Controller{}),
	}
}

// heldRequest is one captured request: what was sent, and — after the
// release — what the API server answered.
type heldRequest struct {
	method string
	path   string
	device string
	body   []byte

	responded     bool
	responseCode  int
	responseBody  string
	responseError error
}

// requestHold captures the first PATCH whose body carries the marker
// and blocks it inside the transport until released. The hold state is
// shared by every transport the manager builds from the config
// (client, cache and API reader each wrap a fresh base transport), so
// exactly one request is ever held no matter how many transports exist.
type requestHold struct {
	marker  []byte
	release chan struct{}
	once    sync.Once

	mu   sync.Mutex
	held *heldRequest
}

// newRequestHold builds a hold for the given body marker.
func newRequestHold(marker []byte) *requestHold {
	return &requestHold{
		marker:  marker,
		release: make(chan struct{}),
	}
}

// wrap installs the hold around one base transport.
func (h *requestHold) wrap(base http.RoundTripper) http.RoundTripper {
	return &holdingRoundTripper{hold: h, base: base}
}

// captured returns the held request, nil while nothing is held.
func (h *requestHold) captured() *heldRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held
}

// result returns the held request's response once it completed.
func (h *requestHold) result() (int, string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.held == nil || !h.held.responded {
		return 0, "", false
	}
	return h.held.responseCode, h.held.responseBody, true
}

// releaseRequest lets the held request proceed; safe to call any number
// of times.
func (h *requestHold) releaseRequest() {
	h.once.Do(func() { close(h.release) })
}

// capture records the first marker-matching request and reports whether
// this call took the hold.
func (h *requestHold) capture(req *http.Request, body []byte) *heldRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.held != nil {
		return nil
	}
	h.held = &heldRequest{
		method: req.Method,
		path:   req.URL.Path,
		device: deviceNameFromPath(req.URL.Path),
		body:   body,
	}
	return h.held
}

// record stores the released request's outcome and restores the
// response body for the waiting client.
func (h *requestHold) record(held *heldRequest, resp *http.Response) {
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		held.responseError = fmt.Errorf("reading the released response: %w", err)
		held.responded = true
		return
	}
	held.responseCode = resp.StatusCode
	held.responseBody = string(body)
	held.responded = true
	resp.Body = io.NopCloser(bytes.NewReader(body))
}

// holdingRoundTripper is one transport wrapped by the shared hold.
type holdingRoundTripper struct {
	hold *requestHold
	base http.RoundTripper
}

// RoundTrip forwards every request unchanged, except the first
// PATCH whose body carries the marker: that one is captured and blocks
// inside this call until the test releases it (or the request's context
// dies, so a shutdown manager can never deadlock the worker).
func (rt *holdingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPatch || req.Body == nil {
		return rt.base.RoundTrip(req)
	}

	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("race: reading the patch body: %w", err)
	}

	held := rt.hold.capture(req, body)
	if held == nil {
		req.Body = replayBody(body)
		return rt.base.RoundTrip(req)
	}

	select {
	case <-rt.hold.release:
	case <-req.Context().Done():
		return nil, fmt.Errorf("race: the held %s %s aborted before release: %w",
			req.Method, req.URL.Path, req.Context().Err())
	}

	req.Body = replayBody(body)
	resp, err := rt.base.RoundTrip(req)
	if resp != nil {
		rt.hold.record(held, resp)
	}
	return resp, err
}

// replayBody rebuilds a consumed request body so the forwarded request
// carries the exact original bytes.
func replayBody(body []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(body))
}

// deviceNameFromPath extracts the object name from a cluster-scoped
// resource path, tolerating the /status subresource suffix.
func deviceNameFromPath(path string) string {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) == 0 {
		return ""
	}
	if segments[len(segments)-1] == "status" {
		segments = segments[:len(segments)-1]
	}
	if len(segments) == 0 {
		return ""
	}
	return segments[len(segments)-1]
}

// oneLine collapses an API server error body to a single line for logs.
func oneLine(body string) string {
	return strings.ReplaceAll(strings.TrimSpace(body), "\n", " ")
}
