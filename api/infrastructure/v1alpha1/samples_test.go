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
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
)

// The samples in config/samples must stay decodable into the typed APIs;
// they are applied separately after the CRDs are established (demo/e2e), so
// silent drift breaks the documented walkthrough.
func TestSamplesDecode(t *testing.T) {
	samplesDir := filepath.Join("..", "..", "..", "config", "samples")

	t.Run("hardware device sample matches design doc 4.3", func(t *testing.T) {
		g := NewWithT(t)
		raw, err := os.ReadFile(filepath.Join(samplesDir, "infrastructure_v1alpha1_hardwaredevice.yaml"))
		g.Expect(err).NotTo(HaveOccurred())

		var dev v1alpha1.HardwareDevice
		g.Expect(yaml.UnmarshalStrict(raw, &dev)).To(Succeed())
		g.Expect(dev.Name).To(Equal(deviceName))
		g.Expect(dev.Spec).To(MatchAllFields(Fields{
			"Class": Equal(acceleratorClass),
			"Identity": MatchAllFields(Fields{
				"ID": Equal(deviceID),
			}),
			"Host": MatchAllFields(Fields{
				"Name": Equal(nodeName),
				"UID":  Equal(types.UID(nodeUID)),
			}),
			"Source": MatchAllFields(Fields{
				"DRA": PointTo(MatchAllFields(Fields{
					"Driver": Equal(nvidiaDriver),
					"Pool":   Equal(nodeName),
					"Device": Equal(deviceName),
				})),
				"Simulator": BeNil(),
			}),
		}))
	})

	t.Run("hardware discovery sample matches design doc 5.3", func(t *testing.T) {
		g := NewWithT(t)
		raw, err := os.ReadFile(filepath.Join(samplesDir, "infrastructure_v1alpha1_hardwarediscovery.yaml"))
		g.Expect(err).NotTo(HaveOccurred())

		var singleton v1alpha1.HardwareDiscovery
		g.Expect(yaml.UnmarshalStrict(raw, &singleton)).To(Succeed())
		g.Expect(singleton.Name).To(Equal("cluster"), "the singleton instance name is fixed")
		g.Expect(singleton.Spec.Class).To(Equal(acceleratorClass))
		g.Expect(singleton.Spec.NodeSelector).To(PointTo(MatchAllFields(Fields{
			"MatchLabels":      HaveKeyWithValue(nodePoolLabelKey, nodePoolLabelVal),
			"MatchExpressions": BeEmpty(),
		})))
		g.Expect(singleton.Spec.Sources.Simulator).To(PointTo(MatchAllFields(Fields{
			"Enabled":   PointTo(BeTrue()),
			"Namespace": Equal(simulatorSourceNS),
			// configMapPrefix is an API-server default (topology); it does not
			// apply on plain client-side unmarshal.
			"ConfigMapPrefix": Equal(""),
		})))
	})
}
