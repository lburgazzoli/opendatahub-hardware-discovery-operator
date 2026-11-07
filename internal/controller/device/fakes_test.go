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

	// The blank import registers the Kubernetes object converters (the
	// typed client.Object → unstructured mapping) that the jq matchers
	// rely on.
	"github.com/lburgazzoli/gomega-matchers/pkg/matchers/jq"
	_ "github.com/lburgazzoli/gomega-matchers/pkg/matchers/k8s"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/device"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/inventory"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	fwconditions "github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
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

	identityKindUUID      = "uuid"
	identityKindNodeIndex = "nodeIndex"

	selectorKeyPool     = "pool"
	selectorPoolDefault = "default"

	// maxStaleDefault mirrors the design default (10m); test values are
	// computed independently of the production constant.
	maxStaleDefault = 10 * time.Minute
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

// trackingClient records every primary-resource write and every
// subresource write; write instrumentation, not resourceVersion
// inference, proves no-op behavior and spec/status separation.
type trackingClient struct {
	client.Client
	creates       []string
	updates       []string
	deletes       []string
	statusUpdates []string
}

func (c *trackingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.creates = append(c.creates, objectRef(obj))
	return c.Client.Create(ctx, obj, opts...)
}

func (c *trackingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.updates = append(c.updates, objectRef(obj))
	return c.Client.Update(ctx, obj, opts...)
}

func (c *trackingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.deletes = append(c.deletes, objectRef(obj))
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *trackingClient) Status() client.SubResourceWriter {
	return &trackingStatusWriter{delegate: c.Client.Status(), recorded: &c.statusUpdates}
}

// reset clears the recorded writes between reconciliations.
func (c *trackingClient) reset() {
	c.creates = nil
	c.updates = nil
	c.deletes = nil
	c.statusUpdates = nil
}

// expectNoPrimaryWrites proves the device controller never touches the
// primary endpoint.
func (c *trackingClient) expectNoPrimaryWrites(g *WithT) {
	g.Expect(c.creates).To(BeEmpty(), "the device controller never creates objects")
	g.Expect(c.updates).To(BeEmpty(), "the device controller never writes the primary endpoint")
	g.Expect(c.deletes).To(BeEmpty(), "the device controller never deletes objects")
}

type trackingStatusWriter struct {
	delegate client.SubResourceWriter
	recorded *[]string
}

func (w *trackingStatusWriter) Create(
	ctx context.Context,
	obj client.Object,
	subResource client.Object,
	opts ...client.SubResourceCreateOption,
) error {
	*w.recorded = append(*w.recorded, objectRef(obj))
	return w.delegate.Create(ctx, obj, subResource, opts...)
}

func (w *trackingStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	*w.recorded = append(*w.recorded, objectRef(obj))
	return w.delegate.Update(ctx, obj, opts...)
}

func (w *trackingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	*w.recorded = append(*w.recorded, objectRef(obj))
	return w.delegate.Patch(ctx, obj, patch, opts...)
}

func (w *trackingStatusWriter) Apply(
	ctx context.Context,
	obj runtime.ApplyConfiguration,
	opts ...client.SubResourceApplyOption,
) error {
	*w.recorded = append(*w.recorded, fmt.Sprintf("%T", obj))
	return w.delegate.Apply(ctx, obj, opts...)
}

func objectRef(obj client.Object) string {
	return fmt.Sprintf("%T/%s", obj, obj.GetName())
}

// fakeClock is the injected collector clock; the fake source stamps
// observations with it.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// fixture wires a Reconciler against a fake cluster, scripted sources
// and a fake clock. The source factory honors the singleton's source
// enablement exactly like the production factory: a nil arm or a nil
// Enabled defaults to enabled.
type fixture struct {
	t       *testing.T
	g       *WithT
	client  *trackingClient
	clock   *fakeClock
	scheme  *runtime.Scheme
	sources []*fakeSource
}

func newFixture(t *testing.T, objs ...client.Object) *fixture {
	t.Helper()
	g := NewWithT(t)
	scheme := testScheme(t)
	inner := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&infrastructurev1alpha1.HardwareDiscovery{},
			&infrastructurev1alpha1.HardwareDevice{},
		).
		WithObjects(objs...).
		Build()
	return &fixture{
		t:      t,
		g:      g,
		client: &trackingClient{Client: inner},
		clock:  &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		scheme: scheme,
	}
}

// draSource returns the fixture's scripted DRA source, creating it on
// first use.
func (f *fixture) draSource() *fakeSource {
	f.t.Helper()
	for _, src := range f.sources {
		if src.id == sourceIDDRA {
			return src
		}
	}
	src := &fakeSource{id: sourceIDDRA, plane: discovery.PlaneDRA, clock: f.clock}
	f.sources = append(f.sources, src)
	return src
}

func (f *fixture) reconciler() *device.Reconciler {
	return &device.Reconciler{
		Client: f.client,
		Scheme: f.scheme,
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

// request builds the reconcile request of one device.
func request(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
}

// mustReconcile runs one observation and fails the test on transient
// errors.
func (f *fixture) mustReconcile(name string) ctrl.Result {
	f.t.Helper()
	result, err := f.reconciler().Reconcile(context.Background(), request(name))
	f.g.ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return result
}

func (f *fixture) getDevice(name string) *infrastructurev1alpha1.HardwareDevice {
	f.t.Helper()
	found := &infrastructurev1alpha1.HardwareDevice{}
	f.g.ExpectWithOffset(1, f.client.Get(context.Background(),
		client.ObjectKey{Name: name}, found)).To(Succeed())
	return found
}

// observedCondition returns the Observed condition and asserts its
// presence.
func observedCondition(
	g *WithT,
	found *infrastructurev1alpha1.HardwareDevice,
) fwapi.Condition {
	cond := fwconditions.FindStatusCondition(found, device.ConditionTypeObserved)
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

// assertStatusMatches compares a device status against the expected
// document: the persisted facts are asserted as a JSON document through
// jq, and collectedAt compares by instant because the fake client
// re-materializes metav1.Time in the process time zone.
func assertStatusMatches(
	g *WithT,
	found *infrastructurev1alpha1.HardwareDevice,
	want infrastructurev1alpha1.HardwareDeviceStatus,
) {
	g.Expect(found).To(SatisfyAll(
		jq.Match(fmt.Sprintf(`.status.observedGeneration == %d`, want.ObservedGeneration)),
		jq.Match(fmt.Sprintf(`.status.allocationPlane == %q`, want.AllocationPlane)),
		jq.Match(fmt.Sprintf(`.status.sourceRef == %q`, want.SourceRef)),
		jq.Match(acceleratorExpr(want.Accelerator)),
	))
	if want.CollectedAt == nil {
		g.Expect(found.Status.CollectedAt).To(BeNil())
		return
	}
	g.Expect(found.Status.CollectedAt).NotTo(BeNil())
	g.Expect(found.Status.CollectedAt.Time.Equal(want.CollectedAt.Time)).To(BeTrue(),
		"collectedAt must match by instant")
}

// acceleratorExpr renders the jq comparison of the accelerator status
// document: optional facts the source does not publish are absent from
// the document (null), never zero or defaulted.
func acceleratorExpr(want *infrastructurev1alpha1.AcceleratorDeviceStatus) string {
	if want == nil {
		return `.status.accelerator == null`
	}
	expr := fmt.Sprintf(`.status.accelerator.model == %q`, want.Model)
	if want.Vendor != "" {
		expr += fmt.Sprintf(` and .status.accelerator.vendor == %q`, want.Vendor)
	} else {
		expr += ` and .status.accelerator.vendor == null`
	}
	if want.Memory != nil {
		expr += fmt.Sprintf(` and .status.accelerator.memory == %q`, want.Memory.String())
	} else {
		expr += ` and .status.accelerator.memory == null`
	}
	return expr
}

// deviceMissing asserts a device does not exist.
func (f *fixture) deviceMissing(name string) {
	f.t.Helper()
	err := f.client.Get(context.Background(),
		client.ObjectKey{Name: name}, &infrastructurev1alpha1.HardwareDevice{})
	f.g.ExpectWithOffset(1, apierrors.IsNotFound(err)).To(BeTrue(),
		"expected hardware device %q to be missing, got %v", name, err)
}
