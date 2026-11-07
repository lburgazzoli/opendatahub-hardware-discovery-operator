# Integration — the ODH module handoff

This document is the integration contract for onboarding the hardware
discovery service into `opendatahub-io/opendatahub-operator` as a platform
module. It states what the shipped service does, what the module
orchestrator must provide, and what neither of them promises. It is written
from the delivered code and the executed e2e results — not from intent.

The approved design contract is
[`docs/design/poc-contract.md`](design/poc-contract.md); the runtime
identifiers are in [`docs/development.md`](development.md); the testing
evidence is in [`docs/testing.md`](testing.md).

## 1. What the service is

A per-cluster, read-only hardware inventory publisher. One
`HardwareDiscovery` singleton (fixed name `cluster`) selects nodes and
configures sources; the controller enumerates whole GPUs on those nodes and
publishes each as a cluster-scoped `HardwareDevice` owned by the singleton.

Two responsibilities, two controllers:

| Controller | Owns | Writes |
|---|---|---|
| inventory (`HardwareDiscovery`) | node selection, source registry, per-node outcomes, device lifecycle (create/prune) | singleton `/status` (`nodeSummaries`, conditions); `HardwareDevice` creates and deletes (spec + metadata via `manager` field manager) |
| device (`HardwareDevice`) | one device's observed facts | device `/status` (facts, `collectedAt`, conditions) via the framework's status apply |

Spec is never rewritten after creation: device identity, host and source
locator are immutable (CEL `self == oldSelf` on `identity.*`, `host.uid`,
`class`); all observation lives in status. Deleting the singleton deletes
every owned device through the Kubernetes owner-reference GC — the service
implements no finalizer of its own.

## 2. Reading the published inventory

- **`Ready` means inventory readiness, not free capacity.** A device
  `Ready=True, Reconciled` records a successful observation of the device's
  existence and facts. It says nothing about allocatable or free capacity —
  the service does not read allocation state.
- **Facts are timestamped.** `status.collectedAt` is the observation clock.
  Consumers that need liveness must check freshness against
  `spec.maxStale` (default `10m`) themselves; a stored `Ready` never
  retracts on its own when the collector stops.
- **Conditions are collector health**, not hardware health: types `Ready`
  and `Degraded`, reasons `Reconciled`, `SourcesUnavailable`,
  `SourceForbidden`, `PoolIncomplete`, `NameCollision`, `IdentityAmbiguous`,
  `InvalidConfiguration`.
- **Reader RBAC ships with the chart.** The chart renders
  `infrastructure-hardwaredevice-viewer-role` and
  `infrastructure-hardwarediscovery-viewer-role` (get/list/watch) plus the
  corresponding admin/editor roles. A module consumer needs only these
  verbs against the two CRDs — the controller's own role is irrelevant to
  readers.
- `status.nodeSummaries` on the singleton is the compact overview: per
  selected node, name, uid, plane, device count and scan outcome.

## 3. Collection semantics the module must preserve

These are the behavioral contracts the orchestrator must not subvert by
configuration:

- **Selector default is all nodes.** An omitted or empty `nodeSelector`
  matches every eligible node.
- **Source arms default to enabled.** A nil or omitted source arm means
  enabled and evidence-gated; disablement must materialize the arm with
  `enabled: false` explicitly.
- **Unknown beats empty.** A selected source's Unknown outcome (incomplete
  pool, forbidden read) takes precedence over every empty-source
  exception: loss of evidence never prunes devices. An empty read prunes
  only when it is disappearance proof for that source kind (a successful
  zero-advertisement DRA read prunes; missing or malformed classic
  `gpu.count` evidence does not).
- **Partial pools are retained.** A DRA pool whose newest generation has
  fewer slices than `pool.resourceSliceCount` yields no partial update and
  no fallback: existing facts are retained and the affected devices carry
  `Degraded=True, PoolIncomplete`.
- **Failed observations retain facts.** Facts and `collectedAt` survive
  failed or unknown observations; `collectedAt` advances only on fact
  change or heartbeat expiry (`maxStale`), so an unchanged-facts status
  rewrite is the exception, not the rule.
- **Node identity is uid-scoped.** `spec.host.uid` is immutable; node
  replacement is delete + recreate of the device, never an in-place UID
  transfer. The incarnation-convergence semantics of status writes are
  proven in `docs/testing.md`.
- **Condition-only recovery.** When evidence returns after a degraded
  scan, the controller recovers by rewriting conditions and facts in one
  pass; there is no separate "repair" path.

## 4. Source provenance — what each plane actually reads

| Source | Reads | Identity | Provenance caveats |
|---|---|---|---|
| `dra` | native `resource.k8s.io/v1` ResourceSlices of allow-listed drivers (default `gpu.nvidia.com`); per-device data | `identity.kind: uuid` — real vendor device ID | Stable API; trusted only for allow-listed drivers. Real-driver ResourceSlice shapes were not tested (simulator only). |
| `classic` | node extended resources (`nvidia.com/gpu`) plus GFD-style labels | `identity.kind: nodeIndex` — synthetic `<node-name>-gpu-<index>`, node-scoped | Deliberate shortcut: stock GFD/NFD labels are homogeneous per node and publish no per-GPU UUID, so per-device identity on this plane is index-only and not joinable with telemetry/DCGM. |
| fake-gpu-operator (FGO) | test-only: synthetic Node scalars/labels simulating both planes | n/a (feeds the two above) | **Test-only.** Run:ai fake-gpu-operator chart (digest `sha256:e73fc8c0…`, v0.2.1), installed by the e2e harness on CPU-only Kind clusters. Its frozen simulator facts (Tesla-K80, 11441Mi, count 2) are not hardware evidence, and its DRA plane publishes synthetic UUIDs — the demo's `identity.kind: uuid` values are simulator-generated, not evidence of real vendor identity extraction. |

Production reads only Nodes and native ResourceSlices — no ConfigMaps, no
telemetry, no allocation state. No topology, MIG, health/utilization or
scheduling claims are made by any plane.

Superseded: the draft-spec's ConfigMap identity adapter never shipped —
identity is resolved from the sources above at discovery time.

## 5. Framework integration architecture

The service is framework-first, per the module contract:

- **Dependency:** `github.com/opendatahub-io/odh-platform-utilities/framework`
  pinned to the approved immutable main snapshot
  `v0.4.1-0.20261105151519-69e52f844d32` (commit `69e52f84`). The pin is a
  deliberate choice over the latest tag: the tag line pins
  controller-runtime/k8s versions that skew from this repository's
  (controller-runtime v0.25.2), and the snapshot carries the builder API
  this service registers through.
- **Registration:** both controllers register through the framework
  builder (`ReconcilerFor → …Watches(…) → Build(ctx)`); there is no local
  native registration path left. The inventory scan is serialized by
  stamping the manager's `Controller` options with
  `GroupKindConcurrency["HardwareDiscovery.infrastructure.platform.opendatahub.io"] = 1`
  (`ManagerControllerOptions`) — one singleton, one writer — while the
  device controller keeps the global concurrency default.
- **PlatformObject:** both APIs embed the framework status inline
  (`fwapi.Status`), so conditions/`observedGeneration` carry framework
  semantics; the module reads them with no local translation.
- **Action pipelines:** each controller is a framework action pipeline
  (scan/observe → status) that deliberately omits the framework's render,
  deploy, GC and finalizer actions — the service renders and deploys
  nothing of its own. The Helm chart is the deployment vehicle, not a
  reconciled child.
- **Condition aggregation:** each pipeline aggregates its actions'
  outcomes into the framework condition set (`Ready`/`Degraded` over the
  reasons in §2) through the framework's condition aggregation — there is
  no local condition bookkeeping.
- **Phase:** `status.phase` is derived by the framework from conditions
  and carries no service contract; conditions are the machine-readable
  surface.
- **Status SSA:** device status is written through the framework's
  status-apply path (field manager `device`, subresource-scoped); the
  inventory create path uses field manager `manager` (owns
  `f:metadata`/`f:spec`). The split is framework behavior; consumers
  should not write either region.

## 6. Packaging and deployment contract

- **Chart:** `dist/chart` — Kubebuilder helm/v2-alpha generated, committed
  as reviewed output, with `fullnameOverride:
  opendatahub-hardware-discovery-operator` so rendered names are stable
  under any release name (release used by the e2e: `hardware-discovery`,
  namespace `opendatahub-hardware-discovery-operator-system`).
- **Module metadata:** `packaging/module.yaml` (`PlatformModuleDefinition`,
  name `hardware-discovery`) — singleton instance GVK, chart path, related
  image token. It is metadata, not a shipped CRD or an instance.
- **Singleton creation is the orchestrator's job.** The chart installs the
  controller, RBAC and CRDs; it does not create the `cluster`
  `HardwareDiscovery` instance. The future ODH operator handler (or a
  user) applies it — `config/samples/` shows the shape.
- **Related images:** one image, `RELATED_IMAGE_HARDWARE_DISCOVERY_CONTROLLER`,
  resolved from environment or `--related-images-file` at deploy time; the
  chart's `controllerImage.{repository,tag}` values are the render-time
  fallback. Never colon-split.
- **CRD retention:** the chart is generated with CRDs retained on uninstall
  (`helm.sh/resource-policy: keep`), so an operator upgrade does not delete
  live `HardwareDevice` objects. Verified live in the e2e uninstall
  scenario.
- **No automatic upgrade or rollback.** v1alpha1 is PoC-stage: changed
  schemas require a fresh deployment (delete singleton + devices,
  uninstall, install). No live-object migration, conversion webhook or
  management-state transition is implemented or promised.

## 7. What the ODH operator side must still do

Not done here, and deliberately not stubbed:

1. A module handler in `opendatahub-operator` that creates the `cluster`
   singleton (and owns its lifecycle) when the module is enabled.
2. A pin of this repository/module in the operator's manifest pins.
3. Related-image wiring into the operator's image resolution.
4. Any module-level status aggregation beyond what
   `status.conditions`/`status.nodeSummaries` already publish.
5. The events RBAC gap: the framework recorder emits events for
   cluster-scoped `HardwareDevice`s into namespace `default`, where the
   chart ServiceAccount has no events permission. Best-effort diagnostics,
   cosmetic-to-minor; fix belongs to the chart/operator review, not here.

No pins, handlers or versions are invented for these — that is the
operator repository's decision surface.

## 8. Conformance vs onboarding

The destination is explicit: integration into
`opendatahub-io/opendatahub-operator` as a platform module — standalone
Helm execution is this PoC's proof path, not the goal. The mandatory
module contract (framework-first, no arbitrary standalone consumer)
governed every API/controller/runtime/packaging decision here.

The evidence-backed packaging checklist — singleton GVK/name, chart and
Deployment names, image inputs, namespace/config alignment, service
permissions, CRD lifecycle, and the separation of the installation
envelope (controller + CRDs + RBAC) from operands (the service deploys
none) — is the package-contract table in
[`docs/development.md`](development.md); the framework/builder and
dependency use is §5 above.

The Kind demonstration is **conformance evidence for the service**, not
onboarding evidence for the module:

- proven: the chart deploys a working controller; both source planes
  discover, retain, prune and converge; the singleton lifecycle behaves;
  CRDs survive uninstall; the status-write incarnation semantics hold on a
  real 1.34.11 API server (`docs/testing.md`).
- not proven: anything requiring physical GPUs, real GFD/NFD labels, real
  DRA drivers, or a running opendatahub-operator (no handler, pin or
  module registration was executed). Remaining operator-side work is
  §7 above; no root/framework type compatibility is inferred from
  external examples, and a passing Helm demo is not ODH proof.

Onboarding the module into the ODH operator is future work on the
operator's terms; this repository ships the contract pieces above for it.

## 9. Superseded and deferred design decisions (historical record)

Kept for provenance; the shipped contract differs:

- the draft spec's **all-facts-in-spec / no-status** shape — superseded by
  the observed-facts-in-status model (immutable spec, timestamped status).
- **topology day-one** — deferred: topology fields are not in v1alpha1.
- **exhaustive phase enum** — superseded by framework conditions; `phase`
  is a derived, uncontracted field.
- the **ConfigMap identity adapter** — superseded by source-native
  identity resolution.

Later additive API directions — other device classes, topology, MIG, and
a real per-device classic identity — are explicitly *not implemented and
not promised* by this PoC; each would be a reviewed, additive schema
change.
