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

package classic

import (
	"fmt"
	"math"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// GFD-style node label keys read by the classic source (design doc §9,
// amendment 3). These are the labels a real classic (ClusterPolicy/GFD)
// cluster publishes; the source reads exactly what the fleet provides.
const (
	// LabelGPUProduct is the node's GPU model label.
	LabelGPUProduct = "nvidia.com/gpu.product"

	// LabelGPUMemory is the node's per-GPU memory label, in MiB.
	LabelGPUMemory = "nvidia.com/gpu.memory"

	// LabelGPUCount is the node's GPU count label.
	LabelGPUCount = "nvidia.com/gpu.count"
)

// DefaultResourceName is the default extended resource whose presence in
// node allocatable or capacity is classic-plane evidence (the singleton's
// spec.sources.classic.resourceName can override it).
const DefaultResourceName = "nvidia.com/gpu"

// nodeGPUCount parses the node's gpu.count label. ok is false when the
// label is absent or malformed: a node without a parseable count is not
// claimable by this source — its inventory is unknown, never zero.
func nodeGPUCount(node *corev1.Node) (count int64, ok bool) {
	raw, present := node.Labels[LabelGPUCount]
	if !present {
		return 0, false
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed < 0 || parsed > math.MaxInt32 {
		return 0, false
	}
	return parsed, true
}

// extendedResourceEvidence reports whether the node publishes the
// configured classic extended resource in allocatable or capacity. The
// quantity is presence evidence only: it is never interpreted as
// remaining free GPUs, and the device count always comes from the labels.
func extendedResourceEvidence(node *corev1.Node, resourceName string) bool {
	_, inAllocatable := node.Status.Allocatable[corev1.ResourceName(resourceName)]
	_, inCapacity := node.Status.Capacity[corev1.ResourceName(resourceName)]
	return inAllocatable || inCapacity
}

// acceleratorFacts reads the node's homogeneous label facts (design doc
// §9): model from gpu.product, memory from gpu.memory MiB → bytes
// (BinarySI). GFD does not publish vendor, so it is omitted. Absent
// labels yield absent facts; a present but malformed memory label is an
// error — malformed input is unknown, never a silently zeroed fact.
func acceleratorFacts(node *corev1.Node) (discoveryFacts, error) {
	var facts discoveryFacts

	model, hasModel := node.Labels[LabelGPUProduct]
	if hasModel && model != "" {
		facts.model = model
	}

	if raw, present := node.Labels[LabelGPUMemory]; present && raw != "" {
		mib, err := parseMemoryMiB(raw)
		if err != nil {
			return discoveryFacts{}, err
		}
		memory := resource.NewQuantity(mib*1024*1024, resource.BinarySI)
		facts.memory = memory
	}

	return facts, nil
}

// parseMemoryMiB parses a gpu.memory label value (MiB) with explicit
// overflow handling: a value that cannot be represented in bytes as an
// int64 is malformed, not clamped.
func parseMemoryMiB(raw string) (int64, error) {
	mib, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || mib < 0 {
		return 0, fmt.Errorf("%w: %s=%q", ErrMalformedLabel, LabelGPUMemory, raw)
	}
	if mib > math.MaxInt64/(1024*1024) {
		return 0, fmt.Errorf("%w: %s=%q overflows bytes", ErrMalformedLabel, LabelGPUMemory, raw)
	}
	return mib, nil
}

// discoveryFacts is the intermediate label-facts holder; absent facts
// stay zero and are omitted when mapped to discovery.Facts.
type discoveryFacts struct {
	model  string
	memory *resource.Quantity
}

// present reports whether any fact was observed.
func (f discoveryFacts) present() bool {
	return f.model != "" || f.memory != nil
}
