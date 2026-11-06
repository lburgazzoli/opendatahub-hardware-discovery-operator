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

package simulator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	. "github.com/onsi/gomega"
)

// Shared test literals (goconst): the golden values stay hand-written and
// independent of the implementation's helpers.
const (
	testEnabledKey   = "enabled"
	testNodeName     = "node-a"
	testSliceName    = "slice-a"
	testMemoryKey    = "memory"
	testComponentKey = "app.kubernetes.io/component"
)

func newFakeClient(objects ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = resourcev1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func node(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// frozenSharedValues is the §9.1 shared values block, written out as an
// independent golden (not derived from the implementation).
func frozenSharedValues() map[string]any {
	enabled := func(on bool) map[string]any {
		return map[string]any{testEnabledKey: on}
	}
	return map[string]any{
		"gpuOperator":            enabled(false),
		"nvidiaDraDriver":        enabled(false),
		"kwokDraPlugin":          enabled(false),
		"kwokGpuDevicePlugin":    enabled(false),
		"computeDomainDraPlugin": enabled(false),
		"statusUpdater":          enabled(true),
		"topologyServer":         enabled(true),
		"topologyConfigMap":      enabled(true),
		"statusExporter":         enabled(false),
		"migFaker":               enabled(false),
		"runtimeClass":           enabled(false),
		"builtinProfiles":        enabled(false),
		"topology": map[string]any{
			"nodePools": map[string]any{
				DefaultNodePool: map[string]any{
					"gpuProduct": "Tesla-K80",
					"gpuCount":   2,
					"gpuMemory":  11441,
				},
			},
			"nodePoolLabelKey": NodePoolLabelKey,
		},
	}
}

func TestProfileValuesAreFrozen(t *testing.T) {
	g := NewWithT(t)

	classic := frozenSharedValues()
	classic["devicePlugin"] = map[string]any{testEnabledKey: true}
	classic["draPlugin"] = map[string]any{testEnabledKey: false}

	dra := frozenSharedValues()
	dra["devicePlugin"] = map[string]any{testEnabledKey: false}
	dra["draPlugin"] = map[string]any{testEnabledKey: true}

	g.Expect(ProfileClassic.Values()).To(Equal(classic), "classic values must match design doc §9.1 exactly")
	g.Expect(ProfileDRA.Values()).To(Equal(dra), "DRA values must match design doc §9.1 exactly")

	unknown := Profile("kwok")
	g.Expect(unknown.Values()).To(BeNil())
	g.Expect(unknown.NodeLabels()).To(BeNil())
}

func TestProfileNodeLabels(t *testing.T) {
	g := NewWithT(t)

	g.Expect(ProfileClassic.NodeLabels()).To(Equal(map[string]string{
		"run.ai/simulated-gpu-node-pool":      "default",
		"nvidia.com/gpu.deploy.device-plugin": "true",
	}))
	g.Expect(ProfileDRA.NodeLabels()).To(Equal(map[string]string{
		"run.ai/simulated-gpu-node-pool":       "default",
		"nvidia.com/gpu.deploy.dra-plugin-gpu": "true",
	}))
}

func TestChartRefIsDigestPinned(t *testing.T) {
	g := NewWithT(t)
	g.Expect(ChartRef()).To(Equal(
		"oci://ghcr.io/run-ai/fake-gpu-operator/fake-gpu-operator" +
			"@sha256:e73fc8c0d0a375dfc452eae63b1446cd82e5a302634905acdbd31cbc5a036c11"))
}

func TestLabelNodesAppliesProfileLabels(t *testing.T) {
	g := NewWithT(t)
	cli := newFakeClient(node(testNodeName), node("node-b"))

	g.Expect(LabelNodes(context.Background(), cli, ProfileClassic.NodeLabels())).To(Succeed())

	for _, name := range []string{testNodeName, "node-b"} {
		labeled := &corev1.Node{}
		g.Expect(cli.Get(context.Background(), client.ObjectKey{Name: name}, labeled)).To(Succeed())
		g.Expect(labeled.Labels).To(HaveKeyWithValue("run.ai/simulated-gpu-node-pool", "default"))
		g.Expect(labeled.Labels).To(HaveKeyWithValue("nvidia.com/gpu.deploy.device-plugin", "true"))
	}

	// Idempotent: re-labeling existing labels changes nothing.
	g.Expect(LabelNodes(context.Background(), cli, ProfileClassic.NodeLabels())).To(Succeed())

	g.Expect(LabelNodes(context.Background(), cli, nil)).To(HaveOccurred(), "empty label set is an error")
}

// topologyYAMLFixture is a hand-written FGO-shaped topology.yml. It is a
// fixture, deliberately independent of any production parser.
const topologyYAMLFixture = `gpuMemory: 11441
gpuProduct: Tesla-K80
driverVersion: "560.35.05"
gpus:
- id: GPU-11111111-2222-3333-4444-555555555555
  status: {}
- id: GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee
  status: {}
migStrategy: mixed
`

func topologyConfigMap(data string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: Namespace,
			Name:      TopologyConfigMapName(testNodeName),
		},
		Data: map[string]string{TopologyDataKey: data},
	}
}

func TestGetNodeTopologyParsesMinimalEvidence(t *testing.T) {
	g := NewWithT(t)
	cli := newFakeClient(topologyConfigMap(topologyYAMLFixture))

	topology, err := GetNodeTopology(context.Background(), cli, testNodeName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(topology.GpuProduct).To(Equal("Tesla-K80"))
	g.Expect(topology.GpuMemoryMi).To(Equal(11441))
	g.Expect(topology.GPUIDs).To(Equal([]string{
		"GPU-11111111-2222-3333-4444-555555555555",
		"GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
	}))
}

func TestGetNodeTopologyErrors(t *testing.T) {
	table := []struct {
		name   string
		object client.Object
		want   string
	}{
		{name: "missing configmap", object: nil, want: "not found"},
		{
			name: "missing data key",
			object: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: Namespace,
					Name:      TopologyConfigMapName(testNodeName),
				},
			},
			want: "no topology.yml data key",
		},
		{
			name:   "unparseable data",
			object: topologyConfigMap("\tinvalid: ["),
			want:   "parsing topology.yml",
		},
	}

	for _, entry := range table {
		t.Run(entry.name, func(t *testing.T) {
			g := NewWithT(t)
			cli := newFakeClient()
			if entry.object != nil {
				cli = newFakeClient(entry.object)
			}

			_, err := GetNodeTopology(context.Background(), cli, testNodeName)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(entry.want))
		})
	}
}

func TestWaitForNodeTopologyBounded(t *testing.T) {
	t.Run("present immediately", func(t *testing.T) {
		g := NewWithT(t)
		cli := newFakeClient(topologyConfigMap(topologyYAMLFixture))

		topology, err := WaitForNodeTopology(context.Background(), cli, testNodeName)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(topology.GpuProduct).To(Equal("Tesla-K80"))
	})

	t.Run("absent times out with the context bound", func(t *testing.T) {
		g := NewWithT(t)
		cli := newFakeClient()

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		_, err := WaitForNodeTopology(ctx, cli, testNodeName)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("timed out waiting for simulator evidence"))
	})
}

func resourceSlice(driver, pool string, generation, count int64, devices int) resourcev1.ResourceSlice {
	slice := resourcev1.ResourceSlice{
		Spec: resourcev1.ResourceSliceSpec{
			Driver: driver,
			Pool: resourcev1.ResourcePool{
				Name:               pool,
				Generation:         generation,
				ResourceSliceCount: count,
			},
		},
	}
	for i := range devices {
		slice.Spec.Devices = append(slice.Spec.Devices, resourcev1.Device{
			Name: fmt.Sprintf("gpu-%d", i),
		})
	}
	return slice
}

func TestSnapshotPool(t *testing.T) {
	table := []struct {
		name   string
		slices []resourcev1.ResourceSlice
		driver string
		pool   string
		want   PoolSnapshot
	}{
		{
			name:   "empty cluster is an incomplete snapshot",
			slices: nil,
			want:   PoolSnapshot{Driver: DRADriver, Pool: testNodeName, Complete: false},
		},
		{
			name:   "complete single-slice pool",
			slices: []resourcev1.ResourceSlice{resourceSlice(DRADriver, testNodeName, 1, 1, 2)},
			want:   PoolSnapshot{Driver: DRADriver, Pool: testNodeName, Generation: 1, ResourceSliceCount: 1, Complete: true},
		},
		{
			name: "partial generation is incomplete",
			slices: []resourcev1.ResourceSlice{
				resourceSlice(DRADriver, testNodeName, 2, 2, 2),
			},
			want: PoolSnapshot{Driver: DRADriver, Pool: testNodeName, Generation: 2, ResourceSliceCount: 2, Complete: false},
		},
		{
			name: "highest generation wins and must be complete",
			slices: []resourcev1.ResourceSlice{
				resourceSlice(DRADriver, testNodeName, 1, 1, 2),
				resourceSlice(DRADriver, testNodeName, 2, 2, 2),
				resourceSlice(DRADriver, testNodeName, 2, 2, 1),
			},
			want: PoolSnapshot{Driver: DRADriver, Pool: testNodeName, Generation: 2, ResourceSliceCount: 2, Complete: true},
		},
		{
			name: "foreign driver and pool are ignored",
			slices: []resourcev1.ResourceSlice{
				resourceSlice("other.example.com", testNodeName, 5, 1, 2),
				resourceSlice(DRADriver, "node-b", 5, 1, 2),
			},
			want: PoolSnapshot{Driver: DRADriver, Pool: testNodeName, Generation: 0, ResourceSliceCount: 0, Complete: false},
		},
	}

	for _, entry := range table {
		t.Run(entry.name, func(t *testing.T) {
			g := NewWithT(t)
			snapshot := SnapshotPool(entry.slices, DRADriver, testNodeName)

			g.Expect(snapshot.Driver).To(Equal(entry.want.Driver))
			g.Expect(snapshot.Pool).To(Equal(entry.want.Pool))
			g.Expect(snapshot.Generation).To(Equal(entry.want.Generation))
			g.Expect(snapshot.ResourceSliceCount).To(Equal(entry.want.ResourceSliceCount))
			g.Expect(snapshot.Complete).To(Equal(entry.want.Complete))
			g.Expect(len(snapshot.Slices)).To(BeNumerically(">=", 0))
		})
	}
}

func TestWaitForResourceSlicesBounded(t *testing.T) {
	t.Run("complete pool returns", func(t *testing.T) {
		g := NewWithT(t)
		cli := newFakeClient(&resourcev1.ResourceSlice{
			ObjectMeta: metav1.ObjectMeta{Name: testSliceName},
			Spec: resourcev1.ResourceSliceSpec{
				Driver: DRADriver,
				Pool:   resourcev1.ResourcePool{Name: testNodeName, Generation: 1, ResourceSliceCount: 1},
			},
		})

		snapshot, err := WaitForResourceSlices(context.Background(), cli, DRADriver, testNodeName)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(snapshot.Complete).To(BeTrue())
		g.Expect(snapshot.Slices).To(HaveLen(1))
	})

	t.Run("incomplete pool times out with the context bound", func(t *testing.T) {
		g := NewWithT(t)
		cli := newFakeClient()

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		_, err := WaitForResourceSlices(ctx, cli, DRADriver, testNodeName)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("timed out"))
	})
}

func TestWaitForChangedResourceSlices(t *testing.T) {
	// previous is a complete single-slice snapshot with the pre-mutation
	// memory capacity.
	previous := PoolSnapshot{
		Driver:             DRADriver,
		Pool:               testNodeName,
		Generation:         1,
		ResourceSliceCount: 1,
		Complete:           true,
		Slices:             []resourcev1.ResourceSlice{resourceSliceWithMemory(DRADriver, testNodeName, 1, 1, 11441)},
	}

	t.Run("generation bump is accepted as the change", func(t *testing.T) {
		g := NewWithT(t)
		cli := newFakeClient(&resourcev1.ResourceSlice{
			ObjectMeta: metav1.ObjectMeta{Name: testSliceName},
			Spec: resourcev1.ResourceSliceSpec{
				Driver: DRADriver,
				Pool:   resourcev1.ResourcePool{Name: testNodeName, Generation: 2, ResourceSliceCount: 1},
				Devices: []resourcev1.Device{{
					Name: "gpu-0",
					Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
						testMemoryKey: {Value: *resource.NewQuantity(11441*1024*1024, resource.BinarySI)},
					},
				}},
			},
		})

		changed, err := WaitForChangedResourceSlices(context.Background(), cli, DRADriver, testNodeName, previous)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(changed.Generation).To(BeNumerically(">", previous.Generation))
	})

	t.Run("in-place device change at the same generation is accepted", func(t *testing.T) {
		g := NewWithT(t)
		// FGO v0.2.1's DRA library keeps the generation when a single
		// matched slice is updated in place; the changed facts must still
		// be detected.
		cli := newFakeClient(&resourcev1.ResourceSlice{
			ObjectMeta: metav1.ObjectMeta{Name: testSliceName},
			Spec: resourcev1.ResourceSliceSpec{
				Driver: DRADriver,
				Pool:   resourcev1.ResourcePool{Name: testNodeName, Generation: 1, ResourceSliceCount: 1},
				Devices: []resourcev1.Device{{
					Name: "gpu-0",
					Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
						testMemoryKey: {Value: *resource.NewQuantity(16384*1024*1024, resource.BinarySI)},
					},
				}},
			},
		})

		changed, err := WaitForChangedResourceSlices(context.Background(), cli, DRADriver, testNodeName, previous)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(changed.Generation).To(Equal(previous.Generation))
		g.Expect(devicesEqual(previous.Slices, changed.Slices)).To(BeFalse())
	})

	t.Run("unchanged snapshot times out with the context bound", func(t *testing.T) {
		g := NewWithT(t)
		unchanged := resourceSliceWithMemory(DRADriver, testNodeName, 1, 1, 11441)
		cli := newFakeClient(&unchanged)

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		_, err := WaitForChangedResourceSlices(ctx, cli, DRADriver, testNodeName, previous)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("timed out"))
	})
}

// resourceSliceWithMemory builds a complete single-slice pool whose single
// device carries the given memory capacity in MiB.
func resourceSliceWithMemory(driver, pool string, generation, count, memoryMi int64) resourcev1.ResourceSlice {
	slice := resourceSlice(driver, pool, generation, count, 1)
	slice.Spec.Devices[0].Capacity = map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
		testMemoryKey: {Value: *resource.NewQuantity(memoryMi*1024*1024, resource.BinarySI)},
	}
	return slice
}

func TestDevicesEqualIgnoresOrdering(t *testing.T) {
	g := NewWithT(t)

	twoDevices := func(memoryMi int64) resourcev1.ResourceSlice {
		slice := resourceSlice(DRADriver, testNodeName, 1, 1, 2)
		for i := range slice.Spec.Devices {
			slice.Spec.Devices[i].Capacity = map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
				testMemoryKey: {Value: *resource.NewQuantity(memoryMi*1024*1024, resource.BinarySI)},
			}
		}
		return slice
	}

	ordered := []resourcev1.ResourceSlice{twoDevices(11441)}

	reversed := []resourcev1.ResourceSlice{func() resourcev1.ResourceSlice {
		s := twoDevices(11441)
		slices.Reverse(s.Spec.Devices)
		return s
	}()}
	g.Expect(devicesEqual(ordered, reversed)).To(BeTrue(), "device order inside a slice must not matter")

	changedFacts := []resourcev1.ResourceSlice{twoDevices(16384)}
	g.Expect(devicesEqual(ordered, changedFacts)).To(BeFalse(), "changed capacity is a device change")

	renamed := []resourcev1.ResourceSlice{func() resourcev1.ResourceSlice {
		s := twoDevices(11441)
		s.Spec.Devices[0].Name = "gpu-renamed"
		return s
	}()}
	g.Expect(devicesEqual(ordered, renamed)).To(BeFalse(), "a renamed device is a different device set")

	g.Expect(devicesEqual(ordered, nil)).To(BeFalse(), "different slice counts are unequal")
}

func TestMutateNodeTopologyPreservesUnknownFields(t *testing.T) {
	g := NewWithT(t)
	cli := newFakeClient(topologyConfigMap(topologyYAMLFixture))

	g.Expect(MutateNodeTopology(context.Background(), cli, testNodeName, func(doc map[string]any) {
		doc["gpuMemory"] = 16384
		doc["gpuProduct"] = "Tesla-V100"
	})).To(Succeed())

	topology, err := GetNodeTopology(context.Background(), cli, testNodeName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(topology.GpuMemoryMi).To(Equal(16384))
	g.Expect(topology.GpuProduct).To(Equal("Tesla-V100"))
	g.Expect(topology.GPUIDs).To(HaveLen(2), "identity fields survive a facts mutation")

	// Unknown fields are preserved verbatim, not dropped by re-marshaling.
	cm := &corev1.ConfigMap{}
	g.Expect(cli.Get(context.Background(),
		client.ObjectKey{Namespace: Namespace, Name: TopologyConfigMapName(testNodeName)}, cm)).To(Succeed())

	preserved := map[string]any{}
	g.Expect(yaml.Unmarshal([]byte(cm.Data[TopologyDataKey]), &preserved)).To(Succeed())
	g.Expect(preserved).To(HaveKeyWithValue("driverVersion", "560.35.05"))
	g.Expect(preserved).To(HaveKeyWithValue("migStrategy", "mixed"))
}

func draPluginPod(name string, ready bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: Namespace,
			Name:      name,
			Labels:    map[string]string{testComponentKey: DRAPodComponentLabel},
		},
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	return pod
}

func TestWaitForPodsReady(t *testing.T) {
	t.Run("ready pods satisfy the minimum", func(t *testing.T) {
		g := NewWithT(t)
		cli := newFakeClient(draPluginPod("dra-a", true))

		g.Expect(WaitForPodsReady(context.Background(), cli,
			client.MatchingLabels{testComponentKey: DRAPodComponentLabel}, 1)).To(Succeed())
	})

	t.Run("not-ready pods time out with the context bound", func(t *testing.T) {
		g := NewWithT(t)
		cli := newFakeClient(draPluginPod("dra-a", false))

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		err := WaitForPodsReady(ctx, cli,
			client.MatchingLabels{testComponentKey: DRAPodComponentLabel}, 1)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("timed out"))
	})

	t.Run("non-positive minimum is rejected", func(t *testing.T) {
		g := NewWithT(t)
		cli := newFakeClient()
		g.Expect(WaitForPodsReady(context.Background(), cli,
			client.MatchingLabels{"component": DevicePluginPodComponentLabel}, 0)).To(HaveOccurred())
	})
}

func TestDeleteDRAPluginPods(t *testing.T) {
	g := NewWithT(t)
	cli := newFakeClient(draPluginPod("dra-plugin-old", true), draPluginPod("dra-plugin-other", true))

	deleted, err := deleteDRAPluginPods(context.Background(), cli)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(deleted).To(ConsistOf("dra-plugin-old", "dra-plugin-other"))

	pods := &corev1.PodList{}
	g.Expect(cli.List(context.Background(), pods, client.InNamespace(Namespace))).To(Succeed())
	g.Expect(pods.Items).To(BeEmpty(), "all dra-plugin pods are deleted")
}

func TestDeleteDRAPluginPodsWithoutPodsFails(t *testing.T) {
	g := NewWithT(t)
	cli := newFakeClient()

	_, err := deleteDRAPluginPods(context.Background(), cli)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("no gpu.nvidia.com pods found"))
}

func TestWaitDRAPluginPodsReady(t *testing.T) {
	t.Run("replacement pods ready", func(t *testing.T) {
		g := NewWithT(t)
		// The deleted pods are gone; a fresh replacement is already Ready.
		cli := newFakeClient(draPluginPod("dra-plugin-abc123", true))

		g.Expect(waitDRAPluginPodsReady(context.Background(), cli,
			[]string{"dra-plugin-old"})).To(Succeed())
	})

	t.Run("no replacement times out with the context bound", func(t *testing.T) {
		g := NewWithT(t)
		cli := newFakeClient()

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		err := waitDRAPluginPodsReady(ctx, cli, []string{"dra-plugin-old"})
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("timed out"))
	})
}

func TestReleaseInstalled(t *testing.T) {
	t.Run("release secret is detected", func(t *testing.T) {
		g := NewWithT(t)
		cli := newFakeClient(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: Namespace,
				Name:      "sh.helm.release.v1." + ReleaseName + ".v1",
			},
		})
		installed, err := ReleaseInstalled(context.Background(), cli)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(installed).To(BeTrue())
	})

	t.Run("foreign secrets are ignored", func(t *testing.T) {
		g := NewWithT(t)
		cli := newFakeClient(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: Namespace,
				Name:      "sh.helm.release.v1.other-release.v1",
			},
		})
		installed, err := ReleaseInstalled(context.Background(), cli)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(installed).To(BeFalse())
	})
}

func TestCollectSnapshotWritesEvidence(t *testing.T) {
	g := NewWithT(t)
	cli := newFakeClient(
		topologyConfigMap(topologyYAMLFixture),
		&resourcev1.ResourceSlice{
			ObjectMeta: metav1.ObjectMeta{Name: testSliceName},
			Spec: resourcev1.ResourceSliceSpec{
				Driver: DRADriver,
				Pool:   resourcev1.ResourcePool{Name: testNodeName, Generation: 1, ResourceSliceCount: 1},
			},
		},
	)

	dir := t.TempDir()
	g.Expect(CollectSnapshot(context.Background(), cli, dir)).To(Succeed())

	configMaps, err := readFile(t, dir, "simulator-configmaps.yaml")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(configMaps).To(ContainSubstring("topology-node-a"))

	resourceslices, err := readFile(t, dir, "resourceslices.yaml")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(resourceslices).To(ContainSubstring("driver: " + DRADriver))
}

func readFile(_ *testing.T, dir, name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	return string(data), err
}
