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
	"errors"
	"testing"

	. "github.com/onsi/gomega"

	fwclient "github.com/opendatahub-io/odh-platform-utilities/framework/client"
	fwmanager "github.com/opendatahub-io/odh-platform-utilities/framework/manager"
)

// TestRegisterControllers proves the runtime registers exactly the two
// discovery controllers through their real framework-builder setup
// paths — the unstructured watch setup of both builders must succeed
// against the framework manager's cache-coherent client — and nothing
// else.
func TestRegisterControllers(t *testing.T) {
	g := NewWithT(t)

	stub := newManagerStub(t)
	manager := fwmanager.New(stub)

	g.Expect(RegisterControllers(manager)).To(Succeed())
	g.Expect(stub.runnables).To(HaveLen(2),
		"exactly the inventory and device controllers register, nothing else")
	g.Expect(manager.GetClient()).To(BeAssignableToTypeOf(&fwclient.Client{}),
		"both registrations run against the framework's cache-coherent client")
}

// TestRegisterControllersPropagatesErrors proves a manager that rejects
// a controller surfaces as a contextual registration error instead of a
// half-registered runtime.
func TestRegisterControllersPropagatesErrors(t *testing.T) {
	g := NewWithT(t)

	stub := newManagerStub(t)
	stub.addErr = errors.New("manager is shutting down")

	err := RegisterControllers(fwmanager.New(stub))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("registering inventory controller"))
	g.Expect(err.Error()).To(ContainSubstring("manager is shutting down"))
}
