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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
)

// exampleHardwareDevice mirrors the approved example CR (design doc §4.3).
func exampleHardwareDevice() *v1alpha1.HardwareDevice {
	collected := metav1.NewTime(time.Date(2026, 11, 6, 15, 32, 31, 0, time.UTC))
	return &v1alpha1.HardwareDevice{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiVersion,
			Kind:       deviceKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: deviceName,
			Labels: map[string]string{
				deviceClassLabel: acceleratorClass,
				nodeLabel:        nodeName,
			},
		},
		Spec: v1alpha1.HardwareDeviceSpec{
			Class: acceleratorClass,
			Identity: v1alpha1.DeviceIdentity{
				ID: deviceID,
			},
			Host: v1alpha1.DeviceHost{
				Name: nodeName,
				UID:  types.UID(nodeUID),
			},
			Source: v1alpha1.DeviceSource{
				DRA: &v1alpha1.DRASource{
					Driver: nvidiaDriver,
					Pool:   nodeName,
					Device: deviceName,
				},
			},
		},
		Status: v1alpha1.HardwareDeviceStatus{
			Model:              "Tesla-K80",
			Memory:             quantity("12635713536"),
			AllocationPlane:    "dra",
			CollectedAt:        &collected,
			SourceRef:          "dra/gpu.nvidia.com/gpu-node-a",
			ObservedGeneration: 1,
		},
	}
}

// quantity builds a heap Quantity fixture with a stable string form.
func quantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func TestHardwareDeviceSpecJSONLayout(t *testing.T) {
	g := NewWithT(t)

	dev := exampleHardwareDevice()
	dev.Status = v1alpha1.HardwareDeviceStatus{} // spec layout only

	raw, err := json.Marshal(dev)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(raw)).To(MatchJSON(`{
		"apiVersion": "infrastructure.platform.opendatahub.io/v1alpha1",
		"kind": "HardwareDevice",
		"metadata": {
			"name": "gpu-11111111-2222-3333-4444-555555555555",
			"labels": {
				"hardwarediscovery.platform.opendatahub.io/device-class": "accelerator",
				"hardwarediscovery.platform.opendatahub.io/node": "gpu-node-a"
			}
		},
		"spec": {
			"class": "accelerator",
			"identity": {"id": "GPU-11111111-2222-3333-4444-555555555555"},
			"host": {
				"name": "gpu-node-a",
				"uid": "11111111-1111-1111-1111-111111111111"
			},
			"source": {
				"dra": {
					"driver": "gpu.nvidia.com",
					"pool": "gpu-node-a",
					"device": "gpu-11111111-2222-3333-4444-555555555555"
				}
			}
		}
	}`))
}

func TestHardwareDeviceStatusJSONLayout(t *testing.T) {
	t.Run("observed facts", func(t *testing.T) {
		g := NewWithT(t)
		raw, err := json.Marshal(exampleHardwareDevice())
		g.Expect(err).NotTo(HaveOccurred())

		var doc map[string]any
		g.Expect(json.Unmarshal(raw, &doc)).To(Succeed())
		status, ok := doc["status"].(map[string]any)
		g.Expect(ok).To(BeTrue(), "status must be an object")
		g.Expect(status).To(MatchKeys(IgnoreExtras, Keys{
			"model":              Equal("Tesla-K80"),
			"memory":             Equal("12635713536"),
			"allocationPlane":    Equal("dra"),
			"collectedAt":        Equal("2026-11-06T15:32:31Z"),
			"sourceRef":          Equal("dra/gpu.nvidia.com/gpu-node-a"),
			"observedGeneration": Equal(float64(1)),
		}))
		g.Expect(status).NotTo(HaveKey("vendor"), "vendor is omitted when unpublished")
	})

	t.Run("unknown facts are omitted, never zero", func(t *testing.T) {
		g := NewWithT(t)
		raw, err := json.Marshal(&v1alpha1.HardwareDevice{
			Spec: v1alpha1.HardwareDeviceSpec{Class: acceleratorClass},
		})
		g.Expect(err).NotTo(HaveOccurred())

		var doc map[string]any
		g.Expect(json.Unmarshal(raw, &doc)).To(Succeed())
		g.Expect(doc).NotTo(HaveKey("status"), "zero status is omitted")
	})
}

func TestHardwareDeviceRoundTrip(t *testing.T) {
	g := NewWithT(t)

	dev := exampleHardwareDevice()
	raw, err := json.Marshal(dev)
	g.Expect(err).NotTo(HaveOccurred())

	var got v1alpha1.HardwareDevice
	g.Expect(json.Unmarshal(raw, &got)).To(Succeed())

	// metav1.Time unmarshal normalizes to the local zone (standard
	// apimachinery behavior), so compare instants separately from the rest.
	gotCollected := got.Status.CollectedAt
	got.Status.CollectedAt = nil
	want := *dev
	wantCollected := want.Status.CollectedAt
	want.Status.CollectedAt = nil
	g.Expect(&got).To(Equal(&want))
	g.Expect(gotCollected.Time).To(BeTemporally("==", wantCollected.Time))
}

func TestHardwareDeviceDeepCopy(t *testing.T) {
	g := NewWithT(t)

	dev := exampleHardwareDevice()
	clone := dev.DeepCopy()
	g.Expect(clone).To(Equal(dev))
	g.Expect(clone).NotTo(BeIdenticalTo(dev))

	clone.Spec.Source.DRA.Pool = "mutated"
	g.Expect(dev.Spec.Source.DRA.Pool).To(Equal("gpu-node-a"), "deep copy must not alias nested pointers")

	list := &v1alpha1.HardwareDeviceList{Items: []v1alpha1.HardwareDevice{*dev}}
	g.Expect(list.DeepCopy()).To(Equal(list))
}
