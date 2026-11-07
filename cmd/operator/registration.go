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
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"

	devicecontroller "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/device"
	inventorycontroller "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/internal/controller/inventory"
)

// RegisterControllers registers both discovery controllers — and only
// those — through their framework-builder setup paths, so the runtime
// owns wiring, never business logic. One shared source factory instance
// is explicit in the construction of both controllers: it is a
// stateless configuration over the manager's API reader (scan-driven
// direct reads), not a process-local snapshot, and both controllers
// resolve devices through identically configured sources with no write
// access.
func RegisterControllers(mgr ctrl.Manager) error {
	sources := inventorycontroller.NewSourceFactory(mgr.GetAPIReader())

	if err := (&inventorycontroller.Controller{
		Sources: sources,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("operator: registering inventory controller: %w", err)
	}

	if err := (&devicecontroller.Controller{
		Sources: sources,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("operator: registering device controller: %w", err)
	}

	return nil
}
