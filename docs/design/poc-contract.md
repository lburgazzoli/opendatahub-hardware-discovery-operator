# Hardware discovery service — PoC execution contract (proposal)

Status: **APPROVED — task 001 gate passed 2026-11-06.**
Date: 2026-11-06. Author: task 001 executor.

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
| odh-platform-tools (`make module-lint`) | `github.com/lburgazzoli/odh-platform-tools@v0.0.0-20261028215308-948ecd9b3ace` (immutable pseudo-version; repo has no tags) | proxy `@latest` resolves to exactly the inspected commit 948ecd9b |

### 2.3 Framework dependency decision

**No Go dependency on `odh-platform-utilities` or `odh-platform-operator`
internals.** Rationale **[source-inspected]**:

- `odh-platform-utilities v0.6.0` (latest published) pins
  controller-runtime `v0.23.3` / k8s.io `v0.35.x`, two minors behind our
  v0.25.x/v0.37.x baseline; its framework (`framework/manager`,
  `framework/controller/reconciler`) is built around manifest
  deployment/finalizers and a platform-specific `common.Condition` type
  (`api/common/types.go`) that is **not** `metav1.Condition`. Importing it
  buys nothing for a discovery service and creates status-type friction
  (see §6.4).
- The DB-operator harness is a *pattern* reference (small `Instance`
  interface, functional options); its actual code is test-support in a
  foreign module and is re-implemented in our own test tree (task 010).

This resolves "immutable usable dependencies at milestone 001": the service
depends only on controller-runtime and the standard k8s.io libraries;
framework reuse is limited to conventions, not code.

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
│   ├── source.go                   # Source interface, DTOs, Outcome enum
│   ├── locator.go                  # Locator union (dra | simulator)
│   ├── identity.go                 # name derivation, sanitization, collision guards
│   ├── facts.go                    # Facts, equality (excludes timestamps)
│   └── plane.go                    # plane resolution / dominance rules
├── pkg/discovery/dra/              # stable resource.k8s.io/v1 NVIDIA adapter
├── pkg/discovery/classic/          # FGO simulator adapter (opt-in, PoC-only)
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
| `spec.identity.id` | string | yes | minLength 1, maxLength 63; CEL `self == oldSelf` | physical/simulated device ID (NVIDIA GPU UUID); never derived from counts or list indexes |
| `spec.host.name` | string | yes | minLength 1 | Node name at discovery time |
| `spec.host.uid` | string | yes | minLength 1; CEL `self == oldSelf` | Node UID; node replacement = delete + recreate (§11), never an in-place UID transfer |
| `spec.source.dra` | object | exactly one of `dra`/`simulator` (CEL) | see below | DRA locator |
| `spec.source.dra.driver` | string | yes | enum-extended allow-list, default pin `gpu.nvidia.com` | |
| `spec.source.dra.pool` | string | yes | minLength 1 | ResourceSlice `pool.name` |
| `spec.source.dra.device` | string | yes | minLength 1 | ResourceSlice `device.name` |
| `spec.source.simulator.namespace` | string | yes | DNS namespace | FGO simulator namespace (opt-in profile only) |
| `spec.source.simulator.configMap` | string | yes | `topology-<node>` by convention | |
| `spec.source.simulator.id` | string | yes | matches `spec.identity.id` | |

### 4.2 Status

| JSON path | Type | Notes |
|---|---|---|
| `status.vendor` | string | omitted when the source does not publish it (PoC: neither FGO CMs nor NVIDIA DRA attributes carry vendor — omitted, never defaulted) |
| `status.model` | string | FGO `gpuProduct` / DRA `gpu.nvidia.com/productName` |
| `status.memory` | `resource.Quantity` | pattern `^([+-]?[0-9.]+)([eEinumkKMGTP]*[-+]?[0-9]*)$`; FGO MiB → bytes, BinarySI |
| `status.allocationPlane` | string, enum `[classic, dra]` | which plane evidenced the device |
| `status.collectedAt` | `metav1.Time` | collector observation clock; never copied from the producer |
| `status.sourceRef` | string | provenance, e.g. `dra/gpu.nvidia.com/<pool>` or `simulator/<ns>/<cm>`; replaced, not merged, on observed source change |
| `status.conditions` | `[]metav1.Condition` | device observation conditions (§7.4) |
| `status.observedGeneration` | int64 | |

Optional unknown facts are omitted — no zero/defaulted facts, no placeholder
structs, no vendor attribute bag.

Printer columns: `Class` (.spec.class), `Model` (.status.model), `Memory`
(.status.memory), `Node` (.spec.host.name), `Plane`
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
    dra:
      driver: gpu.nvidia.com
      pool: gpu-node-a
      device: gpu-11111111-2222-3333-4444-555555555555
status:
  model: Tesla-K80
  memory: 12635713536
  allocationPlane: dra
  collectedAt: "2026-11-06T15:32:31Z"
  sourceRef: dra/gpu.nvidia.com/gpu-node-a
  conditions:
  - type: Observed
    status: "True"
    reason: Observed
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
attributes) are added as new *optional* status fields or optional structs
with their own validation, plus new enum values for `spec.class`. Because
every current field is required-and-validated or explicitly optional with
`omitempty`, additions are non-breaking; no empty bag/map ships in v1alpha1.
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
| `spec.sources.dra.enabled` | bool | `true` | stable DRA source |
| `spec.sources.dra.driverAllowList` | []string | `["gpu.nvidia.com"]` | whole-GPU advertisements only from these drivers |
| `spec.sources.simulator.enabled` | bool | `false` | **opt-in** classic PoC adapter; default off |
| `spec.sources.simulator.namespace` | string | required when enabled | the only namespace whose ConfigMaps are read |
| `spec.sources.simulator.configMapPrefix` | string | `topology` | FGO `TOPOLOGY_CM_NAME` |

Root-schema CEL (validated **[executed]**):
`self.metadata.name == 'cluster'` — the singleton cannot be created under
any other name; no webhook needed. Deleting and recreating `cluster` is the
documented reset path.

### 5.2 Status

| JSON path | Type | Notes |
|---|---|---|
| `status.observedGeneration` | int64 | |
| `status.conditions` | `[]metav1.Condition` | collector/observation health — **not** hardware health, **not** free-capacity (types: `Ready`, `Degraded`; reasons: `Reconciled`, `SourcesUnavailable`, `SourceForbidden`, `PoolIncomplete`, `NameCollision`, `IdentityAmbiguous`, `InvalidConfiguration`) |
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
    simulator:
      enabled: true
      namespace: gpu-operator
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
   │ resource.k8s.io/v1  │     │ FGO simulator CM reader  │
   │ read-only client    │     │ (opt-in, PoC-only)       │
   └─────────────────────┘     └──────────────────────────┘

 External read-only inputs: Nodes, ResourceSlices/DeviceClasses (resource.k8s.io/v1),
 ConfigMaps (simulator namespace only, adapter opt-in).
 External writes: HardwareDevice (spec, singleton controller only),
 HardwareDevice/status (device controller only), HardwareDiscovery/status,
 Lease (leader election), Events.
```

### 6.2 Read/write ownership matrix

| Resource | singleton ctrl | device ctrl | sources | consumers |
|---|---|---|---|---|
| HardwareDiscovery | read spec, write status | — | — | — |
| HardwareDiscovery/status | write | — | — | read |
| HardwareDevice | create/delete, write spec | read spec | — | get/list/watch |
| HardwareDevice/status | read | **only writer** | — | read |
| Node | read (get/list/watch) | read | read | — |
| ResourceSlice/DeviceClass | read (scan, guarded) | read (observe) | read-only | — |
| ConfigMap (simulator ns) | read (scan, only if enabled) | read (observe) | read-only | — |
| Lease / Event | write (as used) | write Events | — | — |

### 6.3 Creation / observation / deletion sequences

Creation (singleton controller, per scan):
1. Resolve selected nodes (label selector in reconcile logic, not cache
   filtering, so deselection and new matches stay observable).
2. For each node, run plane resolution (§8.2): DRA first, classic only with
   both extended-resource evidence and the opt-in identity source.
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

Deletion:
- Authoritative disappearance (successful empty enumeration) → prune owned
  objects (after §11 grace when the trigger is node absence/UID change).
- User removes class/selector coverage → those nodes' owned devices pruned.
- Singleton deletion → real GC via ownerRef removes children; no finalizers.
- Foreign objects are never touched.

### 6.4 Platform status compatibility (explicit resolution)

Our CRDs use standard `metav1.Condition`. The platform-common
`common.Condition`/`Phase` types from `odh-platform-utilities` are **not**
imported (§2.3): they are a different type family from a framework we do not
depend on. Future ODH orchestration consumption reads our
`status.conditions` by type/reason string and `status.nodeSummaries`; no
runlevel/ManagementState integration is claimed. This is the explicit
resolution the gate asked for — metav1 and the platform types do not match,
and we do not pretend otherwise.

### 6.5 Failure-state table

| Failure | Detection | Inventory effect | Condition |
|---|---|---|---|
| resource.k8s.io/v1 not served / Forbidden | discovery check + API error on scan | retain all facts+collectedAt; plane unknown | `SourcesUnavailable`/`SourceForbidden` |
| Partial new pool generation | slice count < `pool.resourceSliceCount` at max generation | retain; **no** partial update, no fallback | `PoolIncomplete` |
| Successful empty enumeration | `Outcome=Success`, zero identities for node | prune owned devices of that node | — (Ready) |
| Simulator disabled / CM missing | adapter outcome Unknown | retain | `SourcesUnavailable` |
| Duplicate/ambiguous identity (same ID twice, or same ID on two live nodes) | enumeration guard | reject both; no object created/updated | `IdentityAmbiguous` |
| Name collision with foreign object | name derivation | skip creation | `NameCollision` |
| Node absent or UID mismatch | node cache/read | retain during grace, then prune | node summary `outcome=Unknown`→pruned |
| Source published fact change | Observe diff | update facts + collectedAt | — |
| Source temporarily unreadable for one device | Observe error | keep last facts; condition only | device `SourceUnavailable` |

---

## 7. Source DTO contract (frozen at this gate)

All types live in `pkg/discovery`; adapters never construct
`HardwareDevice` Kubernetes objects — the mapping to
`HardwareDeviceStatus` happens once, in the device controller.

```go
// Plane is the allocation plane that evidenced a device.
type Plane string // "classic" | "dra"

// Outcome distinguishes authoritative results from unknown ones.
type Outcome string
const (
    OutcomeSuccess Outcome = "Success" // authoritative snapshot; may be empty
    OutcomeUnknown Outcome = "Unknown" // error / missing API / forbidden / incomplete pool
)

// Locator is the frozen, source-owned union stored verbatim in spec.source.
type Locator struct {
    DRA       *DRALocator       `json:"dra,omitempty"`
    Simulator *SimulatorLocator `json:"simulator,omitempty"`
}
type DRALocator       struct{ Driver, Pool, Device string }
type SimulatorLocator struct{ Namespace, ConfigMap, ID string }

// Identity is a discovered device identity plus how to re-observe it.
type Identity struct {
    ID       string      // physical/simulated device ID (GPU UUID)
    NodeName string
    NodeUID  types.UID
    Plane    Plane
    Locator  Locator
}

// Facts are the minimal supported observations; absent = omitted, never zero.
type Facts struct {
    Vendor string            // optional, omitted when unsupported
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

type Source interface {
    ID() string
    Plane() Plane
    // Enumerate: identities for the given nodes. Empty+Success prunes;
    // Unknown preserves all prior state.
    Enumerate(ctx context.Context, nodes []NodeInfo) (Enumeration, error)
    // Observe: resolve one previously enumerated identity into facts.
    Observe(ctx context.Context, id Identity) (Observation, error)
}
```

Mapping into status: `Facts` → `status.{vendor,model,memory}`, `Plane` →
`status.allocationPlane`, `CollectedAt` → `status.collectedAt`,
`Source.ID()`+locator → `status.sourceRef`. No downstream executor may
change this representation without a re-approved contract change.

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

### 8.2 Dominance and plane resolution

- Pool key `(driver, pool.name)`; only the **highest generation** is read.
- Pool completeness: the number of slices present at the highest generation
  must equal `pool.resourceSliceCount`; otherwise the pool is
  `Outcome=Unknown` — no partial updates, no fallback to GFD/simulator
  facts for that node.
- DRA wins plane detection for a node whenever node-local advertisements
  from an allow-listed driver exist — **even while incomplete**. In that
  case the classic adapter must not claim the node.
- Classic claims a node only when (a) no DRA advertisements exist for it,
  (b) appropriate extended-resource evidence is present
  (`nvidia.com/gpu` in node allocatable/status capacity, provided by the
  fake device plugin in the simulator profile), and (c) the opt-in identity
  source is enabled. If neither plane has the required evidence, the node's
  inventory is **unknown** — never zero devices.

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

---

## 9. Classic FGO simulator adapter profile (opt-in, PoC-only)

**[executed]** release verification and **[source-inspected]** contract:

- Pin: FGO **v0.2.1** (tag `v0.2.1`, commit `0901c8fc9c235e5b2ef8a2876fb89db1f7af23f4`). Released chart
  `oci://ghcr.io/run-ai/fake-gpu-operator/fake-gpu-operator:0.2.1`
  (digest `sha256:e73fc8c0...`) is content-identical to the inspected tag's
  `deploy/fake-gpu-operator` except CD-set `version`/`appVersion: 0.2.1`;
  component images `ghcr.io/run-ai/fake-gpu-operator/{device-plugin,dra-plugin-gpu,status-updater,topology-server,status-exporter}:0.2.1`
  all exist. FGO `main` (`af4ff6d4`) differs from the tag only in
  CHANGELOG/CI/Makefile — contract-relevant files are identical — so the
  release artifact matches the inspected source.
- Adapter reads only: ConfigMaps named `<prefix>-<node>` (default prefix
  `topology`, chart `TOPOLOGY_CM_NAME`), in the **configured namespace
  only**, data key `topology.yml`, fields `gpus[].id`, `gpuProduct`,
  `gpuMemory` (MiB), `migStrategy` (read for provenance only).
  `gpuMemory` → `resource.NewQuantity(MiB*1024*1024, BinarySI)`.
- Explicitly ignored: occupancy/`allocatedBy`/`podGpuUsageStatus`, health,
  pod usage, CUDA execution, `otherDevices`, driver/cuda versions.
- Synthetic IDs are `GPU-<uuid5-SHA1(node name + index)>` — deterministic
  per node name/index; the e2e must not assert physical identity across
  node reprovisioning (IDs change with node name).
- Adapter is enabled only by `spec.sources.simulator.enabled: true` +
  namespace; default off; provenance (`sourceRef`, conditions) always names
  the simulator source. It is evidence of control flow, not production
  classic hardware discovery.

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
- Singleton status contract for future ODH consumption: §6.4 (metav1
  conditions + `nodeSummaries`; no platform-common types).

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
