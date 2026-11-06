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

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// MutateNodeTopology edits one node's topology input ConfigMap in place.
//
// The mutation is expressed on the raw YAML document (map[string]any) so
// unknown fields FGO publishes (driverVersion, cudaVersion, status, ...)
// are preserved verbatim. The mutate function receives the parsed document
// and may change any field; harness scenarios change gpuProduct/gpuMemory.
//
// This is step 1 of the frozen fact-mutation protocol (design doc §9.1):
// the topology-server serves the ConfigMap live, but the native DRA plugin
// enumerates only at startup. After mutating, call RestartDRAPlugin and
// then WaitForChangedResourceSlices before asserting anything about the
// published facts. Never assume live republishing.
func MutateNodeTopology(
	ctx context.Context,
	cli client.Client,
	nodeName string,
	mutate func(doc map[string]any),
) error {
	key := client.ObjectKey{Namespace: Namespace, Name: TopologyConfigMapName(nodeName)}

	cm := &corev1.ConfigMap{}
	if err := cli.Get(ctx, key, cm); err != nil {
		return fmt.Errorf("getting topology ConfigMap %s: %w", key, err)
	}

	raw, ok := cm.Data[TopologyDataKey]
	if !ok {
		return fmt.Errorf("topology ConfigMap %s has no %s data key", key, TopologyDataKey)
	}

	doc := map[string]any{}
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return fmt.Errorf("parsing %s in %s: %w", TopologyDataKey, key, err)
	}

	mutate(doc)

	updated, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshaling mutated %s: %w", TopologyDataKey, err)
	}

	cm.Data[TopologyDataKey] = string(updated)
	if err := cli.Update(ctx, cm); err != nil {
		return fmt.Errorf("updating topology ConfigMap %s: %w", key, err)
	}

	return nil
}

// RestartDRAPlugin deletes the dra-plugin kubelet plugin pods so the
// DaemonSet recreates them and the new pods re-enumerate the (possibly
// mutated) topology input at startup. It waits (bounded by ctx) until at
// least as many replacement pods exist and are Ready. The full flow needs
// a live DaemonSet controller, so it is exercised by the e2e suite; the
// pieces are unit-tested separately.
func RestartDRAPlugin(ctx context.Context, cli client.Client) error {
	deleted, err := deleteDRAPluginPods(ctx, cli)
	if err != nil {
		return err
	}

	return waitDRAPluginPodsReady(ctx, cli, deleted)
}

// deleteDRAPluginPods deletes every dra-plugin kubelet plugin pod and
// returns the deleted pod names.
func deleteDRAPluginPods(ctx context.Context, cli client.Client) ([]string, error) {
	pods, err := listDRAPluginPods(ctx, cli)
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return nil, fmt.Errorf("no %s pods found in namespace %s", DRADriver, Namespace)
	}

	deleted := make([]string, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		if err := cli.Delete(ctx, pod); err != nil {
			return nil, fmt.Errorf("deleting dra-plugin pod %s: %w", pod.Name, err)
		}
		deleted = append(deleted, pod.Name)
	}

	return deleted, nil
}

// waitDRAPluginPodsReady waits (bounded by ctx) until at least as many
// pods that are not part of the deleted set exist and are Ready.
// (DaemonSet replacement pods get fresh generated names.)
func waitDRAPluginPodsReady(ctx context.Context, cli client.Client, deleted []string) error {
	gone := make(map[string]struct{}, len(deleted))
	for _, name := range deleted {
		gone[name] = struct{}{}
	}

	return poll(ctx, func() (bool, error) {
		current, err := listDRAPluginPods(ctx, cli)
		if err != nil {
			return false, err
		}

		ready := 0
		for i := range current {
			pod := &current[i]
			if _, old := gone[pod.Name]; old {
				continue
			}
			if podReady(pod) {
				ready++
			}
		}

		return ready >= len(deleted), nil
	})
}

// listDRAPluginPods lists the dra-plugin kubelet plugin pods in the fixed
// simulator namespace, selected by the chart's component label.
func listDRAPluginPods(ctx context.Context, cli client.Client) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := cli.List(ctx, pods,
		client.InNamespace(Namespace),
		client.MatchingLabels{"app.kubernetes.io/component": DRAPodComponentLabel},
	); err != nil {
		return nil, fmt.Errorf("listing dra-plugin pods: %w", err)
	}
	return pods.Items, nil
}

func podReady(pod *corev1.Pod) bool {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == corev1.PodReady {
			return pod.Status.Conditions[i].Status == corev1.ConditionTrue
		}
	}
	return false
}
