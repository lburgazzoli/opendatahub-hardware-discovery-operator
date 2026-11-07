# Testing

This document records the testing philosophy, the executed verification
levels and their results, and the known limitations of what the suites
prove. It is written for a reader with a clean clone and no access to the
planning context.

## Levels

| Level | Command | Needs | What it proves |
|---|---|---|---|
| Unit | `make test` | nothing (no cluster, no kubeconfig, no network) | Controllers, codecs, identity resolution, lifecycle and observation semantics, package boundaries — with the race detector |
| Generation drift | `make verify` | nothing | Regenerated manifests/chart/RBAC match the committed state byte-for-byte |
| e2e compile | `make test-e2e-compile` | nothing | The e2e-tagged suite builds and vets |
| Two-plane demonstration | `make demo` | podman (or docker), ~4 GiB RAM | The Helm chart reconciling both simulator profiles on run-owned Kind clusters — the full scenario catalog below |
| Full e2e gate | `make test-e2e` | same | The demonstration plus the harness verification test (four cluster lifecycles total) |

`make verify` is the union of `manifests generate fmt vet lint test
drift-check` and is the gate every commit must pass.

## Unit-test philosophy

- **Observe behavior, not structure.** Controllers are exercised through
  interceptor-wrapped fake clients and real reconcile calls; no test
  inspects controller-runtime internals or a started manager's private
  state, and no composition test starts the framework builder.
- **Fixtures never share the implementation's oracle.** Expected
  identities, names and facts are derived independently in each test
  (for example, hash-truncated device names are recomputed from the
  documented derivation, not read from the production constant where the
  assertion would otherwise be circular).
- **No fixed sleeps.** Every asynchronous wait is a bounded `Eventually`
  poll whose timeout is a failure-detection bound, never a scheduling
  mechanism.
- **The core package stays API-independent.** `TestPackageBoundary`
  fails if `pkg/discovery` imports the API types; domain enums convert
  explicitly at the codec/controller boundaries.
- **Gomega matchers are composed.** Field-scoped assertions use jq
  matchers; typed and map-backed k8s objects are asserted through the
  composed k8s/jq matcher toolkit (`k8s.Conditions()`,
  `condition.Is`, `jq.Matchf` on the same elements). Map-backed
  conditions hold JSON strings: pass `string(metav1.ConditionTrue)`,
  not the typed enum, or `reflect.DeepEqual` silently never matches.

## The e2e demonstration (task 011)

`make demo` runs `TestHardwareDiscoveryDemonstration` — one run-owned
Kind cluster per profile, the simulator (Run:ai fake-gpu-operator, digest
`sha256:e73fc8c0…`, test-only) installed first, then the service Helm
chart from `dist/chart` with a locally built controller image loaded as
cargo (no registry push or pull). Every scenario asserts observable
cluster state; none reads controller internals.

Scenario catalog, per profile:

| Scenario | Classic | DRA |
|---|---|---|
| API validation (CEL rules, singleton name, duration patterns) | ✓ | ✓ |
| Chart defaults + minimal singleton (`kubectl`-shaped configure) | ✓ | ✓ |
| Discovery converges to the frozen simulator facts | ✓ | ✓ |
| No duplicate devices across sources/nodes | ✓ | ✓ |
| Spec identity immutability across observations | ✓ | ✓ |
| Heartbeat: unchanged facts refresh `collectedAt` within `maxStale` | ✓ | ✓ |
| Failed observation retains facts, never restamps `collectedAt` | ✓ (field removal) | — |
| Fact mutation restamps and republishes | — | ✓ |
| Incomplete pool: retention + `PoolIncomplete`, then recovery | — | ✓ |
| Selector prune and full reselect | ✓ | ✓ |
| Server version pinned + spec/status separation (main-resource update drops status) | ✓ | ✓ |
| Incarnation-convergence race | ✓ | — |
| Singleton deletion garbage-collects owned devices | ✓ | ✓ |
| Helm uninstall retains the CRDs | ✓ | ✓ |

### The incarnation-convergence race

The scenario stops the in-cluster controller, runs the production
controller registration in the test process on the chart's own
ServiceAccount token, and holds the first facts-carrying status patch
inside a wrapped transport. It then deletes the target device and
recreates it under the same name with a fresh uid, releases the held
patch, and proves the accepted status-write incarnation semantics on the
real API server:

- the stale apply is **accepted** (HTTP 200) — a custom-resource
  `/status` server-side apply carries no optimistic-concurrency
  precondition and the payload's uid is not a guard (verified by direct
  probe against the pinned 1.34.11 server: the identical stale-uid
  apply on a built-in kind is rejected with 422, on a CR it is not);
- the API server's own response witnesses the transfer — the new
  incarnation's uid carrying the stale facts;
- the recreated incarnation's own observation overwrites the transferred
  status within one pass — convergence, not atomicity — and the steady
  state never describes the stale facts.

This mirrors the unit-level contract
(`TestReconcileIncarnationRace`) and the design amendment recorded in
`docs/design/poc-contract.md` §6.3.

### Evidence

Each profile run writes a YAML evidence document under `dist/e2e/`
(gitignored): server version, nodes, devices with facts and conditions,
source snapshots, and the race outcome (held/recreated uids, response
code, transferred and converged models). On failure the harness collects
diagnostics (events, pod logs, node logs) under `/tmp` for the exact
run-owned cluster and tears only that cluster down.

## Executed results

Executed 2026-11-07 on the approved baseline — kind v0.33.0,
`kindest/node:v1.34.11`, podman 6.1.3 (rootless), Helm v4.3.1 (pinned
`go run`), Linux/amd64:

- `make demo` — PASS (classic 118s, DRA 142s)
- `make test-e2e` — PASS (demonstration + harness verification, 394s,
  four cluster lifecycles)
- `make verify` — PASS (all cluster-free checks incl. drift)

## What the suites do NOT prove

- **No physical hardware.** Every executed scenario runs against the
  fake-gpu-operator simulator on CPU-only Kind nodes. Production GPU
  identity, GFD/NFD label fidelity, vendor driver behavior and
  ResourceSlice advertisement shapes from real drivers are not tested.
- **No production classic discovery.** The classic source's synthetic
  `nodeIndex` identity is a deliberate shortcut: per-device identity on
  that plane is index-only because stock GFD/NFD labels are homogeneous
  per node and publish no per-GPU UUID.
- **No ODH-operator integration.** Module-lint success and the Helm
  demonstration are packaging and runtime evidence, not orchestrator
  onboarding; no opendatahub-operator handler, pin or module
  registration was executed.
- **No upgrade/migration.** The v1alpha1 schema is PoC-stage: changed
  schemas require a fresh deployment; no live-object migration,
  conversion webhook or management-state transition is tested or
  promised.
- **Not run:** scale/performance tests, multi-cluster operation,
  disruption/chaos scenarios beyond the deliberate incarnation race,
  and any check requiring real GPU nodes.
