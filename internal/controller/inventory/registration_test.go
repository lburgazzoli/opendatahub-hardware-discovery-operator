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
	"testing"

	. "github.com/onsi/gomega"

	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/inventory"
)

// TestManagerControllerOptions proves the scan serialization is stamped
// as a positive per-kind entry while everything else — the global
// concurrency default and other controllers' per-kind entries — survives
// untouched, and that the caller's map is never aliased.
func TestManagerControllerOptions(t *testing.T) {
	g := NewWithT(t)

	const deviceKind = "HardwareDevice.infrastructure.platform.opendatahub.io"
	base := ctrlconfig.Controller{
		MaxConcurrentReconciles: 8,
		GroupKindConcurrency: map[string]int{
			deviceKind: 4,
		},
	}

	got := inventory.ManagerControllerOptions(base)

	g.Expect(got.GroupKindConcurrency).To(HaveKeyWithValue(
		inventory.SingletonGroupKind.String(), 1))
	g.Expect(got.GroupKindConcurrency).To(HaveKeyWithValue(deviceKind, 4))
	g.Expect(got.MaxConcurrentReconciles).To(Equal(8))

	// Mutating the result must not leak into the caller's map.
	got.GroupKindConcurrency[inventory.SingletonGroupKind.String()] = 2
	g.Expect(base.GroupKindConcurrency).NotTo(HaveKey(
		inventory.SingletonGroupKind.String()))
}

// TestNewReconcilerRequiresSourceFactory proves registration fails fast
// on an unwired source factory, before any controller is created.
func TestNewReconcilerRequiresSourceFactory(t *testing.T) {
	g := NewWithT(t)

	f := newFixture(t, testSingleton())
	mgr := f.newManager()

	_, err := (&inventory.Controller{}).NewReconciler(context.Background(), mgr)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("source factory must not be nil"))
	g.Expect(mgr.runnables).To(BeEmpty())
}
