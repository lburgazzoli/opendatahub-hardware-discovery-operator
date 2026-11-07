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

package device_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrlmanager "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	ctrlrecorder "sigs.k8s.io/controller-runtime/pkg/recorder"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/device"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/inventory"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	fwconditions "github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	fwreconciler "github.com/opendatahub-io/odh-platform-utilities/framework/controller/reconciler"
)

// Well-known test cluster identities and expected values, independent
// of production constants.
const (
	singletonUID = "singleton-uid"
	nodeAName    = "gpu-node-a"
	nodeAUID     = "uid-a"
	nodeBName    = "gpu-node-b"
	nodeBUID     = "uid-b"

	sourceIDDRA     = "dra"
	sourceIDClassic = "classic"
	classTest       = "accelerator"
	driverNVIDIA    = "gpu.nvidia.com"
	poolWorker      = "workerpool"
	deviceIDGPU     = "gpu-abc123"
	deviceNameGPU   = "gpu-abc123"
	draDeviceName   = "gpu-0"
	modelTesla      = "Tesla-K80"
	modelOther      = "Tesla-V100"
	memoryK80       = "12635713536"
	memoryV100      = "8589934592"
	reasonObserved  = "Observed"

	// conditionTypeObserved is the observation condition type, computed
	// independently of the production constant.
	conditionTypeObserved = "Observed"
	conditionTypeReady    = "Ready"
	conditionTypeProv     = "ProvisioningSucceeded"

	identityKindUUID      = "uuid"
	identityKindNodeIndex = "nodeIndex"

	selectorKeyPool     = "pool"
	selectorPoolDefault = "default"

	// maxStaleDefault mirrors the design default (10m); test values are
	// computed independently of the production constant.
	maxStaleDefault = 10 * time.Minute

	phaseReady    = "Ready"
	phaseNotReady = "Not Ready"
)

// fixtureStart is the fake clock's epoch, shared by status seeds.
var fixtureStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// testScheme builds a scheme with the core and infrastructure types.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := infrastructurev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// fakeSource is a scripted discovery source: the device controller only
// ever calls Observe, so ClaimsNode and Enumerate fail loudly if
// anything else reaches for them. Successful observations are stamped
// with the fixture clock — the collector clock lives in the sources —
// and every Observe call is recorded so tests can prove the controller
// resolved exactly the locator-targeted identity in spec.
type fakeSource struct {
	id       string
	plane    discovery.Plane
	clock    *fakeClock
	facts    discovery.Facts
	err      error
	observed []discovery.Identity
}

func (s *fakeSource) ID() string             { return s.id }
func (s *fakeSource) Plane() discovery.Plane { return s.plane }

func (s *fakeSource) ClaimsNode(context.Context, discovery.NodeInfo) bool {
	return false
}

func (s *fakeSource) Enumerate(context.Context, []discovery.NodeInfo) (discovery.Enumeration, error) {
	return discovery.Enumeration{}, fmt.Errorf("fake source %q cannot enumerate", s.id)
}

func (s *fakeSource) Observe(_ context.Context, id discovery.Identity) (discovery.Observation, error) {
	s.observed = append(s.observed, id)
	if s.err != nil {
		return discovery.Observation{}, s.err
	}
	return discovery.Observation{Identity: id, Facts: s.facts, CollectedAt: s.clock.Now()}, nil
}

// fakeClock is the injected collector clock; the fake source stamps
// observations with it.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// writeLog records every controller write, split by endpoint: primary
// Create/Update/Patch/Apply/Delete/DeleteAllOf separately from the
// status subresource apply. Write instrumentation, not resourceVersion
// inference, proves the spec/status separation. Apply payloads are
// captured by identity at record time — the applying client may mutate
// the payload object in place while delegating.
type writeLog struct {
	creates       []string
	updates       []string
	patches       []string
	applies       []string
	deletes       []string
	deleteAllOfs  []string
	statusApplies []string
	appliedUIDs   []types.UID
}

// reset clears the recorded writes between reconciliations.
func (l *writeLog) reset() {
	l.creates = nil
	l.updates = nil
	l.patches = nil
	l.applies = nil
	l.deletes = nil
	l.deleteAllOfs = nil
	l.statusApplies = nil
	l.appliedUIDs = nil
}

// expectNoPrimaryWrites proves the device controller never touches the
// primary endpoint.
func (l *writeLog) expectNoPrimaryWrites(t *testing.T) {
	t.Helper()
	g := NewWithT(t)
	g.Expect(l.creates).To(BeEmpty(), "the device controller never creates objects")
	g.Expect(l.updates).To(BeEmpty(), "the device controller never writes the primary endpoint")
	g.Expect(l.patches).To(BeEmpty(), "the device controller never patches the primary endpoint")
	g.Expect(l.applies).To(BeEmpty(), "the device controller never server-side-applies the primary endpoint")
	g.Expect(l.deletes).To(BeEmpty(), "the device controller never deletes objects")
	g.Expect(l.deleteAllOfs).To(BeEmpty(), "the device controller never bulk-deletes objects")
}

// managerStub satisfies the framework builder's manager needs offline: the
// fake cluster client, the fixture scheme, a recording event recorder, a
// dummy REST config — the discovery and dynamic clients built from it are
// never dialed in tests — plus the registration surface the real
// controller-runtime builder consumes: controller options, a logger, a
// cache and Runnable capture. A fresh stub is created per registration so
// controller restarts never collide on names.
type managerStub struct {
	ctrlmanager.Manager

	client           client.Client
	scheme           *runtime.Scheme
	recorder         ctrlrecorder.EventRecorder
	config           *rest.Config
	cache            cache.Cache
	controllerOption ctrlconfig.Controller
	logger           logr.Logger
	addErr           error
	runnables        []ctrlmanager.Runnable
}

func (m *managerStub) GetClient() client.Client { return m.client }

func (m *managerStub) GetScheme() *runtime.Scheme { return m.scheme }

func (m *managerStub) GetEventRecorder(string) ctrlrecorder.EventRecorder { return m.recorder }

func (m *managerStub) GetConfig() *rest.Config { return m.config }

func (m *managerStub) GetCache() cache.Cache { return m.cache }

func (m *managerStub) GetControllerOptions() ctrlconfig.Controller { return m.controllerOption }

func (m *managerStub) GetLogger() logr.Logger { return m.logger }

func (m *managerStub) Add(r ctrlmanager.Runnable) error {
	if m.addErr != nil {
		return m.addErr
	}
	m.runnables = append(m.runnables, r)
	return nil
}

// fixture wires a framework device controller against a fake cluster,
// scripted sources and a fake clock. The reconciler is built through the
// production ReconcilerFor/Build path for every reconciliation, so
// controller restarts are exercised for free. An interceptor-wrapped
// client is the controller's only view of the cluster: primary writes
// and the status subresource apply are recorded separately, and scripted
// read failures or apply-time races hook the same indirection. Tests
// observe behavior through the objects in the fake cluster and read or
// mutate them through the unwrapped inner client. The fixture holds no
// testing.T or gomega state: helpers take t contextually and derive
// their gomega assertions from it.
type fixture struct {
	inner    client.WithWatch
	client   client.Client
	writes   *writeLog
	clock    *fakeClock
	scheme   *runtime.Scheme
	recorder *events.FakeRecorder
	sources  []*fakeSource

	// hooks are the interceptor indirection: unset hooks delegate to
	// the wrapped client unchanged.
	hooks fixtureHooks
}

// fixtureHooks holds optional scripted behaviors injected between the
// controller and the fake cluster.
type fixtureHooks struct {
	// get fails or redirects one read.
	get func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error
	// subResourceApply runs before a subresource apply is delegated.
	subResourceApply func(ctx context.Context, c client.Client, subResourceName string, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error
}

// newFixture builds the fake cluster with the given objects and wires
// the interceptor instrumentation around it.
func newFixture(t *testing.T, objs ...client.Object) *fixture {
	t.Helper()
	scheme := testScheme(t)
	inner := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&infrastructurev1alpha1.HardwareDiscovery{},
			&infrastructurev1alpha1.HardwareDevice{},
		).
		WithObjects(objs...).
		Build()

	f := &fixture{
		inner:    inner,
		writes:   &writeLog{},
		clock:    &fakeClock{now: fixtureStart},
		scheme:   scheme,
		recorder: &events.FakeRecorder{Events: make(chan string, 128)},
	}
	f.client = interceptor.NewClient(inner, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			f.writes.creates = append(f.writes.creates, objectRef(obj))
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			f.writes.updates = append(f.writes.updates, objectRef(obj))
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			f.writes.patches = append(f.writes.patches, objectRef(obj))
			return c.Patch(ctx, obj, patch, opts...)
		},
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			f.writes.applies = append(f.writes.applies, applyRef(obj))
			return c.Apply(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			f.writes.deletes = append(f.writes.deletes, objectRef(obj))
			return c.Delete(ctx, obj, opts...)
		},
		DeleteAllOf: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteAllOfOption) error {
			f.writes.deleteAllOfs = append(f.writes.deleteAllOfs, objectRef(obj))
			return c.DeleteAllOf(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if f.hooks.get != nil {
				return f.hooks.get(ctx, c, key, obj, opts...)
			}
			return c.Get(ctx, key, obj, opts...)
		},
		SubResourceApply: func(ctx context.Context, c client.Client, subResourceName string, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
			f.recordStatusApply(obj)
			if f.hooks.subResourceApply != nil {
				return f.hooks.subResourceApply(ctx, c, subResourceName, obj, opts...)
			}
			return c.SubResource(subResourceName).Apply(ctx, obj, opts...)
		},
	})
	return f
}

// recordStatusApply records one status subresource apply together with
// the outgoing payload's identity, captured before the applying client
// can mutate the payload in place.
func (f *fixture) recordStatusApply(obj runtime.ApplyConfiguration) {
	f.writes.statusApplies = append(f.writes.statusApplies, applyRef(obj))
	if payload, ok := obj.(client.Object); ok {
		f.writes.appliedUIDs = append(f.writes.appliedUIDs, payload.GetUID())
	}
}

// draSource returns the fixture's scripted DRA source, creating it on
// first use.
func (f *fixture) draSource() *fakeSource {
	for _, src := range f.sources {
		if src.id == sourceIDDRA {
			return src
		}
	}
	src := &fakeSource{id: sourceIDDRA, plane: discovery.PlaneDRA, clock: f.clock}
	f.sources = append(f.sources, src)
	return src
}

// classicSource returns the fixture's scripted classic source, creating
// it on first use.
func (f *fixture) classicSource() *fakeSource {
	for _, src := range f.sources {
		if src.id == sourceIDClassic {
			return src
		}
	}
	src := &fakeSource{id: sourceIDClassic, plane: discovery.PlaneClassic, clock: f.clock}
	f.sources = append(f.sources, src)
	return src
}

// newManager builds a fresh registration-capable manager stub over the
// fixture's shared state. The device controller keeps the manager's
// global concurrency default (only the inventory singleton is
// serialized per kind), so the stub's controller options carry nothing
// discovery-specific. Name validation is skipped because the fixture
// registers a fresh, never-started controller for every reconciliation
// while controller-runtime's name registry is process-global and only
// releases the names of controllers that actually started and stopped.
func (f *fixture) newManager() *managerStub {
	return &managerStub{
		client:   f.client,
		scheme:   f.scheme,
		recorder: f.recorder,
		config:   &rest.Config{Host: "https://127.0.0.1"},
		cache:    &informertest.FakeInformers{Scheme: f.scheme},
		controllerOption: ctrlconfig.Controller{
			SkipNameValidation: new(true),
		},
		logger: logr.Discard(),
	}
}

// controller builds the production controller over the fixture's
// scripted sources. The factory honors the singleton's source enablement
// exactly like the production factory: a nil arm or a nil Enabled
// defaults to enabled.
func (f *fixture) controller() *device.Controller {
	return &device.Controller{
		Sources: func(cfg infrastructurev1alpha1.DiscoverySources) (discovery.Sources, error) {
			enabled := func(on *bool) bool { return on == nil || *on }
			draOn := cfg.DRA == nil || enabled(cfg.DRA.Enabled)
			classicOn := cfg.Classic == nil || enabled(cfg.Classic.Enabled)
			registered := make([]discovery.Source, 0, len(f.sources))
			for _, src := range f.sources {
				switch src.id {
				case sourceIDDRA:
					if !draOn {
						continue
					}
				case sourceIDClassic:
					if !classicOn {
						continue
					}
				}
				registered = append(registered, src)
			}
			return discovery.NewSources(registered...)
		},
	}
}

// reconciler builds the production framework reconciler through
// ReconcilerFor/Build over a fresh manager stub.
func (f *fixture) reconciler(t *testing.T) *fwreconciler.Reconciler {
	t.Helper()
	r, err := f.controller().NewReconciler(context.Background(), f.newManager())
	if err != nil {
		t.Fatalf("fixture: building device reconciler: %v", err)
	}
	return r
}

// request builds the reconcile request of one device.
func request(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
}

// mustReconcile runs one observation and fails the test on transient
// errors.
func (f *fixture) mustReconcile(t *testing.T, name string) ctrl.Result {
	t.Helper()
	result, err := f.reconciler(t).Reconcile(context.Background(), request(name))
	NewWithT(t).ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return result
}

// getDevice reads one device through the unwrapped cluster client.
func (f *fixture) getDevice(t *testing.T, name string) *infrastructurev1alpha1.HardwareDevice {
	t.Helper()
	found := &infrastructurev1alpha1.HardwareDevice{}
	NewWithT(t).ExpectWithOffset(1, f.inner.Get(context.Background(),
		client.ObjectKey{Name: name}, found)).To(Succeed())
	return found
}

// updateDevice persists a device mutation through the unwrapped cluster
// client, bypassing the controller's write instrumentation.
func (f *fixture) updateDevice(t *testing.T, found *infrastructurev1alpha1.HardwareDevice) {
	t.Helper()
	NewWithT(t).ExpectWithOffset(1, f.inner.Update(context.Background(), found)).To(Succeed())
}

// deviceMissing asserts a device does not exist.
func (f *fixture) deviceMissing(t *testing.T, name string) {
	t.Helper()
	err := f.inner.Get(context.Background(),
		client.ObjectKey{Name: name}, &infrastructurev1alpha1.HardwareDevice{})
	NewWithT(t).ExpectWithOffset(1, apierrors.IsNotFound(err)).To(BeTrue(),
		"expected hardware device %q to be missing, got %v", name, err)
}

// observedCondition returns the Observed condition and asserts its
// presence.
func observedCondition(
	g *WithT,
	found *infrastructurev1alpha1.HardwareDevice,
) fwapi.Condition {
	cond := fwconditions.FindStatusCondition(found, conditionTypeObserved)
	g.Expect(cond).NotTo(BeNil(), "the Observed condition must exist")
	return *cond
}

// testNode builds a Node with the given identity.
func testNode(name string, uid string, nodeLabels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			UID:    types.UID(uid),
			Labels: nodeLabels,
		},
	}
}

// testSingleton builds the singleton with the accelerator class, the
// default all-node selection and the default maxStale; mutators adjust
// it for specific scenarios.
func testSingleton(mutators ...func(*infrastructurev1alpha1.HardwareDiscovery)) *infrastructurev1alpha1.HardwareDiscovery {
	single := &infrastructurev1alpha1.HardwareDiscovery{
		ObjectMeta: metav1.ObjectMeta{
			Name:       inventory.SingletonName,
			UID:        singletonUID,
			Generation: 1,
		},
		Spec: infrastructurev1alpha1.HardwareDiscoverySpec{Class: classTest},
	}
	for _, mutate := range mutators {
		mutate(single)
	}
	return single
}

// withSelector narrows the singleton's node selection.
func withSelector(matchLabels map[string]string) func(*infrastructurev1alpha1.HardwareDiscovery) {
	return func(single *infrastructurev1alpha1.HardwareDiscovery) {
		single.Spec.NodeSelector = &metav1.LabelSelector{MatchLabels: matchLabels}
	}
}

// withDRADisabled turns the DRA source off in the singleton config.
func withDRADisabled() func(*infrastructurev1alpha1.HardwareDiscovery) {
	return func(single *infrastructurev1alpha1.HardwareDiscovery) {
		off := false
		single.Spec.Sources.DRA = &infrastructurev1alpha1.DRASourceConfig{Enabled: &off}
	}
}

// seedDevice builds a device owned by the singleton.
func seedDevice(
	t *testing.T,
	scheme *runtime.Scheme,
	single *infrastructurev1alpha1.HardwareDiscovery,
	name string,
	spec infrastructurev1alpha1.HardwareDeviceSpec,
) *infrastructurev1alpha1.HardwareDevice {
	t.Helper()
	found := &infrastructurev1alpha1.HardwareDevice{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			UID:        types.UID("device-" + name),
			Generation: 1,
			Labels:     discovery.DeviceLabels(spec.Class, spec.Host.Name),
		},
		Spec: spec,
	}
	if err := controllerutil.SetControllerReference(single, found, scheme); err != nil {
		t.Fatal(err)
	}
	return found
}

// draDeviceSpec is the spec of the well-known DRA device, computed
// independently of the controller mapping.
func draDeviceSpec(nodeName string, nodeUID string, pool string) infrastructurev1alpha1.HardwareDeviceSpec {
	return infrastructurev1alpha1.HardwareDeviceSpec{
		Class: classTest,
		Identity: infrastructurev1alpha1.DeviceIdentity{
			Kind: identityKindUUID,
			ID:   deviceIDGPU,
		},
		Host: infrastructurev1alpha1.DeviceHost{
			Name: nodeName,
			UID:  types.UID(nodeUID),
		},
		Source: infrastructurev1alpha1.DeviceSource{
			Kind: sourceIDDRA,
			DRA: &infrastructurev1alpha1.DRASource{
				Driver: driverNVIDIA,
				Pool:   pool,
				Device: draDeviceName,
			},
		},
	}
}

// classicDeviceSpec is the spec of a classic device on a node.
func classicDeviceSpec(nodeName string, nodeUID string, index int32, id string) infrastructurev1alpha1.HardwareDeviceSpec {
	return infrastructurev1alpha1.HardwareDeviceSpec{
		Class: classTest,
		Identity: infrastructurev1alpha1.DeviceIdentity{
			Kind: identityKindNodeIndex,
			ID:   id,
		},
		Host: infrastructurev1alpha1.DeviceHost{
			Name: nodeName,
			UID:  types.UID(nodeUID),
		},
		Source: infrastructurev1alpha1.DeviceSource{
			Kind: sourceIDClassic,
			Classic: &infrastructurev1alpha1.ClassicSource{
				Node:  nodeName,
				Index: index,
			},
		},
	}
}

// mustQuantity parses a test quantity; the memory fact is a pointer, nil
// when the source does not publish it.
func mustQuantity(v string) *resource.Quantity {
	q := resource.MustParse(v)
	return &q
}

// acceleratorFacts builds scripted DRA facts.
func acceleratorFacts(model string, memory string) discovery.Facts {
	return discovery.Facts{Accelerator: &discovery.AcceleratorFacts{
		Model:  model,
		Memory: mustQuantity(memory),
	}}
}

// expectedStatus computes the expected device status of a successful
// DRA observation independently of the controller mapping.
func expectedStatus(collectedAt time.Time, model string) infrastructurev1alpha1.HardwareDeviceStatus {
	return infrastructurev1alpha1.HardwareDeviceStatus{
		Status: fwapi.Status{
			ObservedGeneration: 1,
		},
		AllocationPlane: sourceIDDRA,
		CollectedAt:     &metav1.Time{Time: collectedAt},
		SourceRef:       sourceIDDRA + "/" + driverNVIDIA + "/" + poolWorker,
		Accelerator: &infrastructurev1alpha1.AcceleratorDeviceStatus{
			Model:  model,
			Memory: mustQuantity(memoryK80),
		},
	}
}

// objectRef renders a recorded object reference.
func objectRef(obj client.Object) string {
	return fmt.Sprintf("%T/%s", obj, obj.GetName())
}

// applyRef renders a recorded apply-configuration reference.
func applyRef(obj runtime.ApplyConfiguration) string {
	if named, ok := obj.(client.Object); ok {
		return fmt.Sprintf("%T/%s", obj, named.GetName())
	}
	return fmt.Sprintf("%T", obj)
}
