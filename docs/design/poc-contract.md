# Hardware discovery service — PoC execution contract (proposal)

Status: **APPROVED — task 001 gate passed 2026-11-06; amended 2026-11-07.**
Date: 2026-11-06. Author: task 001 executor.

Amendment (2026-11-07, user-directed): HardwareDevice status uses a per-class
facts struct — `status.accelerator{vendor, model, memory}` — selected by the
immutable `spec.class` instead of flat accelerator-flavored top-level fields;
`pkg/discovery.Facts` mirrors the union (`Facts.Accelerator`). No status-side
class discriminator. Rationale: flat `status.memory` is accelerator-specific
and would be a breaking move when the `network` class arrives; the change was
made before any status-writing controller or consumer existed. Recorded in
the execution ledger.

Amendment 2 (2026-11-07, user-directed): **pluggable sources — no simulator
code in production.** The core `pkg/discovery` no longer enumerates source
kinds: `Locator` is an interface implemented by each source package, plane
resolution is a source-driven priority fold (each source owns its node-claim
rule; `PlaneEvidence` is deleted), and sources are registered in an ordered
registry. The CRD arm is renamed `simulator` → **`classic`**
(`spec.source.classic`, `spec.sources.classic`) after the allocation plane
it evidences. Recorded in the execution ledger.

Amendment 3 (2026-11-07, user-directed, supersedes the classic-source parts
of amendment 2): **classic discovery is a production source reading node
labels — the FGO ConfigMap concept is deleted entirely.** The controller
discovers accelerators on DRA nodes (ResourceSlices, per-device UUID) and on
pre-DRA/classic nodes (GFD-style node labels + node allocatable, both real
inputs that exist on today's fleet). Consequences: (a) the plan's
no-fabricated-identity rule is deliberately relaxed by user decision —
classic devices get a **synthetic node-scoped identity**
`spec.identity.id = <node-name>-gpu-<index>` (deterministic, ≤63 chars with
hash-truncation, dies with the node incarnation) and a new immutable
`spec.identity.kind: uuid|nodeIndex` distinguishes real vendor identity
from synthetic node-scoped identity; (b) the classic source locator is
`spec.source.classic{node, index}`; (c) the classic source is registered by
the production binary alongside DRA (default enabled; claims remain
evidence-gated); (d) the e2e classic scenario applies GFD-style labels as
scenario data — the controller reads exactly what a real classic cluster
publishes, with zero simulator awareness on either plane. The partial
ConfigMap-based classic adapter work is discarded. Recorded in the
execution ledger.

Amendment 4 (2026-11-07, user-directed, supersedes §2.3 and §6.4):
**framework adoption and the device Ready contract.** The service depends
on the published odh-platform-utilities **framework module v0.4.0**
(`github.com/opendatahub-io/odh-platform-utilities/framework`, tag
`framework/v0.4.0`, commit `fd6f202b`), a separate Go module from root
`v0.6.0`. Both CRDs implement `framework/api.PlatformObject`: each typed
status struct embeds `framework/api.Status` inline (`phase`,
`observedGeneration`, framework `api.Condition` conditions — an atomic list
with `severity`, a different type family from `metav1.Condition`). The
controllers are being migrated onto the framework reconciler and action
pipeline (§6.4); render/deploy/GC/finalizer actions are deliberately not
wired. **Device `Ready` is inventory readiness**: True only when the device
is eligible under current configuration and its recorded
identity/host/locator were successfully observed; False on failed
observation, missing/replaced host, deselection, disabled source or invalid
config — retaining prior facts and `collectedAt`; absent/Unknown before the
first evaluation; `Observed` remains as a diagnostic dependent. Consumers
must also check `collectedAt` freshness when the controller is stopped.
Normal framework status-SSA attempts on unchanged reconciliations are
accepted; collectedAt still advances only on initial observation, facts/
plane/provenance change or heartbeat expiry. This is a **direct v1alpha1
PoC schema change for fresh disposable deployments**: no live-object
migration, conversion webhook or production upgrade path is provided, and
no new management-state behavior is claimed. Recorded in the execution
ledger.

Amendment 5 (2026-11-07, user-directed, extends Amendment 4):
**framework main pin and mandatory builder registration.** The framework
dependency moves from the published `framework/v0.4.0` tag to the
**immutable main snapshot
`v0.4.1-0.20261105151519-69e52f844d32`** (commit
`69e52f844d3209424d3c3d79910a25a304233b75`), gaining the
`ReconcilerFor → ReconcilerBuilder → Build(ctx)` registration path.
Both controllers must register through that builder path — production
code never calls `ctrl.NewControllerManagedBy` or the native
controller-runtime builder chain directly (the framework's internal use
of them inside its `Build` is framework-owned). Scan serialization is
expressed through the manager's `Controller` options: a positive
`GroupKindConcurrency` entry keyed
`"HardwareDiscovery.infrastructure.platform.opendatahub.io"` (verified
**[source-inspected]** at controller-runtime `v0.25.2`: `doController`
applies a positive per-kind entry before the global
`MaxConcurrentReconciles` default), so the device controller keeps the
global default. The event recorder is obtained as
`mgr.GetEventRecorder(<controller name>)` (events.k8s.io). This
anticipates the future ODH-operator module integration consuming the
same framework. Recorded in the execution ledger.

This document is the approved concrete PoC contract governing all service
implementation (tasks 002+). The human approved it as proposed on 2026-11-06
in the execution session, resolving the gate choices as recorded in §1 —
notably the project name **`lburgazzoli/opendatahub-hardware-discovery-operator`**
and module path `github.com/lburgazzoli/opendatahub-hardware-discovery-operator`.
Evidence is labeled **[executed]**, **[source-inspected]**, or **[not-run]**;
see `.context/plans/hardware-discovery-service/decisions.md` for the raw commands.

---

## 1. Gate decisions (all resolved by human approval, 2026-11-06)

| # | Choice | Resolution |
|---|--------|------------|
| D1 | Go module path | `github.com/lburgazzoli/opendatahub-hardware-discovery-operator` — project name `lburgazzoli/opendatahub-hardware-discovery-operator`; baked into Kubebuilder scaffolding (PROJECT/go.mod) at task 002 |
| D2 | Singleton kind + fixed instance name | kind `HardwareDiscovery`, group `infrastructure.platform.opendatahub.io`, v1alpha1, plural `hardwarediscoveries`, fixed instance name `cluster` |
| D3 | controller-runtime patch level | `v0.25.2` (latest 0.25.x, published 2026-09-30) |
| D4 | golangci-lint pin | `v2.14.0` (first release validated against Go 1.27.x stdlib; `v2.13.x` fails with Go 1.27.2+ export data — observed by FGO upstream, issue #280) |
| D5 | Grace-bookkeeping annotation key | `hardwarediscovery.platform.opendatahub.io/node-orphaned-at` on the HardwareDevice object (see §10) |
| D6 | Repo/Git workspace | Git initialized in the working directory on human direction; task-001 commit created (design document only, `.context/` uncommitted) |

All other content of this document was approved as proposed.

---

## 2. Identity, dependency and tool pins

### 2.1 API identity (proposed)

| Item | Value |
|---|---|
| Group (both CRDs) | `infrastructure.platform.opendatahub.io` |
| Version | `v1alpha1` (served + storage; exactly one storage version) |
| Device CRD | `HardwareDevice`, plural `hardwaredevices`, short names `hwdev`, `hw`, categories `odh`, `platform`, `all`, cluster-scoped, status subresource |
| Singleton CRD | `HardwareDiscovery`, plural `hardwarediscovery`, no short names, cluster-scoped, status subresource |
| Singleton instance | exactly one object named `cluster`, enforced by a root-schema CEL rule (validated **[executed]**, §14) |
| Kubebuilder domain/group decomposition | domain `platform.opendatahub.io`, group `infrastructure` → API package `api/infrastructure/v1alpha1` (multigroup layout per standing rules) |

### 2.2 Tool pins (all verified published **[executed]** via proxy.golang.org unless noted)

| Tool | Pin | Verification |
|---|---|---|
| Kubebuilder | `sigs.k8s.io/kubebuilder/v4@v4.16.0` | module downloaded; contains `go/v4` and `helm/v2-alpha` (`pkg/plugins/optional/helm/v2alpha`, plugin version 2-alpha); scaffolds controller-runtime `v0.25.0`, controller-tools `v0.22.0`, kustomize `v5.8.1`, golangci-lint `v2.13.1`, `go 1.26.0`, and a main.go that already supports `--webhook-port=-1` |
| controller-runtime | `v0.25.2` | published; pulls `k8s.io/{api,apimachinery,client-go} v0.37.0`, `go 1.26.0` |
| controller-tools (controller-gen) | `v0.22.0` (Kubebuilder pin) | published |
| Kustomize | `v5.8.1` (Kubebuilder pin) | published |
| Helm CLI | `go run helm.sh/helm/v4/cmd/helm@v4.3.1` | published; local `helm` binary is v4.3.1 and was used for chart verification |
| golangci-lint | `v2.14.0` (D4) | published |
| Go | directive `go 1.26.0` (scaffold baseline), toolchain `go1.27.3` (installed) | — |
| Kind | `sigs.k8s.io/kind@v0.33.0` (CLI + Go library for the harness) | published 2026-08-26; ships node images through `kindest/node:v1.37.0` |
| Kind node image | `kindest/node:v1.34.11` | **[executed]** serves `resource.k8s.io/v1` (DRA GA since Kubernetes 1.34, default-on) |
| Gomega | `v1.44.0` | published (DB harness uses v1.42.1; we take current) |
| gomega-matchers | `github.com/lburgazzoli/gomega-matchers v0.6.0` | published; jq/k8s/condition matchers used by controller tests |
| odh-platform-utilities framework | `github.com/opendatahub-io/odh-platform-utilities/framework v0.4.1-0.20261105151519-69e52f844d32` (commit `69e52f844d3209424d3c3d79910a25a304233b75`, main snapshot superseding the `framework/v0.4.0` tag of amendment 4) | published **[executed]** via proxy.golang.org (amendment 5); separate module from root v0.6.0; requires k8s.io `v0.37.1` (compatible patch raise from v0.37.0) and controller-runtime `v0.25.1` (we keep `v0.25.2`); promotes `go-logr` to a direct dependency |
| odh-platform-tools (`make module-lint`) | `github.com/lburgazzoli/odh-platform-tools@v0.0.0-20261028215308-948ecd9b3ace` (immutable pseudo-version; repo has no tags) | proxy `@latest` resolves to exactly the inspected commit 948ecd9b |

### 2.3 Framework dependency decision

**Superseded by amendment 4 (2026-11-07).** The original gate decision
rejected a Go dependency on `odh-platform-utilities`/`odh-platform-operator`
internals because root `v0.6.0` pinned controller-runtime `v0.23.3` /
k8s.io `v0.35.x` (two minors behind our baseline) and its platform
`common.Condition` was not `metav1.Condition`. The published **framework
module `v0.4.0`** removes both objections: it pins controller-runtime
`v0.25.1` / k8s.io `v0.37.1` — compatible with our `v0.25.2`/`v0.37.x`
baseline (MVS keeps our controller-runtime and raises Kubernetes patch
versions to `v0.37.1`) — and `framework/api` defines the platform contract
(`PlatformObject`, `Status`, `Condition`) directly. The user directed
adoption: both CRDs are `PlatformObject`s and the controllers run the
framework reconciler/action pipeline (no render/deploy/GC/finalizer
actions). The DB-operator harness remains a pattern reference re-implemented
in our own test tree; root-module `odh-platform-operator` internals remain
out of bounds, and root-module convenience imports are not used — simple
types come from `framework/api`.

---

## 3. Package map (proposed)

```
opendatahub-hardware-discovery-operator/
├── cmd/
│   ├── main.go                     # thin Cobra entry
│   └── operator/operator.go        # manager wiring, flags, probes
├── api/infrastructure/v1alpha1/    # both types + groupversion (Kubebuilder multigroup)
├── internal/controller/
│   ├── hardwarediscovery/          # singleton/module controller (identity, spec, lifecycle)
│   └── hardwaredevice/             # per-device controller (status-only writer)
├── pkg/discovery/                  # core normalization; NO controller/CLI imports
│   ├── source.go                   # Source interface, DTOs, Outcome enum, ordered source registry
│   ├── locator.go                  # Locator interface (Kind/Validate/Equal/Ref)
│   ├── identity.go                 # name derivation, sanitization, collision guards
│   ├── facts.go                    # Facts, equality (excludes timestamps)
│   └── plane.go                    # source-driven node-claim fold (SelectNode)
├── pkg/discovery/dra/              # stable resource.k8s.io/v1 NVIDIA adapter
│                                   #   (production source; DRALocator, claim rule,
│                                   #    pool completeness, uuid identity)
├── pkg/discovery/classic/          # classic labels source (production; GFD-style
│                                   #   node labels + allocatable, nodeIndex identity)
├── test/support/cluster/           # task 010: Kind lifecycle (Instance/Config/Setup/Stop)
├── test/e2e/                       # task 010/011: two-plane suite (build tag)
├── config/                         # Kustomize generation input (scaffold output)
├── dist/chart/                     # generated Helm chart (kubebuilder helm/v2-alpha default)
├── packaging/module.yaml           # PlatformModuleDefinition metadata (§12)
├── make/{e2e,helm}.mk              # task-owned make fragments
└── docs/
```

Boundaries: `pkg/discovery` imports only k8s.io/apimachinery + stdlib;
adapters (`dra`, `classic`) import `pkg/discovery` and client-go read
interfaces; controllers import `pkg/discovery`. Nothing in `pkg/discovery`
imports `internal/controller`, `cmd`, or client-go write paths. The
simulator's private details (ConfigMap names, YAML keys) exist only in
`pkg/discovery/classic`.

---

## 4. HardwareDevice API contract

### 4.1 Spec

| JSON path | Type | Required | Validation | Notes |
|---|---|---|---|---|
| `spec.class` | string | yes | enum `[accelerator]`; CEL `self == oldSelf` (immutable) | sole class in the PoC; additive growth = new enum values + new optional sibling structs, never a catch-all |
| `spec.identity.kind` | string | yes | enum `[uuid, nodeIndex]`; CEL `self == oldSelf` | honesty field: `uuid` = real vendor identity (DRA); `nodeIndex` = synthetic node-scoped identity (classic labels) — not joinable with telemetry/DCGM, index-based |
| `spec.identity.id` | string | yes | minLength 1, maxLength 63; CEL `self == oldSelf` | vendor device ID (GPU UUID) for `kind: uuid`; deterministic `<node-name>-gpu-<index>` (hash-truncated to 63) for `kind: nodeIndex`; never derived from list indexes of an unbounded source |
| `spec.host.name` | string | yes | minLength 1 | Node name at discovery time |
| `spec.host.uid` | string | yes | minLength 1; CEL `self == oldSelf` | Node UID; node replacement = delete + recreate (§11), never an in-place UID transfer |
| `spec.source.kind` | string | yes | enum `[dra, classic]`; must match the set arm (CEL) | registered source kind that owns this locator; the production binary registers `dra` only — `classic` exists for the classic-plane test source (§9) |
| `spec.source.dra` | object | exactly one of `dra`/`classic` (CEL), matching `kind` | see below | DRA locator |
| `spec.source.dra.driver` | string | yes | enum-extended allow-list, default pin `gpu.nvidia.com` | |
| `spec.source.dra.pool` | string | yes | minLength 1 | ResourceSlice `pool.name` |
| `spec.source.dra.device` | string | yes | minLength 1 | ResourceSlice `device.name` |
| `spec.source.classic.node` | string | yes | equals `spec.host.name` (CEL) | the Node whose labels describe this device |
| `spec.source.classic.index` | int32 | yes | minimum 0 | the device's index within the node's `gpu.count`; stable for the node incarnation |

### 4.2 Status

| JSON path | Type | Notes |
|---|---|---|
| `status.accelerator` | object | per-class facts struct, present iff facts observed for a `spec.class: accelerator` device (root CEL: `!has(self.status.accelerator) \|\| self.spec.class == 'accelerator'`); future classes arrive as sibling structs — class-specific facts never live at the status top level |
| `status.accelerator.vendor` | string | omitted when the source does not publish it (PoC: neither FGO CMs nor NVIDIA DRA attributes carry vendor — omitted, never defaulted) |
| `status.accelerator.model` | string | FGO `gpuProduct` / DRA `gpu.nvidia.com/productName` |
| `status.accelerator.memory` | `resource.Quantity` | pattern `^([+-]?[0-9.]+)([eEinumkKMGTP]*[-+]?[0-9]*)$`; FGO MiB → bytes, BinarySI |
| `status.allocationPlane` | string, enum `[classic, dra]` | which plane evidenced the device |
| `status.collectedAt` | `metav1.Time` | collector observation clock; never copied from the producer |
| `status.sourceRef` | string | provenance, e.g. `dra/gpu.nvidia.com/<pool>` or `classic/<node>`; replaced, not merged, on observed source change |
| `status.phase` | string | embedded framework `api.Status` block (amendment 4): framework-derived phase |
| `status.observedGeneration` | int64 | embedded framework `api.Status` block: last successfully observed generation under framework semantics |
| `status.conditions` | `[]framework/api.Condition` | embedded framework `api.Status` block: **atomic list** (not the previous map-keyed `metav1.Condition` list) whose items add `severity` and `lastHeartbeatTime`; carries the device `Observed` diagnostic condition and, after the controller migration, the framework-aggregated `Ready` and `ProvisioningSucceeded` dependents |

`Ready` is **inventory readiness** (amendment 4): True only when the device
is eligible under current configuration and its recorded
identity/host/locator were successfully observed; False on failed
observation, missing/replaced host, deselection, disabled source or invalid
config, with prior facts and `collectedAt` retained; absent/Unknown before
the first evaluation. It is **not** hardware health, free allocation
capacity, workload suitability or validation; a stopped controller cannot
retract a stored True — consumers must also check `collectedAt` freshness.

Optional unknown facts are omitted — no zero/defaulted facts, no placeholder
structs, no vendor attribute bag. There is **no status-side class
discriminator**: `spec.class` is immutable (CEL) and therefore *is* the
discriminator selecting the per-class facts struct; duplicating it into
status would only invite drift.

Printer columns: `Class` (.spec.class), `Model` (.status.accelerator.model),
`Memory` (.status.accelerator.memory), `Node` (.spec.host.name), `Plane`
(.status.allocationPlane), `Collected` (.status.collectedAt), `Age`.

### 4.3 Example CR

```yaml
apiVersion: infrastructure.platform.opendatahub.io/v1alpha1
kind: HardwareDevice
metadata:
  name: gpu-11111111-2222-3333-4444-555555555555
  labels:
    hardwarediscovery.platform.opendatahub.io/device-class: accelerator
    hardwarediscovery.platform.opendatahub.io/node: gpu-node-a
  ownerReferences:
  - apiVersion: infrastructure.platform.opendatahub.io/v1alpha1
    kind: HardwareDiscovery
    name: cluster
    uid: <singleton-uid>
    controller: true
    blockOwnerDeletion: true
spec:
  class: accelerator
  identity:
    id: GPU-11111111-2222-3333-4444-555555555555
  host:
    name: gpu-node-a
    uid: 11111111-1111-1111-1111-111111111111
  source:
    kind: dra
    dra:
      driver: gpu.nvidia.com
      pool: gpu-node-a
      device: gpu-11111111-2222-3333-4444-555555555555
status:
  observedGeneration: 1
  phase: Ready
  accelerator:
    model: Tesla-K80
    memory: 12635713536
  allocationPlane: dra
  collectedAt: "2026-11-06T15:32:31Z"
  sourceRef: dra/gpu.nvidia.com/gpu-node-a
  conditions:
  - type: Observed
    status: "True"
    reason: Observed
    observedGeneration: 1
    lastTransitionTime: "2026-11-06T15:32:31Z"
```

### 4.4 Naming, labels, collision and truncation policy

- Object name = deterministic function of `identity.id` only (not the host):
  lowercase, invalid chars → `-`, collapse runs, trim; if the result is not a
  legal DNS-1123 subdomain (≤253) or collides with a *different* raw
  identity, append `-<sha256(identity.id)[:8]>`. Determinism is unit-tested;
  the controller never invents UUIDs.
- Name collision with a **foreign** object (exists, not owned by the
  singleton, or owned but different `identity.id`): never adopt, never
  overwrite; report a singleton `Degraded` condition (`NameCollision`) and
  skip creating that device. Collision resolution is a human action.
- Legal label set (service-written only; prefix
  `hardwarediscovery.platform.opendatahub.io/`):
  - `device-class: accelerator` (mirror of `spec.class` for consumer
    selection; value is an enum, ≤63 chars by construction);
  - `node: <node-name>` — label **values** are limited to 63 chars while
    node names may reach 253: if the node name is not a legal label value,
    use `<first-52-chars>-<sha256(node-name)[:10]>`. The truncation is
    deterministic and unit-tested; the full node name always remains in
    `spec.host.name`. The label is an index, never an identity source.
- No Node owner reference, ever; ownership is exclusively the singleton
  ownerRef above (real GC then handles singleton deletion).

### 4.5 Additive extension path (no placeholders now)

Future optional facts (PCIe/NUMA location, MIG geometry, relations, vendor
attributes) are added as new *optional* fields inside the per-class status
struct with their own validation, plus new enum values for `spec.class` and
new sibling class structs (e.g. a future `status.network`) gated by the same
root CEL presence rule. Because every current field is
required-and-validated or explicitly optional with `omitempty`, additions
are non-breaking; no empty bag/map ships in v1alpha1.
A future PCI identity locator would extend the `spec.source` union — the CEL
"exactly one" rule is written as `oneOf`-style counting so adding a third
arm is a reviewed schema change, not a silent reopening.

---

## 5. HardwareDiscovery (singleton) API contract

### 5.1 Spec

| JSON path | Type | Default | Notes |
|---|---|---|---|
| `spec.class` | string, enum `[accelerator]` | required | single class; CEL `self == oldSelf` |
| `spec.nodeSelector` | `metav1.LabelSelector` | omitted/empty = all eligible nodes | standard selector; changes never require webhooks (validated **[executed]** — empty selector accepted, `matchLabels`/`matchExpressions` structural validation active) |
| `spec.maxStale` | `metav1.Duration` | `10m` | heartbeat bound for unchanged facts; pattern `^([0-9]+(s|m|h))+$` |
| `spec.rescanPeriod` | `metav1.Duration` | `5m` | bounded periodic rescan even without input events |
| `spec.nodeGracePeriod` | `metav1.Duration` | `15m` | orphan grace for absent/UID-changed nodes |
| `spec.sources.dra.enabled` | bool | `true` | stable DRA source — the only source registered by the production binary |
| `spec.sources.dra.driverAllowList` | []string | `["gpu.nvidia.com"]` | whole-GPU advertisements only from these drivers |
| `spec.sources.classic.enabled` | bool | `true` | classic labels source — production source, registered alongside DRA; claims remain evidence-gated (extended-resource + GFD labels present), so enabling it never fabricates devices on non-GPU nodes |
| `spec.sources.classic.resourceName` | string | `nvidia.com/gpu` | the extended resource whose presence in node allocatable/capacity is classic-plane evidence |

Root-schema CEL (validated **[executed]**):
`self.metadata.name == 'cluster'` — the singleton cannot be created under
any other name; no webhook needed. Deleting and recreating `cluster` is the
documented reset path.

### 5.2 Status

| JSON path | Type | Notes |
|---|---|---|
| `status.phase` | string | embedded framework `api.Status` block (amendment 4): framework-derived phase |
| `status.observedGeneration` | int64 | embedded framework `api.Status` block: last successfully observed generation under framework semantics |
| `status.conditions` | `[]framework/api.Condition` | embedded framework `api.Status` block (atomic list, items add `severity`): collector/observation health — **not** hardware health, **not** free-capacity (types: `Ready`, `Degraded`; reasons: `Reconciled`, `SourcesUnavailable`, `SourceForbidden`, `PoolIncomplete`, `NameCollision`, `IdentityAmbiguous`, `InvalidConfiguration`) |
| `status.nodeSummaries` | []struct | per selected node: `name`, `uid`, `plane`, `deviceCount`, `outcome` — compact inventory overview for the demo query and future ODH consumption |

### 5.3 Example CR

```yaml
apiVersion: infrastructure.platform.opendatahub.io/v1alpha1
kind: HardwareDiscovery
metadata:
  name: cluster
spec:
  class: accelerator
  nodeSelector:
    matchLabels:
      run.ai/simulated-gpu-node-pool: default
  maxStale: 10m
  rescanPeriod: 5m
  nodeGracePeriod: 15m
  sources:
    dra:
      enabled: true
      driverAllowList: [gpu.nvidia.com]
    classic:
      enabled: true
```

### 5.4 Per-fact demo query/display

Each device fact names its demo display: `model`/`memory` →
`kubectl get hwdev` printer columns (demo asserts values for explicit
source IDs); `allocationPlane` → printer column + `status.nodeSummaries`;
provenance (`sourceRef`, `collectedAt`) → `kubectl describe hwdev` and the
e2e assertions; ownership/lifecycle → ownerRef + pruning demos.

---

## 6. Architecture

### 6.1 Dependency diagram

```
                 ┌────────────────────────────────────────────────┐
                 │ cmd/operator (manager, leader election, probes) │
                 └───────────────┬────────────────────────────────┘
                                 │ registers
              ┌──────────────────┴───────────────────┐
              ▼                                      ▼
 ┌─────────────────────────┐          ┌──────────────────────────────┐
 │ internal/controller/     │          │ internal/controller/         │
 │ hardwarediscovery        │          │ hardwaredevice               │
 │ (singleton: spec writer, │          │ (status-only writer via      │
 │  inventory, prune, grace)│          │  /status subresource)        │
 └───────────┬─────────────┘          └──────────────┬───────────────┘
             │ reads/writes owned       │ reads spec.source locator,
             │ HardwareDevice spec      │ observes, writes /status
             ▼                          ▼
      ┌─────────────────────────────────────────────┐
      │ pkg/discovery (core: Source, DTOs, identity,│
      │ facts, plane rules; no k8s write clients)   │
      └───────┬───────────────────────────┬─────────┘
              ▼                           ▼
   ┌─────────────────────┐     ┌──────────────────────────┐
   │ pkg/discovery/dra   │     │ pkg/discovery/classic    │
   │ resource.k8s.io/v1  │     │ Node labels + allocatable│
   │ read-only client    │     │ (production source)      │
   └─────────────────────┘     └──────────────────────────┘

  External read-only inputs: Nodes, ResourceSlices/DeviceClasses
  (resource.k8s.io/v1).
 External writes: HardwareDevice (spec, singleton controller only),
 HardwareDevice/status (device controller only), HardwareDiscovery/status,
 Lease (leader election), Events.
```

### 6.2 Read/write ownership matrix

| Resource | singleton ctrl | device ctrl | sources | consumers |
|---|---|---|---|---|
| HardwareDiscovery | read spec, write status (framework status SSA after the migration) | read spec | — | — |
| HardwareDiscovery/status | write (framework `ApplyStatus`) | — | — | read |
| HardwareDevice | create/delete, write spec | read spec; **never** any primary write | — | get/list/watch |
| HardwareDevice/status | read | **only writer** (framework `ApplyStatus`; never the primary endpoint, including metadata/finalizers) | — | read |
| Node | read (get/list/watch) | read | read | — |
| ResourceSlice/DeviceClass | read (scan, guarded) | read (observe) | read-only | — |
| Lease / Event | write (as used) | write Events | — | — |

### 6.3 Creation / observation / deletion sequences

Creation (singleton controller, per scan):
1. Resolve selected nodes (label selector in reconcile logic, not cache
   filtering, so deselection and new matches stay observable).
2. For each node, run plane resolution (§8.2): DRA first, classic with
   both extended-resource evidence and GFD-style labels.
3. Source `Enumerate` → identities + locators; outcome `Success` (possibly
   empty) vs `Unknown` (§8.4).
4. For each identity: derive name (§4.4); if absent → create with ownerRef,
   spec as §4.1; if present-and-owned-and-same-identity → ensure spec
   matches (no unconditional writes); if foreign/colliding → condition, skip.
5. Prune owned devices absent from an authoritative (Success) enumeration of
   their node — subject to §11 grace.

Observation (device controller, per device):
1. Read own spec (identity, host, source locator).
2. `Observe(locator)` → Facts + plane, stamped with the collector clock.
3. Compare facts (equality excludes `collectedAt`); write /status only if
   facts/plane/sourceRef changed **or** `collectedAt` is older than
   `maxStale` at observation time (heartbeat).
4. `Observe` errors surface as device conditions (`SourceUnavailable`) and
   never delete facts; NotFound of the device itself is reconcile success.

Status-write incarnation semantics (amendment, 2026-11-07, user-directed):
the framework persists device status through server-side apply without an
optimistic-concurrency precondition, so a stale-copy observation can
transiently land on a same-name replacement created while an observation
was in flight. This is accepted eventual consistency, not a bug: the
replacement's creation reconciles within one pass and overwrites the
transferred status with its own observation or its failure condition;
same-name recreation is same-identity by the name-resolution design; and
every reconcile revalidates the spec identity against the live cluster
(host UID, class, source enablement, locator) before observing. The
steady-state guarantee — status describes the last evaluation of the
current incarnation — is upheld by convergence, not by an atomic
per-write guard.

Deletion:
- Authoritative disappearance (successful empty enumeration) → prune owned
  objects (after §11 grace when the trigger is node absence/UID change).
- User removes class/selector coverage → those nodes' owned devices pruned.
- Singleton deletion → real GC via ownerRef removes children; no finalizers.
- Foreign objects are never touched.

### 6.4 Platform status compatibility (amendment 4)

Both CRDs implement `framework/api.PlatformObject`: the typed status
structs embed `framework/api.Status` inline, so `phase`,
`observedGeneration` and the framework `api.Condition` list (atomic, with
`severity`) live at the usual status paths while the typed discovery data
(accelerator facts, allocationPlane, collectedAt, sourceRef,
nodeSummaries) keeps its own JSON paths. The controllers run the framework
reconciler/action pipeline; the framework performs status server-side
apply after the actions, and normal SSA attempts on unchanged
reconciliations are accepted. There are no shadow duplicate
`Conditions`/`ObservedGeneration` fields. Future ODH orchestration
consumption reads our `status.conditions` by type/reason string and
`status.nodeSummaries`; no runlevel/ManagementState integration is claimed.
The previous explicit "no framework dependency, metav1-only" resolution
(§2.3) is superseded.

### 6.5 Failure-state table

| Failure | Detection | Inventory effect | Condition |
|---|---|---|---|
| resource.k8s.io/v1 not served / Forbidden | discovery check + API error on scan | retain all facts+collectedAt; plane unknown | `SourcesUnavailable`/`SourceForbidden` |
| Partial new pool generation | slice count < `pool.resourceSliceCount` at max generation | retain; **no** partial update, no fallback | `PoolIncomplete` |
| Successful empty enumeration | `Outcome=Success`, zero identities for node | prune owned devices of that node | — (Ready) |
| Simulator disabled / classic source disabled by configuration | `spec.sources.classic.enabled=false` | source not registered; DRA-only discovery | — (deliberate, not a failure) |
| Duplicate/ambiguous identity (same ID twice, or same ID on two live nodes) | enumeration guard | reject both; no object created/updated | `IdentityAmbiguous` |
| Name collision with foreign object | name derivation | skip creation | `NameCollision` |
| Node absent or UID mismatch | node cache/read | retain during grace, then prune | node summary `outcome=Unknown`→pruned |
| Source published fact change | Observe diff | update facts + collectedAt | — |
| Source temporarily unreadable for one device | Observe error | keep last facts; condition only | device `SourceUnavailable` |

---

## 7. Source DTO contract (frozen at this gate)

All types live in `pkg/discovery` (core) and the source packages; adapters
never construct `HardwareDevice` Kubernetes objects — the mapping to
`HardwareDeviceStatus` happens once, in the device controller. **The core is
source-agnostic (amendments 2+3): it never enumerates source kinds, planes
or input types.**

```go
// Plane is the allocation plane that evidenced a device.
type Plane string // "classic" | "dra"

// IdentityKind distinguishes real vendor identity from synthetic
// node-scoped identity (amendment 3): "uuid" (DRA) vs "nodeIndex"
// (classic labels). It is persisted as spec.identity.kind.
type IdentityKind string

// Outcome distinguishes authoritative results from unknown ones.
type Outcome string
const (
    OutcomeSuccess Outcome = "Success" // authoritative snapshot; may be empty
    OutcomeUnknown Outcome = "Unknown" // error / missing API / forbidden / incomplete pool
)

// Locator is the pluggable, source-owned locator stored in spec.source.
// Each source package implements it for its own locator shape; the core
// never enumerates kinds.
type Locator interface {
    // Kind is the registered source kind ("dra", "classic", ...); it must
    // match the spec.source.kind arm that carries this locator.
    Kind() string
    // Validate checks the locator's required fields.
    Validate() error
    // Equal reports deep equality; it never mutates the caller's values.
    Equal(Locator) bool
    // Ref renders the locator's provenance suffix, e.g.
    // "gpu.nvidia.com/<pool>" or "<node>".
    Ref() string
}

// Identity is a discovered device identity plus how to re-observe it.
type Identity struct {
    ID       string       // vendor UUID (kind uuid) or <node>-gpu-<index> (kind nodeIndex)
    Kind     IdentityKind // uuid | nodeIndex
    NodeName string
    NodeUID  types.UID
    Plane    Plane
    Locator  Locator
}

// Facts are the minimal supported observations (per-class union; absent =
// omitted, never zero).
type Facts struct {
    Accelerator *AcceleratorFacts
}
type AcceleratorFacts struct {
    Vendor string
    Model  string
    Memory *resource.Quantity
}

// Enumeration is the result of enumerating one node from one source.
type Enumeration struct {
    Outcome    Outcome
    Identities []Identity // empty + Success is authoritative
    Err        error     // set iff Outcome == Unknown; classified
}

// Observation is a successful single-device observation.
type Observation struct {
    Identity    Identity
    Facts       Facts
    CollectedAt time.Time // collector clock, stamped at observation
}

// Source is a discovery source plugin. Each implementation owns its
// locator type, its input reading (via injected read-only clients), its
// node-claim rule and its facts parsing; the core folds claiming sources
// by priority.
type Source interface {
    ID() string
    Plane() Plane
    // ClaimsNode reports whether this source claims the node for
    // discovery. The claim rule is source knowledge: the DRA source
    // claims nodes with allow-listed advertisements (even while
    // incomplete); the classic source claims nodes with classic
    // extended-resource evidence plus GFD-style labels. Unclaimed nodes
    // are unknown for this source — never an authoritative empty set.
    ClaimsNode(ctx context.Context, node NodeInfo) bool
    // Enumerate: identities for the given nodes. Empty+Success prunes;
    // Unknown preserves all prior state.
    Enumerate(ctx context.Context, nodes []NodeInfo) (Enumeration, error)
    // Observe: resolve one previously enumerated identity into facts.
    Observe(ctx context.Context, id Identity) (Observation, error)
}

// Sources is the ordered source registry (priority = order; DRA registers
// before classic so DRA claims win). Duplicate kinds are rejected at
// registration.
type Sources struct{ /* ordered []Source */ }

// SelectNode folds the per-source node decisions in priority order: the
// first source that claims the node decides (its enumeration's Outcome
// governs — Success is authoritative even when empty, Unknown retains).
// When no registered source claims the node, the selection is Unknown
// with ErrNoPlaneEvidence — unknown, never zero devices.
func SelectNode(node NodeInfo, sources Sources, enumerated map[string]Enumeration) NodeSelection
```

Locator implementations (both production): `pkg/discovery/dra.DRALocator
{Driver, Pool, Device}` (uuid identity, from ResourceSlice advertisements)
and `pkg/discovery/classic.Locator{Node, Index}` (nodeIndex identity, from
node labels — `gpu.count` devices per node, homogeneous label facts,
deterministic ≤63-char id derivation with hash truncation). Sources read
their own inputs through injected read-only clients (`client.Reader`);
`NodeInfo` stays minimal (`Name`, `UID`). Mapping between `spec.source`
arms and locator implementations happens through per-kind codecs owned by
each source package; adapters never construct HardwareDevice objects.

Mapping into status: `Facts.Accelerator` → `status.accelerator` (vendor,
model, memory), `Plane` → `status.allocationPlane`, `CollectedAt` →
`status.collectedAt`, `Source.ID()`+locator → `status.sourceRef`. No
downstream executor may change this representation without a re-approved
contract change.

---

## 8. DRA source profile (stable `resource.k8s.io/v1`)

### 8.1 Verified whole-GPU type/keys (FGO v0.2.1 release profile)

**[executed]** against `resource.k8s.io/v1` on `kindest/node:v1.34.11`, and
**[source-inspected]** in FGO v0.2.1 `internal/dra-plugin-gpu/discovery.go`:

- Driver: `gpu.nvidia.com` (must be in `driverAllowList`).
- Whole-GPU discriminator: attribute `gpu.nvidia.com/type` == `gpu`
  (matches the chart-rendered DeviceClass CEL; MIG/partition types are
  skipped without synthesizing parents).
- Identity: attribute `gpu.nvidia.com/uuid` (primary);
  FGO-qualified fallbacks accepted exactly as emitted by the pinned
  release: unqualified `uuid` and legacy `model` are published alongside
  the qualified keys by v0.2.1 and are read as *fallbacks only*, never
  guessed key-by-key. A device with no usable UUID → **missing-ID
  rejection**: skipped, logged, counted; no object, no invented ID.
- Model: `gpu.nvidia.com/productName` (fallback `model`).
- Memory: capacity `memory`, a `resource.Quantity` in bytes (FGO converts
  MiB→bytes with BinarySI; verified `12635713536` round-trips).
- v1 encoding note (spike finding): attribute values serialize as
  `{string: ...}` in `resource.k8s.io/v1` — the v1beta1-style
  `{stringValue: ...}` is rejected by strict decoding. Our client uses the
  `k8s.io/api/resource/v1` Go types, which encode correctly.

### 8.2 Node claiming and plane resolution (source-driven, amendments 2+3)

Plane resolution is not core knowledge: each registered source owns its
claim rule, and `SelectNode` folds claims in registration order (DRA before
classic).

- DRA source claims a node whenever node-local advertisements from an
  allow-listed driver exist — **even while incomplete** (pool key
  `(driver, pool.name)`, highest generation only; the number of slices at
  the highest generation must equal `pool.resourceSliceCount`, else the
  pool is `Outcome=Unknown` — no partial updates, no fallback to classic
  facts for that node).
- Classic source claims a node when classic extended-resource evidence is
  present (`nvidia.com/gpu` — or the configured `resourceName` — in node
  allocatable/status capacity) and GFD-style labels supply the aggregate
  facts. It enumerates `gpu.count` devices with nodeIndex identities and
  homogeneous label facts (model `nvidia.com/gpu.product`, memory
  `nvidia.com/gpu.memory` MiB → bytes; heterogeneous nodes are a documented
  limitation of the labels input).
- When a node is claimed by DRA, the classic enumeration is never used as a
  fallback — including for DRA-incomplete nodes. When no registered source
  claims a node, its inventory is **unknown** — never zero devices.

### 8.3 No-remaining-slices vs missing API/Forbidden

| Situation | Classification | Effect |
|---|---|---|
| API discovery: `resource.k8s.io/v1` absent | Unknown | retain everything, condition, recheck next scan |
| List ResourceSlices → Forbidden | Unknown | retain, condition |
| List succeeds (any result) | authoritative | … |
| …zero allow-listed slices for a node that had some | authoritative empty | prune that node's DRA-backed owned devices (after grace rules if the node itself is also gone) |
| …pool present but slice count at max generation < `resourceSliceCount` | Unknown (incomplete) | retain, `PoolIncomplete` |

A successful list is authoritative; any error or incompleteness is unknown.
The two are never conflated.

**Pruning authority on an unclaimed node (task 017 correction).** A
source's successful empty enumeration of a node proves its devices gone
only under two conditions. First, no registered source claims the node
(`ErrNoPlaneEvidence`): a selected source's Unknown outcome — an
incomplete pool, a forbidden read — takes precedence over every
empty-source exception, so loss of evidence never prunes. Second, an
empty read must be disappearance proof for the source kind: the DRA
claim rule (advertisements exist) is separate from its zero-proof (a
successful read that finds none), so a zero-advertisement node's
DRA-backed devices are pruned, while the classic claim rule (evidence +
parseable `gpu.count`) makes an unclaimed classic node's empty
enumeration — missing or malformed `gpu.count`, missing
extended-resource evidence — loss of evidence, never a proven zero:
classic devices are retained. A trustworthy classic zero (evidence plus
a valid `gpu.count=0`) claims the node and prunes through the ordinary
desired-set path instead. The exception never turns a never-claimed
node into a zero-device claim: the node's selection stays Unknown and
no identities are fabricated.

---

## 9. Classic labels source profile (production, amendment 3)

The classic source is a **production source** reading exactly what a real
classic (ClusterPolicy/GFD) cluster publishes — no ConfigMap contract, no
simulator-aware code:

- **Inputs (all real, cluster-existing today):** node labels
  `nvidia.com/gpu.product` (model), `nvidia.com/gpu.memory` (MiB),
  `nvidia.com/gpu.count` (device count); classic extended-resource evidence
  (`nvidia.com/gpu` — or the singleton-configured `resourceName` — in node
  allocatable/status capacity). Read via an injected read-only client; the
  source claims only nodes with both evidence and labels.
- **Identity (user-approved relaxation):** one device per index
  `0..gpu.count-1`, `spec.identity.kind: nodeIndex`,
  `spec.identity.id = <node-name>-gpu-<index>` (deterministic; hash-truncate
  to ≤63 chars when the node name is long). Synthetic node-scoped identity:
  stable for the node incarnation, not joinable with telemetry/DCGM,
  documented as such via `identity.kind`. Node reprovisioning follows the
  standard UID-grace/delete-recreate path.
- **Facts:** homogeneous per node — model/memory from the labels
  (`gpu.memory` MiB → bytes, BinarySI); vendor omitted (GFD does not
  publish it). Absent labels on a claimed node = omitted facts, never
  zero/defaulted.
- **Known limitation (documented, not hidden):** GFD publishes one
  product/memory per node; heterogeneous GPU nodes are misreported as
  homogeneous. The labels input cannot do better; real per-device identity
  on classic nodes needs a future source (DCGM/NVML reader).
- **Configuration:** `spec.sources.classic.{enabled, resourceName}`;
  default enabled — claims stay evidence-gated, so non-GPU nodes are never
  fabricated.

### 9.1 Simulator install profiles (for task 010/011; frozen here)

Both profiles install the released chart into namespace **`gpu-operator`**
(the native FGO v0.2.1 DRA plugin hard-codes
`http://topology-server.gpu-operator/topology/nodes/`, so the release
namespace is fixed; per-node ConfigMaps also land there via
`TOPOLOGY_CM_NAMESPACE={{ .Release.Namespace }}`).

Shared values (real NVIDIA stack off, no KWOK):

```yaml
gpuOperator:        {enabled: false}
nvidiaDraDriver:    {enabled: false}
kwokDraPlugin:      {enabled: false}
kwokGpuDevicePlugin:{enabled: false}
computeDomainDraPlugin: {enabled: false}
statusUpdater:      {enabled: true}   # creates per-node topology CMs
topologyServer:     {enabled: true}
topologyConfigMap:  {enabled: true}
statusExporter:     {enabled: false}
migFaker:           {enabled: false}
runtimeClass:       {enabled: false}
builtinProfiles:    {enabled: false}
topology:
  nodePools:
    default: {gpuProduct: Tesla-K80, gpuCount: 2, gpuMemory: 11441}
  nodePoolLabelKey: run.ai/simulated-gpu-node-pool
```

- **Classic profile**: `devicePlugin.enabled: true`,
  `draPlugin.enabled: false`. Kind nodes labeled
  `run.ai/simulated-gpu-node-pool: default` and
  `nvidia.com/gpu.deploy.device-plugin: "true"` (fake device plugin
  DaemonSet selector; it needs privileged host mounts
  `/var/lib/kubelet/device-plugins` — disposable Kind nodes only). Our
  singleton enables the simulator source; assertions must show provenance
  naming it.
- **DRA profile**: `draPlugin.enabled: true`, `devicePlugin.enabled: false`.
  Nodes labeled `nvidia.com/gpu.deploy.dra-plugin-gpu: "true"` (chart's
  DaemonSet nodeSelector). The chart renders the `gpu.nvidia.com`
  DeviceClass itself when `nvidiaDraDriver.enabled` is false; on 1.34 it
  uses `resource.k8s.io/v1` + `extendedResourceName: nvidia.com/gpu`
  (**[executed]** shape acceptance, §14).
- Native DRA plugin enumerates **at startup only** (`NewDriver` →
  `PublishResources` once). Fact mutation protocol (frozen): update the
  cluster/per-node topology input → restart the affected dra-plugin
  DaemonSet pods → wait for the complete changed ResourceSlice snapshot
  (generation bump, full `resourceSliceCount`) → then assert service
  status refresh. Live republishing must not be assumed.
- Sequences run in separate run-owned clusters (or teardown/recreate).

---

## 10. Ownership, collision and grace bookkeeping

- **Grace persistence** (gate decision D5): when a node that owns
  discovered devices becomes absent or its UID changes, the singleton
  controller stamps the affected owned HardwareDevices with annotation
  `hardwarediscovery.platform.opendatahub.io/node-orphaned-at: <RFC3339>`
  (first observation time, collector clock). On recovery (node back with
  the same UID) the annotation is removed. When `now > orphaned-at +
  nodeGracePeriod (15m default)`, the devices are pruned. Because the
  deadline lives in Kubernetes state, a controller restart re-derives it
  and cannot reset it indefinitely. The singleton's
  `status.nodeSummaries` mirrors the grace state per node.
- **Node name reuse / replacement**: the same `identity.id` observed on a
  new Node UID is a new incarnation. During the grace window the old
  object (with its facts) is retained; after grace the old object is
  deleted and a fresh object is created for the new incarnation — status
  is never transferred between incarnations, and `spec.host.uid`
  immutability makes the recreation explicit (validated **[executed]**:
  `self == oldSelf` rejects in-place UID changes).
- **Same ID on two simultaneously live nodes**: ambiguous identity —
  reject both, `IdentityAmbiguous` condition; no silent transfer.
- **Observed source-ref replacement**: a successful observation from a
  different source replaces `status.sourceRef` (last-writer provenance);
  sources may augment the *same proven identity* only (e.g., model facts),
  never combine allocation planes (§8.2 dominance).
- **No hidden mutable handoff**: the two controllers share state only
  through the API objects (spec written by the singleton controller;
  status written by the device controller). No shared in-memory snapshot;
  the device controller resolves everything from its own spec. Source
  snapshots built during a scan are passed by value into reconciliation
  and never mutated concurrently.
- **Restart safety**: scans are idempotent; all bookkeeping (grace
  annotation, ownerRefs, status) is cluster state. The only in-memory
  state is the leader-election lease.

### 10.1 Watch/read strategy and scan defaults

- Watches: `HardwareDiscovery` (config), `HardwareDevice` (owned
  inventory diff), `Node` (label/status changes map to the singleton and
  affected devices; **not** a single generation predicate). No global
  ConfigMap/Secret/Pod informers.
- Source reads are **scan-driven direct reads** (list/get during
  reconciliation), not informers: both `resource.k8s.io/v1` and the
  simulator ConfigMaps are optional/conditional, and missing APIs must not
  block startup or cache sync. Availability is re-checked on every scan
  via discovery. A future guarded-watch optimization is explicitly out of
  PoC scope.
- Periodic bounded rescan: `RequeueAfter = rescanPeriod` (default 5m),
  independently of events.
- **Equality logic**: facts comparison covers `vendor`, `model`,
  `memory` (Quantity.Equals), `allocationPlane`, `sourceRef` — and
  deliberately **excludes `collectedAt`**; a freshly stamped identical
  observation is not a facts change. Status writes happen only on facts
  change or heartbeat expiry (§6.3). Unit tests prove status-only refresh
  causes no primary-resource writes via an intercepting client wrapper.

---

## 11. Helm and PlatformModuleDefinition packaging

- Chart generation: Kubebuilder `helm/v2-alpha` (verified present in
  v4.16.0), default output `dist/chart/` (task 009 owns `make/helm.mk`:
  `make helm-chart IMG=...`, `helm-review-chart`, `helm-lint`,
  `helm-render`, `helm-package`).
- Chart contents: the two CRDs (`dist/chart/templates/crd`, keep-on-uninstall
  by default), one Deployment (`[/manager, operator]`), controller RBAC —
  **no singleton instance**; samples are applied separately after CRDs.
  Manager runs with `--webhook-port=-1` (scaffold supports it), leader
  election on, explicit non-root security, health probes; no cert-manager,
  no ServiceMonitor, no webhooks.
- RBAC notes for module lint: the allowlist
  (**[source-inspected]** odh-platform-tools `checks/types`) permits
  Deployment/Service/ServiceAccount/ConfigMap/Role(Binding)/
  ClusterRole(Binding)/CRD/webhook configs/VAP/cert-manager/NetworkPolicy —
  the chart stays inside it; the leader-election **Lease is created at
  runtime by the manager, not shipped in the chart** (Lease is not on the
  allowlist).
- `packaging/module.yaml` (source-controlled) — PlatformModuleDefinition
  metadata, `spec.manifests.helm.path: ../dist/chart` (path is
  chart-root-relative-to-definition per the tool's loader; verified
  semantics **[source-inspected]**, executed check deferred to task 009's
  `make module-lint`):

```yaml
apiVersion: config.opendatahub.io/v1alpha1
kind: PlatformModuleDefinition
metadata:
  name: hardware-discovery
spec:
  instance:
    name: cluster
    type:
      group: infrastructure.platform.opendatahub.io
      version: v1alpha1
      kind: HardwareDiscovery
  manifests:
    format: helm
    helm:
      path: ../dist/chart
      values: {}
  relatedImages:
  - RELATED_IMAGE_HARDWARE_DISCOVERY_CONTROLLER
  controller:
    deployment:
      name: hardware-discovery-controller
      container:
        image: '{{RELATED_IMAGE_HARDWARE_DISCOVERY_CONTROLLER}}'
```

- Image convention: one controller image, chart value `controllerImage.repository`
  / `.tag` kept as distinct whole values (no colon splitting); local dev
  loads the image into the Kind runtime; ODH `APPLICATIONS_NAMESPACE`
  injection is accounted for by namespacing the Deployment through chart
  values (documented integration metadata, not an ODH-operator change).
- Singleton status contract for future ODH consumption: §6.4 (framework
  `api.Condition` conditions via the embedded platform status block +
  `nodeSummaries`).

---

## 12. Kind testing adaptation (to be appended to testing.md on approval)

- Adapt the DB-operator harness shape
  (`modules/opendatahub-db-operator/test/support/cluster`, commit
  `1574e15f`): small `Instance` (`Config`, `Client`, `Setup`, `Stop`) +
  functional options, **Go Kind provider** (`sigs.k8s.io/kind@v0.33.0`)
  with unique run IDs (`hw-disc-<run-id>`). Explicitly **not** copied: the
  default prefix-wide `Purge` (purge is disabled/absent in our options),
  ambient-kubeconfig writes (ours is a run-private path passed to every
  subprocess — the spike used this pattern), automatic cert-manager
  install, k3s backend.
- `Provider.Create` has no context in the pinned kind library: run provider
  create/delete through a bounded `exec.CommandContext`-style wrapper
  (kind CLI subprocess with `--kubeconfig` and timeout), propagating
  timeout errors honestly — a timed-out caller does not claim cancellation.
  This is the gate-recorded practical method.
- Baseline: `kindest/node:v1.34.11`, discovery checked explicitly for
  `resource.k8s.io/v1` before DRA-profile scenarios (spike did exactly
  this **[executed]**).
- Image loading into the exact Kind runtime via podman (Docker override
  allowed); no registry push. Test-client admin must not mask service RBAC
  (assert as the service ServiceAccount where practical).
- Cleanup before assertions; collect logs/events/source+device snapshots
  on failure; bounded, independent cleanup contexts; destroy only the
  exact run-owned cluster. CRDs retained-by-Helm verified on uninstall.
- No fake ResourceSlice CRD, no alpha DRA gates, no `latest` tags; missing
  runtime/images/simulator incompatibility is a reported blocker, never a
  passing skip.

---

## 13. What task 001 did NOT do

- No Go project, no CRDs/controllers shipped, no service code.
- FGO chart not installed and fake plugins not run (task 011); v0.2.1
  images/chart existence verified, shapes validated standalone.
- Git initialized in the working directory and the task-001 commit created
  only after human approval (D6 resolved); `.context/` remains uncommitted.
- No edits to `.envrc`, `.gitignore`, `docs/spec.md`.

---

## 14. Validation evidence summary

All spike commands, raw outputs and artifacts:
`.context/spikes/001-crd-spike/` (RESULTS.md + manifests) and
`.context/spikes/001-fgo-chart/` (pulled release chart).

**Executed** (live, disposable Kind `hw-disc-001-spike`,
`kindest/node:v1.34.11`, podman 6.1.3, kind v0.33.0, private kubeconfig;
cluster deleted afterwards):

1. `resource.k8s.io/v1` served (deviceclasses/resourceslices/…).
2. Spike HardwareDevice CRD established: CEL rule on a field named `class`
   compiles and evaluates; enum rejects non-accelerator; `self == oldSelf`
   rejects identity.id and host.uid mutation; Quantity pattern rejects
   `garbage!!` and accepts `24Gi` on /status writes.
3. Spike singleton CRD established: root CEL `self.metadata.name ==
   'cluster'` rejects any other name (no webhook needed); empty
   nodeSelector accepted; LabelSelector structure validated.
4. Spec/status isolation: status via main endpoint ignored (silently
   dropped, incl. at create); spec via /status ignored.
5. FGO-shaped DeviceClass + ResourceSlice accepted by `resource.k8s.io/v1`
   after correcting attribute encoding to v1 (`{string: ...}`); FGO's Go
   types emit this encoding.
6. FGO release verification: `helm pull oci://ghcr.io/run-ai/fake-gpu-operator/fake-gpu-operator:0.2.1`
   (digest sha256:e73fc8c0…); chart diffed against the v0.2.1 tag checkout
   (identical except CD version fields); `crane ls` confirms component
   images `:0.2.1`.
7. Published-version verification via proxy.golang.org for every pin in
   §2.2; Kubebuilder v4.16.0 module inspected for plugin set and scaffold
   pins.

**Source-inspected only**: FGO v0.2.1 runtime behavior (startup-only DRA
enumeration, hard-coded topology-server namespace, synthetic ID scheme),
NFD/GFD identity gap conclusions (from the plan's prior research),
odh-platform-tools linter/allowlist, DB harness internals,
odh-platform-utilities framework types.

**Not run**: FGO chart install/fake plugins/e2e (tasks 010/011), any
controller-runtime runtime behavior, RBAC enforcement, Helm install of our
future chart.

---

## 15. Blockers (status after gate approval, 2026-11-06)

1. **Git workspace (D6)**: resolved — Git initialized in the working
   directory on human direction; the task-001 commit
   (`docs(design): approve the hardware discovery PoC contract`) contains
   only this document. No remote was guessed; `.context/` stays uncommitted.
2. **Human approval**: resolved — approved as proposed with D1 changed to
   `github.com/lburgazzoli/opendatahub-hardware-discovery-operator`;
   approved-contract sections copied into `development.md`/`testing.md`;
   task 001 closed. Tasks ≥002 may start in plan order.
3. **Minor, non-blocking**: `packaging/module.yaml` chart-path semantics are
   verified only by source inspection until task 009 runs `make module-lint`
   against a real generated chart.
