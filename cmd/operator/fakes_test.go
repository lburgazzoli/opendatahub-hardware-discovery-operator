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

package operator

import (
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	ctrlmanager "sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlrecorder "sigs.k8s.io/controller-runtime/pkg/recorder"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
)

// managerStub satisfies the framework builder's manager needs offline:
// the fake cluster client, a separate API reader standing in for the
// manager's direct-read reader, the fixture scheme, a recording event
// recorder, a dummy REST config — the discovery and dynamic clients
// built from it are never dialed in tests — plus the registration
// surface the controller-runtime builder consumes. Fresh stubs are
// created per registration; controller name validation is skipped
// because controller-runtime's name registry is process-global and only
// releases the names of controllers that actually started and stopped.
type managerStub struct {
	ctrlmanager.Manager

	client           client.Client
	apiReader        client.Reader
	scheme           *runtime.Scheme
	recorder         ctrlrecorder.EventRecorder
	config           *rest.Config
	cache            cache.Cache
	controllerOption ctrlconfig.Controller
	logger           logr.Logger
	addErr           error
	runnables        []ctrlmanager.Runnable
}

func (m *managerStub) GetClient() client.Client                           { return m.client }
func (m *managerStub) GetAPIReader() client.Reader                        { return m.apiReader }
func (m *managerStub) GetScheme() *runtime.Scheme                         { return m.scheme }
func (m *managerStub) GetEventRecorder(string) ctrlrecorder.EventRecorder { return m.recorder }
func (m *managerStub) GetConfig() *rest.Config                            { return m.config }
func (m *managerStub) GetCache() cache.Cache                              { return m.cache }
func (m *managerStub) GetControllerOptions() ctrlconfig.Controller {
	return m.controllerOption
}
func (m *managerStub) GetLogger() logr.Logger { return m.logger }

func (m *managerStub) Add(runnable ctrlmanager.Runnable) error {
	if m.addErr != nil {
		return m.addErr
	}
	m.runnables = append(m.runnables, runnable)
	return nil
}

// newManagerStub builds the stub over an empty fake cluster with the
// service scheme.
func newManagerStub(t *testing.T) *managerStub {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(infrastructurev1alpha1.AddToScheme(scheme))

	cluster := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&infrastructurev1alpha1.HardwareDiscovery{},
			&infrastructurev1alpha1.HardwareDevice{},
		).
		Build()

	return &managerStub{
		client:    cluster,
		apiReader: cluster,
		scheme:    scheme,
		recorder:  &events.FakeRecorder{Events: make(chan string, 128)},
		config:    &rest.Config{Host: "https://127.0.0.1"},
		cache:     &informertest.FakeInformers{Scheme: scheme},
		controllerOption: ctrlconfig.Controller{
			SkipNameValidation: new(true),
		},
		logger: logr.Discard(),
	}
}

// singletonGroupKindKey is the per-kind concurrency key of the
// HardwareDiscovery singleton, computed independently of the production
// constant.
var singletonGroupKindKey = "HardwareDiscovery." + infrastructurev1alpha1.GroupVersion.Group
