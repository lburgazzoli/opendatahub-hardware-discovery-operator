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

package v1alpha1_test

import (
	"encoding/json"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
)

// exampleHardwareDiscovery mirrors the approved example CR (design doc §5.3).
func exampleHardwareDiscovery() *v1alpha1.HardwareDiscovery {
	enabled, disabled := true, false
	return &v1alpha1.HardwareDiscovery{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiVersion,
			Kind:       discoveryKind,
		},
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: v1alpha1.HardwareDiscoverySpec{
			Class: "accelerator",
			NodeSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{nodePoolLabelKey: nodePoolLabelVal},
			},
			MaxStale:        metav1.Duration{Duration: 10 * time.Minute},
			RescanPeriod:    metav1.Duration{Duration: 5 * time.Minute},
			NodeGracePeriod: metav1.Duration{Duration: 15 * time.Minute},
			Sources: v1alpha1.DiscoverySources{
				DRA: &v1alpha1.DRASourceConfig{
					Enabled:         &enabled,
					DriverAllowList: []string{nvidiaDriver},
				},
				Simulator: &v1alpha1.SimulatorSourceConfig{
					Enabled:         &disabled,
					Namespace:       "gpu-operator",
					ConfigMapPrefix: "topology",
				},
			},
		},
		Status: v1alpha1.HardwareDiscoveryStatus{
			ObservedGeneration: 1,
			NodeSummaries: []v1alpha1.NodeSummary{
				{
					Name:        nodeName,
					UID:         types.UID(nodeUID),
					Plane:       "dra",
					DeviceCount: 2,
					Outcome:     "Success",
				},
			},
		},
	}
}

func TestHardwareDiscoverySpecJSONLayout(t *testing.T) {
	g := NewWithT(t)

	singleton := exampleHardwareDiscovery()
	singleton.Status = v1alpha1.HardwareDiscoveryStatus{}

	raw, err := json.Marshal(singleton)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(raw)).To(MatchJSON(`{
		"apiVersion": "infrastructure.platform.opendatahub.io/v1alpha1",
		"kind": "HardwareDiscovery",
		"metadata": {"name": "cluster"},
		"spec": {
			"class": "accelerator",
			"nodeSelector": {
				"matchLabels": {"run.ai/simulated-gpu-node-pool": "default"}
			},
			"maxStale": "10m0s",
			"rescanPeriod": "5m0s",
			"nodeGracePeriod": "15m0s",
			"sources": {
				"dra": {
					"enabled": true,
					"driverAllowList": ["gpu.nvidia.com"]
				},
				"simulator": {
					"enabled": false,
					"namespace": "gpu-operator",
					"configMapPrefix": "topology"
				}
			}
		}
	}`))
}

func TestHardwareDiscoverySpecOmitsEmpty(t *testing.T) {
	g := NewWithT(t)

	raw, err := json.Marshal(&v1alpha1.HardwareDiscovery{
		Spec: v1alpha1.HardwareDiscoverySpec{Class: "accelerator"},
	})
	g.Expect(err).NotTo(HaveOccurred())

	var doc map[string]any
	g.Expect(json.Unmarshal(raw, &doc)).To(Succeed())
	spec := doc["spec"].(map[string]any)
	g.Expect(spec).To(MatchKeys(IgnoreExtras, Keys{"class": Equal("accelerator")}))
	g.Expect(spec).NotTo(HaveKey("nodeSelector"))
	g.Expect(spec).NotTo(HaveKey("maxStale"))
	g.Expect(spec).NotTo(HaveKey("rescanPeriod"))
	g.Expect(spec).NotTo(HaveKey("nodeGracePeriod"))
	g.Expect(spec).NotTo(HaveKey("sources"))
}

func TestHardwareDiscoveryStatusJSONLayout(t *testing.T) {
	g := NewWithT(t)

	raw, err := json.Marshal(exampleHardwareDiscovery())
	g.Expect(err).NotTo(HaveOccurred())

	var doc map[string]any
	g.Expect(json.Unmarshal(raw, &doc)).To(Succeed())
	status := doc["status"].(map[string]any)
	g.Expect(status).To(MatchKeys(IgnoreExtras, Keys{
		"observedGeneration": Equal(float64(1)),
		"nodeSummaries": ContainElement(MatchAllKeys(Keys{
			"name":        Equal("gpu-node-a"),
			"uid":         Equal("11111111-1111-1111-1111-111111111111"),
			"plane":       Equal("dra"),
			"deviceCount": Equal(float64(2)),
			"outcome":     Equal("Success"),
		})),
	}))
}

func TestHardwareDiscoveryRoundTrip(t *testing.T) {
	g := NewWithT(t)

	raw, err := json.Marshal(exampleHardwareDiscovery())
	g.Expect(err).NotTo(HaveOccurred())

	var got v1alpha1.HardwareDiscovery
	g.Expect(json.Unmarshal(raw, &got)).To(Succeed())
	g.Expect(&got).To(Equal(exampleHardwareDiscovery()))
}

func TestHardwareDiscoveryDeepCopy(t *testing.T) {
	g := NewWithT(t)

	singleton := exampleHardwareDiscovery()
	clone := singleton.DeepCopy()
	g.Expect(clone).To(Equal(singleton))
	g.Expect(clone).NotTo(BeIdenticalTo(singleton))

	clone.Spec.NodeSelector.MatchLabels["run.ai/simulated-gpu-node-pool"] = "mutated"
	g.Expect(singleton.Spec.NodeSelector.MatchLabels["run.ai/simulated-gpu-node-pool"]).
		To(Equal("default"), "deep copy must not alias nested pointers")
}
