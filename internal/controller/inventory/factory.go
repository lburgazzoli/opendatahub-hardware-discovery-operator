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

package inventory

import (
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/classic"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/dra"
)

// SourceFactory builds the ordered source registry for one singleton
// configuration. The registry is rebuilt on every scan, so configuration
// changes take effect without a restart. The production factory
// (NewSourceFactory) registers the DRA source before the classic source:
// registration order is the SelectNode priority, and DRA claims must win
// the fold.
type SourceFactory func(cfg infrastructurev1alpha1.DiscoverySources) (discovery.Sources, error)

// NewSourceFactory returns the production SourceFactory over a read-only
// client. Scan-driven direct reads (an uncached reader such as the
// manager's API reader) keep optional APIs like resource.k8s.io/v1 from
// blocking startup or cache synchronization; the sources themselves
// classify unreadable inputs as Unknown.
func NewSourceFactory(reader client.Reader) SourceFactory {
	return func(cfg infrastructurev1alpha1.DiscoverySources) (discovery.Sources, error) {
		registered := make([]discovery.Source, 0, 2)
		if draEnabled(cfg) {
			source, err := dra.New(reader, dra.WithDriverAllowList(draDrivers(cfg)...))
			if err != nil {
				return discovery.Sources{}, fmt.Errorf("building dra source: %w", err)
			}
			registered = append(registered, source)
		}
		if classicEnabled(cfg) {
			source, err := classic.New(reader, classic.WithResourceName(classicResourceName(cfg)))
			if err != nil {
				return discovery.Sources{}, fmt.Errorf("building classic source: %w", err)
			}
			registered = append(registered, source)
		}
		return discovery.NewSources(registered...)
	}
}

// draEnabled reports whether the DRA source is configured on. A nil arm
// or a nil Enabled defaults to enabled, matching the schema default.
func draEnabled(cfg infrastructurev1alpha1.DiscoverySources) bool {
	return cfg.DRA == nil || cfg.DRA.Enabled == nil || *cfg.DRA.Enabled
}

// draDrivers returns the trusted driver set. A nil allow-list defaults to
// the pinned whole-GPU driver; an explicitly empty one is rejected by the
// source constructor and surfaces as invalid configuration.
func draDrivers(cfg infrastructurev1alpha1.DiscoverySources) []string {
	if cfg.DRA == nil || cfg.DRA.DriverAllowList == nil {
		return []string{dra.DefaultDriver}
	}
	return cfg.DRA.DriverAllowList
}

// classicEnabled reports whether the classic labels source is configured
// on. A nil arm or a nil Enabled defaults to enabled: classic claims stay
// evidence-gated, so enabling it never fabricates devices.
func classicEnabled(cfg infrastructurev1alpha1.DiscoverySources) bool {
	return cfg.Classic == nil || cfg.Classic.Enabled == nil || *cfg.Classic.Enabled
}

// classicResourceName returns the extended resource whose presence in
// node allocatable/capacity is classic-plane evidence.
func classicResourceName(cfg infrastructurev1alpha1.DiscoverySources) string {
	if cfg.Classic == nil || cfg.Classic.ResourceName == "" {
		return classic.DefaultResourceName
	}
	return cfg.Classic.ResourceName
}
