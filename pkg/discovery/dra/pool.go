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

package dra

import (
	"cmp"
	"slices"

	resourcev1 "k8s.io/api/resource/v1"
)

// poolKey identifies one advertisement pool: (driver, pool.name).
type poolKey struct {
	driver string
	name   string
}

// poolSnapshot is the selected view of one pool: only
// the slices present at the highest generation, and whether they satisfy
// the pool's declared resourceSliceCount. Incomplete or conflicting
// snapshots are Unknown — no partial updates, no authoritative empty
// sets.
type poolSnapshot struct {
	driver         string
	name           string
	generation     int64
	sliceCount     int64
	countsConflict bool
	slices         []resourcev1.ResourceSlice
	complete       bool
}

// selectPools reduces a node's slices into per-pool snapshots. Only
// node-local advertisements (spec.nodeName == nodeName) from allow-listed
// drivers are considered: nodeSelector, allNodes and per-device node
// selection layouts are unsupported advertisement shapes and never create
// guessed device hosts. Input order does not matter; the returned map is
// freshly allocated.
func selectPools(collection []resourcev1.ResourceSlice, allowList []string, nodeName string) map[poolKey]poolSnapshot {
	allowed := driverSet(allowList)

	type accumulated struct {
		generation int64
		counts     map[int64]struct{} // declared counts seen at the highest generation
		slices     []resourcev1.ResourceSlice
	}
	pools := make(map[poolKey]*accumulated)

	for i := range collection {
		slice := &collection[i]
		if _, trusted := allowed[slice.Spec.Driver]; !trusted || !isNodeLocal(slice, nodeName) {
			continue
		}
		key := poolKey{driver: slice.Spec.Driver, name: slice.Spec.Pool.Name}
		pool, seen := pools[key]
		if !seen {
			pool = &accumulated{counts: map[int64]struct{}{}}
			pools[key] = pool
		}

		switch {
		case slice.Spec.Pool.Generation > pool.generation:
			// A higher generation invalidates everything seen before:
			// only the highest generation is ever read.
			pool.generation = slice.Spec.Pool.Generation
			pool.slices = []resourcev1.ResourceSlice{*slice}
			pool.counts = map[int64]struct{}{slice.Spec.Pool.ResourceSliceCount: {}}
		case slice.Spec.Pool.Generation == pool.generation:
			pool.slices = append(pool.slices, *slice)
			pool.counts[slice.Spec.Pool.ResourceSliceCount] = struct{}{}
		}
	}

	snapshots := make(map[poolKey]poolSnapshot, len(pools))
	for key, pool := range pools {
		// Slices are ordered by object name so a snapshot's devices are
		// deterministic regardless of input order.
		slices.SortFunc(pool.slices, func(a, b resourcev1.ResourceSlice) int {
			return cmp.Compare(a.Name, b.Name)
		})
		snapshot := poolSnapshot{
			driver:     key.driver,
			name:       key.name,
			generation: pool.generation,
			slices:     pool.slices,
		}
		declared, consistent := singleDeclaredCount(pool.counts)
		if consistent {
			snapshot.sliceCount = declared
		} else {
			snapshot.countsConflict = true
		}
		snapshot.complete = consistent && declared > 0 && int64(len(pool.slices)) == declared
		snapshots[key] = snapshot
	}
	return snapshots
}

// singleDeclaredCount returns the pool's declared resourceSliceCount at
// the highest generation. ok is false when the slices at that generation
// disagree: conflicting counts never yield a partial update.
func singleDeclaredCount(counts map[int64]struct{}) (declared int64, ok bool) {
	for count := range counts {
		if ok && count != declared {
			return 0, false
		}
		declared, ok = count, true
	}
	return declared, ok
}

// isNodeLocal reports whether the slice is a node-local advertisement for
// nodeName (spec.nodeName). Non-node-local layouts are unsupported
// advertisement shapes, not guessed hosts.
func isNodeLocal(slice *resourcev1.ResourceSlice, nodeName string) bool {
	return slice.Spec.NodeName != nil && *slice.Spec.NodeName == nodeName
}

// driverSet builds the allow-list lookup.
func driverSet(allowList []string) map[string]struct{} {
	set := make(map[string]struct{}, len(allowList))
	for _, driver := range allowList {
		set[driver] = struct{}{}
	}
	return set
}

// sortedPoolKeys returns the snapshot keys in a deterministic order so
// enumeration results never depend on map iteration order.
func sortedPoolKeys(snapshots map[poolKey]poolSnapshot) []poolKey {
	keys := make([]poolKey, 0, len(snapshots))
	for key := range snapshots {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b poolKey) int {
		if c := cmp.Compare(a.driver, b.driver); c != 0 {
			return c
		}
		return cmp.Compare(a.name, b.name)
	})
	return keys
}
