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

package inventory_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrlmanager "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	ctrlrecorder "sigs.k8s.io/controller-runtime/pkg/recorder"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/inventory"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/classic"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/dra"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	fwconditions "github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	fwreconciler "github.com/opendatahub-io/odh-platform-utilities/framework/controller/reconciler"
)

// Well-known test cluster identities.
const (
	singletonUID = "singleton-uid"
	nodeAName    = "gpu-node-a"
	nodeAUID     = "uid-a"
	nodeBName    = "gpu-node-b"
	nodeBUID     = "uid-b"
	plainName    = "plain-node"
	plainUID     = "uid-plain"

	deviceClassLabel = "hardwarediscovery.platform.opendatahub.io/device-class"
	nodeLabel        = "hardwarediscovery.platform.opendatahub.io/node"

	// Well-known expected values, independent of production constants.
	sourceIDDRA     = "dra"
	sourceIDClassic = "classic"
	classTest       = "accelerator"
	outcomeSuccess  = "Success"
	outcomeUnknown  = "Unknown"
	modelTesla      = "Tesla-K80"
	selectorKeyPool = "pool"
	deviceIDGPU     = "gpu-abc123"

	// Whole-GPU advertisement contract of the DRA source fixtures.
	nvidiaDriver = "gpu.nvidia.com"
	attrType     = "gpu.nvidia.com/type"
	attrUUID     = "gpu.nvidia.com/uuid"
	attrProduct  = "gpu.nvidia.com/productName"
	typeGPU      = "gpu"
	gpuDeviceRef = "gpu-0"
	memoryBytes  = int64(11441) * 1024 * 1024
)

// testScheme builds a scheme with the core, DRA and infrastructure types.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(resourcev1.AddToScheme(scheme))
	utilruntime.Must(infrastructurev1alpha1.AddToScheme(scheme))
	return scheme
}

// fakeSource is a scripted discovery source: per-node claims and
// enumerations. It never observes; the inventory controller does not
// call Observe.
type fakeSource struct {
	id     string
	plane  discovery.Plane
	claims map[string]bool
	enums  map[string]discovery.Enumeration
}

func (s *fakeSource) ID() string             { return s.id }
func (s *fakeSource) Plane() discovery.Plane { return s.plane }

func (s *fakeSource) ClaimsNode(_ context.Context, node discovery.NodeInfo) bool {
	return s.claims[node.Name]
}

func (s *fakeSource) Enumerate(_ context.Context, nodes []discovery.NodeInfo) (discovery.Enumeration, error) {
	if len(nodes) != 1 {
		return discovery.Enumeration{}, fmt.Errorf("fake source %q enumerates one node at a time, got %d", s.id, len(nodes))
	}
	if enum, ok := s.enums[nodes[0].Name]; ok {
		return enum, nil
	}
	return discovery.Enumeration{Outcome: discovery.OutcomeSuccess}, nil
}

func (s *fakeSource) Observe(context.Context, discovery.Identity) (discovery.Observation, error) {
	return discovery.Observation{}, fmt.Errorf("fake source %q cannot observe", s.id)
}

// trackingClient records every primary-resource write and every
// subresource write; write instrumentation, not resourceVersion
// inference, proves no-op behavior.
type trackingClient struct {
	client.Client
	creates       []string
	updates       []string
	deletes       []string
	patches       []string
	applies       []string
	deleteAllOfs  []string
	statusUpdates []string
	listNodesErr  error
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

func (c *trackingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patches = append(c.patches, objectRef(obj))
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *trackingClient) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	c.applies = append(c.applies, applyRef(obj))
	return c.Client.Apply(ctx, obj, opts...)
}

func (c *trackingClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	c.deleteAllOfs = append(c.deleteAllOfs, objectRef(obj))
	return c.Client.DeleteAllOf(ctx, obj, opts...)
}

func (c *trackingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	// The framework client converts typed lists to unstructured before
	// they reach this client, so the node list is detected by GVK, not
	// by Go type.
	if c.listNodesErr != nil {
		gvk := list.GetObjectKind().GroupVersionKind()
		if gvk.Group == "" && gvk.Version == "v1" && gvk.Kind == "NodeList" {
			return c.listNodesErr
		}
	}
	return c.Client.List(ctx, list, opts...)
}

func (c *trackingClient) Status() client.SubResourceWriter {
	return &trackingStatusWriter{delegate: c.Client.Status(), recorded: &c.statusUpdates}
}

// reset clears the recorded writes between reconciliations.
func (c *trackingClient) reset() {
	c.creates = nil
	c.updates = nil
	c.deletes = nil
	c.patches = nil
	c.applies = nil
	c.deleteAllOfs = nil
	c.statusUpdates = nil
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
	*w.recorded = append(*w.recorded, applyRef(obj))
	return w.delegate.Apply(ctx, obj, opts...)
}

func objectRef(obj client.Object) string {
	return fmt.Sprintf("%T/%s", obj, obj.GetName())
}

// applyRef renders an apply configuration as "Kind/name" for write
// assertions; the concrete apply-configuration types are opaque, so the
// reference is recovered from the applied JSON.
func applyRef(obj runtime.ApplyConfiguration) string {
	data, err := json.Marshal(obj)
	if err != nil {
		return fmt.Sprintf("%T", obj)
	}
	var ref struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(data, &ref); err != nil {
		return fmt.Sprintf("%T", obj)
	}
	return ref.Kind + "/" + ref.Metadata.Name
}

// fakeClock is the injected collector clock.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// managerStub satisfies the framework reconciler's manager needs offline:
// an instrumented client, the fixture scheme, a recording event recorder
// and a dummy REST config — the discovery and dynamic clients built from
// it are never dialed in tests.
type managerStub struct {
	ctrlmanager.Manager

	client   client.Client
	scheme   *runtime.Scheme
	recorder ctrlrecorder.EventRecorder
	config   *rest.Config
}

func (m *managerStub) GetClient() client.Client { return m.client }

func (m *managerStub) GetScheme() *runtime.Scheme { return m.scheme }

func (m *managerStub) GetEventRecorder(string) ctrlrecorder.EventRecorder { return m.recorder }

func (m *managerStub) GetConfig() *rest.Config { return m.config }

// fixture wires a framework reconciler against a fake cluster, scripted
// sources (or the production source factory over a reader of the same
// cluster), a fake clock and a recording event recorder. A fresh
// reconciler is built for every reconciliation, so controller restarts
// are exercised for free.
type fixture struct {
	t             *testing.T
	g             *WithT
	inner         client.WithWatch
	client        *trackingClient
	clock         *fakeClock
	scheme        *runtime.Scheme
	recorder      *events.FakeRecorder
	sources       []discovery.Source
	sourceFactory inventory.SourceFactory
}

// resourceSliceNodeName extracts the fake-client index value for the
// spec.nodeName field selector the DRA source lists with.
func resourceSliceNodeName(obj client.Object) []string {
	slice, ok := obj.(*resourcev1.ResourceSlice)
	if !ok || slice.Spec.NodeName == nil {
		return nil
	}
	return []string{*slice.Spec.NodeName}
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
		WithIndex(&resourcev1.ResourceSlice{}, resourcev1.ResourceSliceSelectorNodeName, resourceSliceNodeName).
		WithObjects(objs...).
		Build()
	return &fixture{
		t:        t,
		g:        g,
		inner:    inner,
		client:   &trackingClient{Client: inner},
		clock:    &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		scheme:   scheme,
		recorder: &events.FakeRecorder{Events: make(chan string, 128)},
	}
}

// useRealSources switches the fixture to the production source factory
// over a reader of the fixture's cluster; the reader can be an
// interceptor over the inner client to script read failures.
func (f *fixture) useRealSources(reader client.Reader) {
	f.sourceFactory = inventory.NewSourceFactory(reader)
}

func (f *fixture) reconciler() *fwreconciler.Reconciler {
	f.t.Helper()
	factory := f.sourceFactory
	if factory == nil {
		factory = func(infrastructurev1alpha1.DiscoverySources) (discovery.Sources, error) {
			return discovery.NewSources(f.sources...)
		}
	}
	r, err := (&inventory.Controller{
		Sources: factory,
		Clock:   f.clock.Now,
	}).NewReconciler(&managerStub{
		client:   f.client,
		scheme:   f.scheme,
		recorder: f.recorder,
		config:   &rest.Config{Host: "https://127.0.0.1"},
	})
	if err != nil {
		f.t.Fatalf("fixture: building inventory reconciler: %v", err)
	}
	return r
}

// mustReconcile runs one scan and fails the test on transient errors.
func (f *fixture) mustReconcile() ctrl.Result {
	f.t.Helper()
	result, err := f.reconciler().Reconcile(context.Background(),
		reconcile.Request{NamespacedName: types.NamespacedName{Name: inventory.SingletonName}})
	f.g.ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return result
}

func (f *fixture) getSingleton() *infrastructurev1alpha1.HardwareDiscovery {
	f.t.Helper()
	single := &infrastructurev1alpha1.HardwareDiscovery{}
	f.g.ExpectWithOffset(1, f.client.Get(context.Background(),
		client.ObjectKey{Name: inventory.SingletonName}, single)).To(Succeed())
	return single
}

func (f *fixture) getDevice(name string) *infrastructurev1alpha1.HardwareDevice {
	f.t.Helper()
	device := &infrastructurev1alpha1.HardwareDevice{}
	f.g.ExpectWithOffset(1, f.client.Get(context.Background(),
		client.ObjectKey{Name: name}, device)).To(Succeed())
	return device
}

func (f *fixture) deviceMissing(name string) {
	f.t.Helper()
	err := f.client.Get(context.Background(),
		client.ObjectKey{Name: name}, &infrastructurev1alpha1.HardwareDevice{})
	f.g.ExpectWithOffset(1, apierrors.IsNotFound(err)).To(BeTrue(),
		"expected hardware device %q to be missing, got %v", name, err)
}

func (f *fixture) listDevices() []infrastructurev1alpha1.HardwareDevice {
	f.t.Helper()
	list := &infrastructurev1alpha1.HardwareDeviceList{}
	f.g.ExpectWithOffset(1, f.client.List(context.Background(), list)).To(Succeed())
	return list.Items
}

// condition returns one singleton condition and asserts its presence.
func condition(g *WithT, single *infrastructurev1alpha1.HardwareDiscovery, conditionType string) fwapi.Condition {
	found := fwconditions.FindStatusCondition(single, conditionType)
	g.Expect(found).NotTo(BeNil(), "condition %q must exist", conditionType)
	return *found
}

// testNode builds a Node with the given identity.
func testNode(name, uid string, nodeLabels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			UID:    types.UID(uid),
			Labels: nodeLabels,
		},
	}
}

// testSingleton builds the singleton with the accelerator class and the
// default all-node selection.
func testSingleton() *infrastructurev1alpha1.HardwareDiscovery {
	return &infrastructurev1alpha1.HardwareDiscovery{
		ObjectMeta: metav1.ObjectMeta{
			Name:       inventory.SingletonName,
			UID:        singletonUID,
			Generation: 1,
		},
		Spec: infrastructurev1alpha1.HardwareDiscoverySpec{Class: classTest},
	}
}

// disableClassic turns the classic source off in the singleton spec, for
// tests that exercise the DRA plane exclusively.
func disableClassic(single *infrastructurev1alpha1.HardwareDiscovery) {
	enabled := false
	single.Spec.Sources.Classic = &infrastructurev1alpha1.ClassicSourceConfig{Enabled: &enabled}
}

// seedDevice builds an owned device carrying a controller reference to
// the singleton.
func seedDevice(
	t *testing.T,
	scheme *runtime.Scheme,
	single *infrastructurev1alpha1.HardwareDiscovery,
	name string,
	spec infrastructurev1alpha1.HardwareDeviceSpec,
	status infrastructurev1alpha1.HardwareDeviceStatus,
) *infrastructurev1alpha1.HardwareDevice {
	t.Helper()
	device := &infrastructurev1alpha1.HardwareDevice{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID("device-" + name),
			Labels: map[string]string{
				deviceClassLabel: spec.Class,
				nodeLabel:        spec.Host.Name,
			},
		},
		Spec:   spec,
		Status: status,
	}
	utilruntime.Must(controllerutil.SetControllerReference(single, device, scheme))
	return device
}

// draIdentity builds a DRA-plane identity with a UUID id.
func draIdentity(nodeName, nodeUID, id, pool string) discovery.Identity {
	return discovery.Identity{
		ID:       id,
		Kind:     discovery.IdentityKindUUID,
		NodeName: nodeName,
		NodeUID:  types.UID(nodeUID),
		Plane:    discovery.PlaneDRA,
		Locator:  dra.Locator{Driver: nvidiaDriver, Pool: pool, Device: gpuDeviceRef},
	}
}

// classicIdentity builds a classic-plane identity with a nodeIndex id.
func classicIdentity(nodeName, nodeUID string, index int32) discovery.Identity {
	return discovery.Identity{
		ID:       fmt.Sprintf("%s-gpu-%d", nodeName, index),
		Kind:     discovery.IdentityKindNodeIndex,
		NodeName: nodeName,
		NodeUID:  types.UID(nodeUID),
		Plane:    discovery.PlaneClassic,
		Locator:  classic.Locator{Node: nodeName, Index: index},
	}
}

// successEnum builds an authoritative enumeration.
func successEnum(identities ...discovery.Identity) discovery.Enumeration {
	return discovery.Enumeration{Outcome: discovery.OutcomeSuccess, Identities: identities}
}

// unknownEnum builds an unknown enumeration with a classified cause.
func unknownEnum(cause error) discovery.Enumeration {
	return discovery.Enumeration{Outcome: discovery.OutcomeUnknown, Err: cause}
}

// gpuSlice builds a complete single-GPU node-local pool advertisement
// with the whole-GPU attribute and capacity contract.
func gpuSlice(name, nodeName, pool, uuid, model string) *resourcev1.ResourceSlice {
	node := nodeName
	return &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   nvidiaDriver,
			NodeName: &node,
			Pool: resourcev1.ResourcePool{
				Name:               pool,
				Generation:         1,
				ResourceSliceCount: 1,
			},
			Devices: []resourcev1.Device{{
				Name: gpuDeviceRef,
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					attrType:    {StringValue: new(typeGPU)},
					attrUUID:    {StringValue: new(uuid)},
					attrProduct: {StringValue: new(model)},
				},
			}},
		},
	}
}

// incompleteSlicePool builds a two-slice pool with only one slice
// present: an incomplete snapshot that must classify as Unknown.
func incompleteSlicePool(name, nodeName, pool, uuid, model string) *resourcev1.ResourceSlice {
	slice := gpuSlice(name, nodeName, pool, uuid, model)
	slice.Spec.Pool.ResourceSliceCount = 2
	return slice
}

// draDeviceSpec is the expected spec of a DRA identity, computed
// independently of the controller mapping.
func draDeviceSpec(id, nodeName, nodeUID, pool string) infrastructurev1alpha1.HardwareDeviceSpec {
	return infrastructurev1alpha1.HardwareDeviceSpec{
		Class: classTest,
		Identity: infrastructurev1alpha1.DeviceIdentity{
			Kind: "uuid",
			ID:   id,
		},
		Host: infrastructurev1alpha1.DeviceHost{
			Name: nodeName,
			UID:  types.UID(nodeUID),
		},
		Source: infrastructurev1alpha1.DeviceSource{
			Kind: sourceIDDRA,
			DRA: &infrastructurev1alpha1.DRASource{
				Driver: nvidiaDriver,
				Pool:   pool,
				Device: gpuDeviceRef,
			},
		},
	}
}

// classicDeviceSpec is the expected spec of a classic identity.
func classicDeviceSpec(nodeName, nodeUID string, index int32) infrastructurev1alpha1.HardwareDeviceSpec {
	return infrastructurev1alpha1.HardwareDeviceSpec{
		Class: classTest,
		Identity: infrastructurev1alpha1.DeviceIdentity{
			Kind: "nodeIndex",
			ID:   fmt.Sprintf("%s-gpu-%d", nodeName, index),
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
