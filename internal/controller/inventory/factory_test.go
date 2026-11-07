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
	"testing"

	. "github.com/onsi/gomega"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/inventory"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
)

func disabled() *bool {
	value := false
	return &value
}

func enabled() *bool {
	value := true
	return &value
}

// sourceIDs renders the registry in registration order.
func sourceIDs(sources discovery.Sources) []string {
	list := sources.List()
	ids := make([]string, 0, len(list))
	for _, src := range list {
		ids = append(ids, src.ID())
	}
	return ids
}

// TestNewSourceFactoryDefaults covers the production factory: omitted
// arms default to enabled, DRA registers before classic (registration
// order is the SelectNode priority), and every disabled arm is simply
// absent from the registry.
func TestNewSourceFactoryDefaults(t *testing.T) {
	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	factory := inventory.NewSourceFactory(reader)

	cases := []struct {
		name string
		cfg  infrastructurev1alpha1.DiscoverySources
		want []string
	}{
		{
			name: "omitted arms default to dra then classic",
			cfg:  infrastructurev1alpha1.DiscoverySources{},
			want: []string{sourceIDDRA, sourceIDClassic},
		},
		{
			name: "nil-enabled arms default to enabled",
			cfg: infrastructurev1alpha1.DiscoverySources{
				DRA:     &infrastructurev1alpha1.DRASourceConfig{Enabled: enabled()},
				Classic: &infrastructurev1alpha1.ClassicSourceConfig{Enabled: enabled()},
			},
			want: []string{sourceIDDRA, sourceIDClassic},
		},
		{
			name: "disabled dra registers classic only",
			cfg: infrastructurev1alpha1.DiscoverySources{
				DRA: &infrastructurev1alpha1.DRASourceConfig{Enabled: disabled()},
			},
			want: []string{sourceIDClassic},
		},
		{
			name: "disabled classic registers dra only",
			cfg: infrastructurev1alpha1.DiscoverySources{
				Classic: &infrastructurev1alpha1.ClassicSourceConfig{Enabled: disabled()},
			},
			want: []string{sourceIDDRA},
		},
		{
			name: "both disabled registers nothing",
			cfg: infrastructurev1alpha1.DiscoverySources{
				DRA:     &infrastructurev1alpha1.DRASourceConfig{Enabled: disabled()},
				Classic: &infrastructurev1alpha1.ClassicSourceConfig{Enabled: disabled()},
			},
			want: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			sources, err := factory(tc.cfg)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(sourceIDs(sources)).To(Equal(tc.want))
		})
	}
}

// TestNewSourceFactoryInvalidConfig covers configuration the source
// constructors reject: it surfaces as an error, never as a registry that
// silently drops a source.
func TestNewSourceFactoryInvalidConfig(t *testing.T) {
	g := NewWithT(t)

	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	factory := inventory.NewSourceFactory(reader)

	// An explicitly empty driver allow-list is a deliberate
	// misconfiguration, not a default.
	_, err := factory(infrastructurev1alpha1.DiscoverySources{
		DRA: &infrastructurev1alpha1.DRASourceConfig{
			Enabled:         enabled(),
			DriverAllowList: []string{},
		},
	})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("driver allow-list"))
}

// TestNewSourceFactoryNilReader covers the reader precondition: a nil
// reader is rejected by the first registered source constructor.
func TestNewSourceFactoryNilReader(t *testing.T) {
	g := NewWithT(t)

	factory := inventory.NewSourceFactory(nil)
	_, err := factory(infrastructurev1alpha1.DiscoverySources{})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("dra: reader must not be nil"))
}
