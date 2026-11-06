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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// pollInterval paces the bounded evidence waits.
const pollInterval = 2 * time.Second

// NodeTopology is a minimal test-side view of FGO's per-node topology.yml
// (ConfigMap data). It intentionally duplicates nothing from the
// production classic adapter: e2e fixtures and expected values must not
// share an oracle with the code under test (testing.md "Maintain
// independence").
type NodeTopology struct {
	GpuProduct  string
	GpuMemoryMi int
	GPUIDs      []string
}

// nodeTopologyDoc mirrors the YAML fields the harness reads for evidence.
type nodeTopologyDoc struct {
	GpuProduct string `json:"gpuProduct"`
	GpuMemory  int    `json:"gpuMemory"`
	Gpus       []struct {
		ID string `json:"id"`
	} `json:"gpus"`
}

// TopologyConfigMapName returns the per-node topology ConfigMap name.
func TopologyConfigMapName(nodeName string) string {
	return TopologyCMName + "-" + nodeName
}

// GetNodeTopology reads and parses one node's topology ConfigMap. Missing
// ConfigMap or data key is an error, never an empty snapshot.
func GetNodeTopology(ctx context.Context, cli client.Client, nodeName string) (*NodeTopology, error) {
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: Namespace, Name: TopologyConfigMapName(nodeName)}
	if err := cli.Get(ctx, key, cm); err != nil {
		return nil, fmt.Errorf("getting topology ConfigMap %s: %w", key, err)
	}

	raw, ok := cm.Data[TopologyDataKey]
	if !ok {
		return nil, fmt.Errorf("topology ConfigMap %s has no %s data key", key, TopologyDataKey)
	}

	return parseNodeTopology([]byte(raw))
}

// parseNodeTopology decodes topology.yml. Unknown fields are preserved
// only by MutateNodeTopology; this reader keeps the minimal evidence view.
func parseNodeTopology(data []byte) (*NodeTopology, error) {
	doc := nodeTopologyDoc{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", TopologyDataKey, err)
	}

	topology := &NodeTopology{
		GpuProduct:  doc.GpuProduct,
		GpuMemoryMi: doc.GpuMemory,
	}
	for _, gpu := range doc.Gpus {
		topology.GPUIDs = append(topology.GPUIDs, gpu.ID)
	}
	return topology, nil
}

// WaitForNodeTopology waits (bounded by ctx) until the node's topology
// ConfigMap exists and parses, then returns it.
func WaitForNodeTopology(ctx context.Context, cli client.Client, nodeName string) (*NodeTopology, error) {
	var result *NodeTopology
	if err := poll(ctx, func() (bool, error) {
		topology, err := GetNodeTopology(ctx, cli, nodeName)
		if err == nil {
			result = topology
			return true, nil
		}
		if !apierrors.IsNotFound(err) {
			return false, err
		}
		return false, nil
	}); err != nil {
		return nil, err
	}

	return result, nil
}

// PoolSnapshot is a complete-or-incomplete view of one DRA pool
// (driver, pool.name): the slices present at the highest generation and
// whether they satisfy the pool's declared resourceSliceCount.
type PoolSnapshot struct {
	Driver             string
	Pool               string
	Generation         int64
	ResourceSliceCount int64
	Slices             []resourcev1.ResourceSlice
	Complete           bool
}

// ListResourceSlices lists every ResourceSlice in the cluster.
func ListResourceSlices(ctx context.Context, cli client.Client) ([]resourcev1.ResourceSlice, error) {
	list := &resourcev1.ResourceSliceList{}
	if err := cli.List(ctx, list); err != nil {
		return nil, fmt.Errorf("listing ResourceSlices: %w", err)
	}
	return list.Items, nil
}

// SnapshotPool reduces the slices of one (driver, pool.name) pool to a
// PoolSnapshot. An absent pool yields an empty, incomplete snapshot.
func SnapshotPool(collection []resourcev1.ResourceSlice, driver, pool string) PoolSnapshot {
	snapshot := PoolSnapshot{Driver: driver, Pool: pool}

	var atMax []resourcev1.ResourceSlice
	for i := range collection {
		slice := &collection[i]
		if slice.Spec.Driver != driver || slice.Spec.Pool.Name != pool {
			continue
		}
		if slice.Spec.Pool.Generation > snapshot.Generation {
			snapshot.Generation = slice.Spec.Pool.Generation
			atMax = nil
		}
		if slice.Spec.Pool.Generation == snapshot.Generation {
			atMax = append(atMax, *slice)
			snapshot.ResourceSliceCount = slice.Spec.Pool.ResourceSliceCount
		}
	}

	snapshot.Slices = atMax
	snapshot.Complete = snapshot.ResourceSliceCount > 0 &&
		int64(len(atMax)) == snapshot.ResourceSliceCount
	return snapshot
}

// WaitForResourceSlices waits (bounded by ctx) until the pool has a
// complete snapshot at the highest generation, then returns it.
func WaitForResourceSlices(ctx context.Context, cli client.Client, driver, pool string) (PoolSnapshot, error) {
	var last PoolSnapshot
	if err := poll(ctx, func() (bool, error) {
		all, err := ListResourceSlices(ctx, cli)
		if err != nil {
			return false, err
		}
		last = SnapshotPool(all, driver, pool)
		return last.Complete && len(last.Slices) > 0, nil
	}); err != nil {
		return last, err
	}

	return last, nil
}

// WaitForChangedResourceSlices waits (bounded by ctx) until the pool has a
// complete snapshot that differs from previous: either a strictly higher
// generation, or — when the simulator's DRA library updates a single
// matched slice in place — device content that no longer matches the
// previous snapshot.
//
// The frozen fact-mutation protocol (design doc §9.1) describes the change
// as a generation bump. [source-inspected] FGO v0.2.1 pins
// k8s.io/dynamic-resource-allocation v0.34.2, whose resourceslice
// controller keeps the generation when exactly one matched slice is updated
// in place ("Kept generation because at most one update API call is
// necessary"); a bump only happens when slices are added, removed, or more
// than one changes. A single-slice pool (one node, gpuCount devices in one
// slice) therefore republishes changed facts at the same generation. The
// wait accepts both shapes and always requires a complete snapshot (full
// resourceSliceCount at the highest generation).
func WaitForChangedResourceSlices(
	ctx context.Context,
	cli client.Client,
	driver, pool string,
	previous PoolSnapshot,
) (PoolSnapshot, error) {
	changed := func(current PoolSnapshot) bool {
		if current.Generation > previous.Generation {
			return true
		}
		return !devicesEqual(previous.Slices, current.Slices)
	}

	var last PoolSnapshot
	if err := poll(ctx, func() (bool, error) {
		all, err := ListResourceSlices(ctx, cli)
		if err != nil {
			return false, err
		}
		last = SnapshotPool(all, driver, pool)
		return last.Complete && len(last.Slices) > 0 && changed(last), nil
	}); err != nil {
		return last, err
	}

	return last, nil
}

// devicesEqual compares the device sets of two snapshot slice collections
// semantically, independent of slice and device ordering.
func devicesEqual(a, b []resourcev1.ResourceSlice) bool {
	if len(a) != len(b) {
		return false
	}

	canonicalA := canonicalDevices(a)
	canonicalB := canonicalDevices(b)
	for i := range canonicalA {
		if !apiequality.Semantic.DeepEqual(canonicalA[i], canonicalB[i]) {
			return false
		}
	}
	return true
}

// canonicalDevices sorts each slice's devices by name and the slices by
// their first device name, producing an order-independent comparison form.
func canonicalDevices(collection []resourcev1.ResourceSlice) [][]resourcev1.Device {
	sorted := make([][]resourcev1.Device, 0, len(collection))
	for i := range collection {
		devices := make([]resourcev1.Device, len(collection[i].Spec.Devices))
		copy(devices, collection[i].Spec.Devices)
		slices.SortFunc(devices, func(x, y resourcev1.Device) int {
			return strings.Compare(x.Name, y.Name)
		})
		sorted = append(sorted, devices)
	}
	slices.SortFunc(sorted, func(x, y []resourcev1.Device) int {
		if len(x) == 0 && len(y) == 0 {
			return 0
		}
		if len(x) == 0 {
			return -1
		}
		if len(y) == 0 {
			return 1
		}
		return strings.Compare(x[0].Name, y[0].Name)
	})
	return sorted
}

// GetDeviceClass reads the chart-rendered gpu.nvidia.com DeviceClass.
func GetDeviceClass(ctx context.Context, cli client.Client) (*resourcev1.DeviceClass, error) {
	class := &resourcev1.DeviceClass{}
	key := client.ObjectKey{Name: DeviceClassName}
	if err := cli.Get(ctx, key, class); err != nil {
		return nil, fmt.Errorf("getting DeviceClass %s: %w", DeviceClassName, err)
	}
	return class, nil
}

// WaitForPodsReady waits (bounded by ctx) until at least min pods matching
// labels in the simulator namespace exist and are Ready. It is the generic
// readiness wait behind the profile-specific evidence checks (classic
// device-plugin pods, DRA kubelet plugin pods).
func WaitForPodsReady(ctx context.Context, cli client.Client, labels client.MatchingLabels, min int) error {
	if min <= 0 {
		return fmt.Errorf("WaitForPodsReady requires a positive minimum, got %d", min)
	}

	return poll(ctx, func() (bool, error) {
		pods := &corev1.PodList{}
		if err := cli.List(ctx, pods, client.InNamespace(Namespace), labels); err != nil {
			return false, fmt.Errorf("listing pods with labels %v: %w", map[string]string(labels), err)
		}

		ready := 0
		for i := range pods.Items {
			if podReady(&pods.Items[i]) {
				ready++
			}
		}
		return ready >= min, nil
	})
}

// CollectSnapshot writes the simulator's source evidence (per-node
// topology ConfigMaps and all ResourceSlices) into dir as YAML, for
// failure diagnostics before a run-owned cluster is destroyed.
func CollectSnapshot(ctx context.Context, cli client.Client, dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating snapshot directory: %w", err)
	}

	configMaps := &corev1.ConfigMapList{}
	if err := cli.List(ctx, configMaps, client.InNamespace(Namespace)); err != nil {
		return fmt.Errorf("listing %s ConfigMaps for snapshot: %w", Namespace, err)
	}
	configMapData, err := yaml.Marshal(configMaps)
	if err != nil {
		return fmt.Errorf("marshaling ConfigMap snapshot: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "simulator-configmaps.yaml"), configMapData, 0o600); err != nil {
		return fmt.Errorf("writing ConfigMap snapshot: %w", err)
	}

	all, err := ListResourceSlices(ctx, cli)
	if err != nil {
		return fmt.Errorf("listing ResourceSlices for snapshot: %w", err)
	}
	sliceData, err := yaml.Marshal(resourcev1.ResourceSliceList{Items: all})
	if err != nil {
		return fmt.Errorf("marshaling ResourceSlice snapshot: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "resourceslices.yaml"), sliceData, 0o600); err != nil {
		return fmt.Errorf("writing ResourceSlice snapshot: %w", err)
	}

	return nil
}

// poll runs check every pollInterval until it returns true, the context
// is done or check errors. check errors and context expiry are propagated.
func poll(ctx context.Context, check func() (bool, error)) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		done, err := check()
		if err != nil {
			return err
		}
		if done {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for simulator evidence: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
