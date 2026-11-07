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

package device_test

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/device"
)

// TestNewReconcilerRequiresSourceFactory proves registration fails fast
// on an unwired source factory, before any controller is created.
func TestNewReconcilerRequiresSourceFactory(t *testing.T) {
	g := NewWithT(t)

	f := newFixture(t, testSingleton())
	mgr := f.newManager()

	_, err := (&device.Controller{}).NewReconciler(context.Background(), mgr)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("source factory must not be nil"))
	g.Expect(mgr.runnables).To(BeEmpty())
}

// TestSetupWithManagerPropagatesRegistrationErrors proves a manager
// that rejects the controller surfaces through SetupWithManager instead
// of being swallowed into a half-registered runtime.
func TestSetupWithManagerPropagatesRegistrationErrors(t *testing.T) {
	g := NewWithT(t)

	f := newFixture(t, testSingleton())
	f.draSource()
	mgr := f.newManager()
	mgr.addErr = errors.New("manager is shutting down")

	err := f.controller().SetupWithManager(mgr)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("manager is shutting down"))
	g.Expect(err.Error()).To(ContainSubstring("building framework reconciler"))
}
