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

package device

import (
	"errors"
	"fmt"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrastructurev1alpha1 "github.com/lburgazzoli/opendatahub-hardware-discovery-operator/api/infrastructure/v1alpha1"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery/dra"
)

// recordObservation applies a successful observation to the device
// status in memory; the framework's status apply is the only writer, so
// a repeated identical apply is allowed and must not resample the
// collection time. It returns the success message and the next
// heartbeat requeue: the initial observation, changed
// facts/plane/provenance, or an age strictly beyond maxStale advances
// collectedAt from the source's collector clock and requeues for the
// full bound; an unchanged fresh observation keeps the persisted stamp
// and requeues for the remaining bound — promptly at the exact
// boundary, where a zero requeue would park the device until an input
// event. Generation changes and condition-only recovery never refresh
// facts.
func recordObservation(
	device *infrastructurev1alpha1.HardwareDevice,
	cfg config,
	identity discovery.Identity,
	observation discovery.Observation,
) (string, time.Duration) {
	facts := acceleratorStatus(observation.Facts)
	provenance := sourceRef(string(device.Spec.Source.Kind), identity.Locator)
	plane := infrastructurev1alpha1.AllocationPlane(identity.Plane)

	// Facts equality excludes collectedAt: timestamp sampling alone is
	// not a facts change.
	factsChanged := !apiequality.Semantic.DeepEqual(device.Status.Accelerator, facts) ||
		device.Status.AllocationPlane != plane ||
		device.Status.SourceRef != provenance

	// The heartbeat bound is evaluated on the collector clock: the new
	// observation stamp versus the persisted one.
	initial := device.Status.CollectedAt == nil
	age := time.Duration(0)
	if !initial {
		age = observation.CollectedAt.Sub(device.Status.CollectedAt.Time)
	}

	device.Status.Accelerator = facts
	device.Status.AllocationPlane = plane
	device.Status.SourceRef = provenance

	message := fmt.Sprintf("observed facts from source %q", device.Spec.Source.Kind)
	if initial || factsChanged || age > cfg.maxStale {
		device.Status.CollectedAt = &metav1.Time{Time: observation.CollectedAt}
		return message, cfg.maxStale
	}
	remaining := cfg.maxStale - age
	if remaining <= 0 {
		remaining = time.Second
	}
	return message, remaining
}

// classifyObserveError maps an observation failure to the condition
// reason (design doc §6.5). Classification uses errors.Is and the API
// status helpers, never message text.
func classifyObserveError(err error) string {
	switch {
	case apierrors.IsForbidden(err):
		return reasonSourceForbidden
	case errors.Is(err, dra.ErrPoolIncomplete):
		return reasonPoolIncomplete
	default:
		return reasonSourceUnavailable
	}
}

// acceleratorStatus maps observed facts to the accelerator status
// struct. Facts are replaced wholesale — absent facts are omitted,
// never zero or defaulted — and the struct is nil when no accelerator
// facts were observed.
func acceleratorStatus(facts discovery.Facts) *infrastructurev1alpha1.AcceleratorDeviceStatus {
	if facts.Accelerator == nil {
		return nil
	}
	status := &infrastructurev1alpha1.AcceleratorDeviceStatus{
		Vendor: facts.Accelerator.Vendor,
		Model:  facts.Accelerator.Model,
	}
	if facts.Accelerator.Memory != nil {
		memory := *facts.Accelerator.Memory
		status.Memory = &memory
	}
	return status
}
