//go:build tools

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

// Package tools pins the approved direct dependencies required by later
// tasks (source adapters, manager/CLI runtime, Kind test helpers) in go.mod
// so parallel follow-up tasks do not each need to alter it. The file carries
// the `tools` build tag: it is compiled by nothing, it only keeps `go mod
// tidy` from dropping the requirements. Test-only dependencies (Gomega) are
// pinned by the real tests instead.
//
// The approved pin list is the task-001 execution contract
// (docs/design/poc-contract.md §2). Additions require a serialized, reviewed
// contract change, per the standing development rules.
package tools

import (
	_ "github.com/spf13/cobra"       // CLI runtime (cmd/, task 008)
	_ "k8s.io/api/resource/v1"       // stable DRA ResourceSlice types (task 005)
	_ "k8s.io/client-go/kubernetes"  // read-only source clients (tasks 004/005)
	_ "sigs.k8s.io/kind/pkg/cluster" // Kind harness provider (task 010)
	_ "sigs.k8s.io/yaml"             // simulator topology.yml parsing (task 004)
)
