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
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/inventory"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/classic"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/dra"
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
)

// testScheme builds a scheme with the core and infrastructure types.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(scheme))
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

func (c *trackingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, isNodes := list.(*corev1.NodeList); isNodes && c.listNodesErr != nil {
		return c.listNodesErr
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
	*w.recorded = append(*w.recorded, fmt.Sprintf("%T", obj))
	return w.delegate.Apply(ctx, obj, opts...)
}

func objectRef(obj client.Object) string {
	return fmt.Sprintf("%T/%s", obj, obj.GetName())
}

// fakeClock is the injected collector clock.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// fixture wires a Reconciler against a fake cluster, scripted sources,
// a fake clock and a recording event recorder. The source factory reads
// the fixture's sources on every scan so tests can change the scripted
// evidence between reconciliations.
type fixture struct {
	t        *testing.T
	g        *WithT
	client   *trackingClient
	clock    *fakeClock
	scheme   *runtime.Scheme
	recorder *events.FakeRecorder
	sources  []discovery.Source
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
		t:        t,
		g:        g,
		client:   &trackingClient{Client: inner},
		clock:    &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		scheme:   scheme,
		recorder: &events.FakeRecorder{Events: make(chan string, 128)},
	}
}

func (f *fixture) reconciler() *inventory.Reconciler {
	return &inventory.Reconciler{
		Client:   f.client,
		Scheme:   f.scheme,
		Recorder: f.recorder,
		Sources: func(infrastructurev1alpha1.DiscoverySources) (discovery.Sources, error) {
			return discovery.NewSources(f.sources...)
		},
		Clock: f.clock.Now,
	}
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
func condition(g *WithT, single *infrastructurev1alpha1.HardwareDiscovery, conditionType string) metav1.Condition {
	found := meta.FindStatusCondition(single.Status.Conditions, conditionType)
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
		Locator:  dra.Locator{Driver: "gpu.nvidia.com", Pool: pool, Device: "gpu-0"},
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

// draDeviceSpec is the expected spec of the well-known DRA identity,
// computed independently of the controller mapping.
func draDeviceSpec(nodeName, nodeUID, pool string) infrastructurev1alpha1.HardwareDeviceSpec {
	return infrastructurev1alpha1.HardwareDeviceSpec{
		Class: classTest,
		Identity: infrastructurev1alpha1.DeviceIdentity{
			Kind: "uuid",
			ID:   deviceIDGPU,
		},
		Host: infrastructurev1alpha1.DeviceHost{
			Name: nodeName,
			UID:  types.UID(nodeUID),
		},
		Source: infrastructurev1alpha1.DeviceSource{
			Kind: sourceIDDRA,
			DRA: &infrastructurev1alpha1.DRASource{
				Driver: "gpu.nvidia.com",
				Pool:   pool,
				Device: "gpu-0",
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
