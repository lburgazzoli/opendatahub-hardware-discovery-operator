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

package discovery

// Equal reports whether f and other carry the same facts. The comparison
// walks the per-class fact structs; in this PoC only accelerator facts
// exist. Memory compares numerically with Quantity.Equal, so equal values
// in different formats are equal, while an absent fact (nil) is distinct
// from a zero quantity: optional facts are omitted, never zero. The
// comparison never mutates the caller-owned Quantity values.
func (f Facts) Equal(other Facts) bool {
	switch {
	case f.Accelerator == nil || other.Accelerator == nil:
		return f.Accelerator == other.Accelerator
	default:
		return f.Accelerator.Equal(*other.Accelerator)
	}
}

// Equal reports whether a and b carry the same accelerator facts.
func (a AcceleratorFacts) Equal(b AcceleratorFacts) bool {
	if a.Vendor != b.Vendor || a.Model != b.Model {
		return false
	}
	switch {
	case a.Memory == nil || b.Memory == nil:
		return a.Memory == b.Memory
	default:
		return a.Memory.Equal(*b.Memory)
	}
}

// ObservationEqual reports whether a and b carry the same observable state
// — the same proven identity (device id and host UID), the same identity
// kind, the same allocation plane, the same source locator and the same
// facts — deliberately excluding CollectedAt: a freshly
// stamped identical observation is not a facts change. Heartbeat-driven
// status writes for unchanged observations are decided by the caller
// against its injected clock, not by this comparison.
func ObservationEqual(a, b Observation) bool {
	return a.Identity.ID == b.Identity.ID &&
		a.Identity.NodeUID == b.Identity.NodeUID &&
		a.Identity.Kind == b.Identity.Kind &&
		a.Identity.Plane == b.Identity.Plane &&
		locatorEqual(a.Identity.Locator, b.Identity.Locator) &&
		a.Facts.Equal(b.Facts)
}

// locatorEqual compares two locators nil-safely: a missing locator never
// equals a present one, and present locators decide via their own Equal.
func locatorEqual(a, b Locator) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(b)
}
