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
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
)

func TestSchemeRegistersApprovedKinds(t *testing.T) {
	g := NewWithT(t)

	// A scheme local to the API package; the scaffolded cmd/main.go registers
	// this same SchemeBuilder for the manager.
	scheme := runtime.NewScheme()
	g.Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())

	groupVersion := schema.GroupVersion{Group: "infrastructure.platform.opendatahub.io", Version: "v1alpha1"}

	for _, tc := range []struct {
		kind     string
		listKind string
	}{
		{kind: deviceKind, listKind: deviceKind + "List"},
		{kind: discoveryKind, listKind: discoveryKind + "List"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			g := NewWithT(t)
			gvk := groupVersion.WithKind(tc.kind)
			listGVK := groupVersion.WithKind(tc.listKind)

			obj, err := scheme.New(gvk)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(obj).To(BeAssignableToTypeOf(schemeTarget(tc.kind)))

			gvks, isUnstructured, err := scheme.ObjectKinds(obj)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(isUnstructured).To(BeFalse())
			g.Expect(gvks).To(ConsistOf(gvk))

			list, err := scheme.New(listGVK)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(list).To(BeAssignableToTypeOf(schemeTarget(tc.listKind)))
			gvks, isUnstructured, err = scheme.ObjectKinds(list)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(isUnstructured).To(BeFalse())
			g.Expect(gvks).To(ConsistOf(listGVK))
		})
	}

	t.Run("served group version", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(v1alpha1.SchemeGroupVersion).To(Equal(groupVersion))
	})
}

// schemeTarget returns a zero object of the kind for assignability checks.
func schemeTarget(kind string) any {
	switch kind {
	case deviceKind:
		return &v1alpha1.HardwareDevice{}
	case deviceKind + "List":
		return &v1alpha1.HardwareDeviceList{}
	case discoveryKind:
		return &v1alpha1.HardwareDiscovery{}
	case discoveryKind + "List":
		return &v1alpha1.HardwareDiscoveryList{}
	default:
		panic("unknown kind " + kind)
	}
}
