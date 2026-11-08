# Hardware Device Inventory — `HardwareDevice` CRD and Collector Spec

**Status**: Draft v0.2 — starting point for implementation; the exact CRD schema is deliberately left open for the implementing agent to define via investigation (§4)
**Source**: [RHAISTRAT-3018](https://issues.redhat.com/browse/RHAISTRAT-3018) (Hardware Inventory and Capability Discovery, llm-d quality for RHOAI 3.7, target Jan 2027), multi-agent research pass 2026-11-04 (Jira + vault + web, adversarially reviewed)
**Date**: 2026-11-04
**Audience**: implementing agent / engineer building the controller. This document is self-contained; it carries the verified context, the settled design record, the CRD and collector specification, and the open questions.

---

## 1. Why this document exists

RHAISTRAT-3018 needs a queryable inventory of the shared llm-d validation fleet's hardware — GPU model/count, and later NIC model/capabilities, topology, CPU, memory, storage, location — with **provenance and collection timestamps**, consumed by the configuration matrix (testable hardware combinations) and by automation, without consumers duplicating a hardware registry.

Research (2026-11-04) established that **no off-the-shelf solution spans both allocation worlds** (classic device-plugin/NFD/GFD discovery and DRA ResourceSlices). The agreed answer is a small CRD + collector that normalizes both planes into one queryable kind. This spec is that design, iterated to the point where implementation can start.

Out of scope for this doc: admission/reservations/utilization (owned by the shared-infrastructure initiative, RHAISTRAT-3020).

## 2. Load-bearing facts the implementer must know

These were verified against primary sources on 2026-11-04. They shape every design decision below — do not assume otherwise.

1. **DRA and the classic device plugin are mutually exclusive per cluster** (NVIDIA GPU Operator v26.7: a cluster has either a `GPUCluster` (DRA) or a `ClusterPolicy` (device plugin), not both; no in-place migration). Source: [GPU Operator 26.7 DRA docs](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/26.7/dra-intro-install.html). The fleet WILL be split across the two planes through 2027.

2. **GFD is NOT deployed in DRA (GPUCluster) mode.** `GPUCluster` manages the DRA driver, ComputeDomains, DCGM, DCGM Exporter and a validator; **GPU Feature Discovery belongs to the `ClusterPolicy` stack**. Therefore `nvidia.com/gpu.product` / `gpu.memory` / `gpu.count` / `gfd.timestamp` node labels exist on classic nodes only. Source: capability comparison table in the GPU Operator 26.7 DRA docs (link above). Consequence: **a labels-only discovery controller is blind on DRA nodes** — ResourceSlices must be a first-class source.

3. **KEP-5004 (DRA Extended Resources; beta K8s 1.36 default-on, GA 1.37) does NOT republish DRA capacity into `Node.status.allocatable`.** Pods can still request `nvidia.com/gpu` on DRA nodes (scheduler creates an implicit ResourceClaim), but anything reading Node objects — kube-state-metrics, dashboards — sees **zero GPUs on DRA nodes**. Sources: [KEP-5004](https://github.com/kubernetes/enhancements/blob/master/keps/sig-scheduling/5004-dra-extended-resource/README.md), [DRA extended resource docs](https://kubernetes.io/docs/concepts/resource-management/dynamic-resource-allocation/dra-api/#extended-resource). On DRA nodes the legacy `nvidia.com/gpu` name additionally requires a user-created DeviceClass with `spec.extendedResourceName` (the operator-managed `gpu.nvidia.com` DeviceClass does not set it).

4. **DRA ResourceSlices are a rich per-device inventory** on DRA nodes: `resource.k8s.io/v1`, cluster-scoped, field-selectable by `spec.driver` / `spec.nodeName` / `spec.pool.name`; NVIDIA driver (`gpu.nvidia.com`) publishes per device `productName`, `memory` capacity, `uuid`, `cudaComputeCapability`, and standardized `resource.kubernetes.io/pciBusID` / `pcieRoot` / `numaNode`. Source: [ResourceSlice attributes reference](https://dra-driver-nvidia-gpu.sigs.k8s.io/docs/reference/resourceslice-attributes/).

5. **NFD runs in both worlds** (it is a hard GPU Operator prerequisite), but NFD itself has no DRA integration (through v0.20.1). GFD labels are the only native collection timestamp in the classic stack (`nvidia.com/gfd.timestamp`). NFD publishes the `NodeFeature` CRD (per-node raw feature inventory, refreshed every `sleepInterval`, default 60s).

6. **Pool freshness protocol**: ResourceSlices carry no measurement timestamp; consumers detect staleness/incompleteness via `spec.pool.generation` (use highest) and `spec.pool.resourceSliceCount` (pool complete when all slices at latest generation are present).

7. **Do not build on alpha DRA APIs** for the Jan 2027 target: `ResourcePoolStatusRequest` (alpha 1.36, gated, counts only), `DRANodeAllocatableResources` (alpha 1.36), DynamicMIG (driver alpha; the internal "GPU Operator 26.11 beta/GA" claim is unverified publicly — treat as roadmap).

Precedents for resource-centric discovery objects (pattern is established, never vendor-neutral across both planes — that gap is what this project fills): Akri `Instance` (one object per discovered device), GPUStack `Devices` CR, NVIDIA `kubectl-nv` (union-membership rule: a node is a GPU node if it has `nvidia.com/gpu`/`mig-*` capacity **or** a `gpu.nvidia.com` ResourceSlice naming it), [NVSentinel](https://github.com/NVIDIA/NVSentinel) (NVIDIA health/reliability tool, DRA-aware, dual-source GPU counting with per-source attribution — useful design reference, not a capability inventory).

## 3. Design record (settled decisions)

| Decision | Value | Rationale |
|---|---|---|
| Style model | **DRA `ResourceSlice` is the aspirational style — but type-safe and deliberately minimal** | Keep the ResourceSlice shape (publisher-owned spec, named devices, field-selectable) but replace the `map[string]Value` attribute bag with typed structs; expose only common user-facing info, not exhaustive device detail |
| API group | `infrastructure.platform.opendatahub.io` | ODH-owned platform building block; NOT `infrastructure.opendatahub.io` (HardwareProfile's GA product group — avoid coupling to rhods-operator release trains and the RHAISTRAT-2519/2382 extension battlefield) |
| Kind | `HardwareDevice` (plural `hardwaredevices`, shortNames `hwdev`, `hw`) | Bare `Device` collides with DRA vocabulary (DeviceClass, ResourceSlice `devices[]`) — the name must say "hardware facts, not allocation unit" |
| Granularity | **One object per physical device** (1 per GPU, 1 per NIC, 1 per disk) | Fleet queries by device; identity survives node reprovisioning; non-K8s hardware (workstations, cloud instances) modelable via `hostRef.kind: Host` |
| Not objects | MIG slices, SR-IOV VFs, sharing replicas = **fields** on the parent device | Allocation units, not hardware identity; avoids churn when geometry changes |
| Initial scope | `class: accelerator` only | `class` enum ships with one value; sibling structs (`network`, `storage`, `fabric`) and enum values added later are additive, no version bump |
| Union discipline | Per-class typed struct, exactly one set per object; sibling structs + CEL is the current recommendation, to be validated during the schema investigation | True `oneOf` is awkward in structural schemas; Gateway API / Kueue `x-kubernetes-validations` pattern |
| Split trigger (pre-committed) | Extract `Accelerator` into its own kind when its struct forces an API version bump or grows a distinct lifecycle/owner | 1→N extraction is cheap; merging later is not |
| Writer | Collector **writes `spec` directly** (ResourceSlice pattern). No status subresource, no webhooks | DRA drivers write ResourceSlice.spec the same way; multiple collectors may write the same kind, discriminated by `provenance.driver` |
| Node relation | `hostRef { kind, name, uid }` — node is an attribute, not the anchor; `uid` guards node-name reuse | |
| Device relations | Inter-device relationships expressed as **local/object refs** (relation type + name + uid), never as duplicated identity; refs are advisory — no GC blocking, stale refs tolerated | Derived relations (same `pcie.root`) stay query-time joins; only non-derivable relations (fabric domains, parent links) are materialized |
| Allocation planes | Per-device `allocation.mode: classic \| dra \| none`; planes never merged | |
| Provenance/freshness | Object-level `provenance.driver` + `collectedAt`; write-on-change + bounded heartbeat | |
| Version | `v1alpha1`; no GA commitment tied to the Jan 2027 fleet deadline | |
| Vendor long tail | `attributes: map[string]string` on the device | Typed structs stop at the vendor-neutral core (model/memory/pcie/numa/mig); vendor specifics (nvlink details, clique, …) go here |
| Product convergence | RHAISTRAT-2519's discovery controller should become a **consumer** of HardwareDevice (fixes its DRA blind spot); cross-link before implementation | See `references/wiki/rhai/gpuaas/rhai-hwp-v2-redesign.md` for the coherence risk |

## 4. CRD requirements — schema shape intentionally left open

The exact OpenAPI schema is **not** specified in this document. The implementing agent is expected to run its own investigation (structural-schema constraints, CEL feasibility in envtest, precedents: ResourceSlice, NodeFeature, SriovNetworkNodeState, Akri `Instance` — see §2 and §11) and propose the concrete `v1alpha1` schema for review before implementation. What follows is the **fixed API surface** (settled in §3 — do not change without revisiting that record) and the **semantic requirements** any proposed schema must satisfy. Field names used below are conceptual handles for discussing the requirements, not a naming commitment.

**Design principle — governs every schema decision: the DRA APIs, `ResourceSlice` in particular, are the aspirational style.** Mimic its shape and discipline (publisher-owned `spec`, named devices, cluster-scoped, field-selectable, replace-on-change semantics) but with two deliberate deviations:

1. **Type-safe instead of stringly.** Where ResourceSlice devices carry a `map[string]Value` attribute bag, HardwareDevice carries typed structs with real OpenAPI types — consumers get validation, codegen, and `kubectl explain` instead of key-by-key convention.
2. **Deliberately smaller, not exhaustive.** ResourceSlice exposes everything an *allocator* needs; HardwareDevice exposes only the **common information a user may need** — the facts a config-matrix job, dashboard user, or test selector would actually query, filter, or display (vendor, model, VRAM, host, allocation plane, MIG geometry, PCIe root for affinity). It does NOT mirror every attribute the sources can produce. When in doubt, leave a field out: adding a field later is additive, removing one is breaking. The `attributes` escape hatch (§4.2 item 8) is the pressure valve that keeps the typed core small.

### 4.1 Fixed API surface

- Group `infrastructure.platform.opendatahub.io`; kind `HardwareDevice`; plural `hardwaredevices`; shortNames `hwdev`, `hw`; categories including `odh`, `platform`, `all`
- Cluster-scoped; `v1alpha1`, served + storage
- **No status subresource, no webhooks, no finalizers** — the collector writes `spec` and owns the whole object (ResourceSlice pattern)
- The collector applies facet labels (vendor/model/class/host) for query ergonomics, normalized to valid label values

### 4.2 Required semantic capabilities

The schema must be able to express, per object:

1. **Device identity** — an immutable vendor identity (GPU UUID, serial, MAC) usable as the join key when multiple sources describe the same device. Not necessarily globally unique; object naming must compensate (§5.3).
2. **Class discriminator** — a single value in v1alpha1 (`accelerator`), defined so that adding values later (`network`, `storage`, `fabric`) is purely additive (enum/struct extension, no version bump).
3. **A typed per-class facts struct** (accelerator only in v1alpha1) carrying **only the common, consumer-facing facts**: vendor, model, memory, and PCIe root/busID — the last captured from day one for future GPU–NIC affinity — plus the small set of facts a consumer would filter or display (architecture, compute capability, NUMA node, MIG capability and geometry). Availability from a source is *necessary but not sufficient* to include a field: the test is "would a user or automation query on this?" A fact that fails that test belongs in the attributes map or nowhere. MIG `mode` must reserve a `dynamic` value (KEP-4815) without defining its semantics yet.
4. **Host reference** — host kind (Kubernetes `Node` vs non-cluster `Host`), name, and a UID for node-incarnation safety (§5.4).
5. **Inter-device relationships, expressed as local/object refs** — where a device has a relationship to another device object (e.g., an accelerator belonging to an NVLink/fabric domain, a future VF pointing at its parent NIC, a device attached to a future `HardwareHost`), the relationship MUST be expressed as a **reference to the other object** (name + UID, plus a relation type), never by duplicating the other device's identity or facts into this one. Refs are slim — same kind and group, so no full `ObjectReference` needed; a LocalObjectReference-style shape suffices, with `uid` for incarnation safety (same rationale as `hostRef`). Semantics: refs are advisory facts, not ownership — no finalizers, no GC blocking; a ref whose target no longer exists is stale data that the collector rewrites or drops on its next pass, and consumers must tolerate it. Derived relationships (e.g., "GPU and NIC share a `pcie.root`") remain query-time joins over attributes — materialize a ref only when the relationship is *not* trivially derivable (fabric domains from Topograph, parent links, topology edges a source reports explicitly).
6. **Allocation plane declaration** — mode `classic | dra | none`; classic implies an extended-resource name, `dra` implies a driver identity (and typically device classes); optionally how the device is subdivided for allocation (time-slicing / MPS / MIG). Invariants to enforce via CEL or equivalent: classic requires a resourceName; dra requires a driver; planes are never merged.
7. **Provenance and freshness** — collector identity (DNS-style), collection timestamp (collector observation time, never copied from a producer — §5.4), and optionally the list of raw sources that fed the object.
8. **Location** — optional, fleet-supplied (site/rack/room).
9. **Vendor long-tail escape hatch** — a free-form attributes map, so the typed struct stops at the vendor-neutral core.

### 4.3 Schema investigation checklist (for the implementing agent)

- **Union mechanics**: how to discriminate per-class structs (sibling optional structs + a CEL "exactly one set, matching class" rule is the current recommendation — validate it) within structural-schema rules. Check CEL field-name gotchas (e.g., a field named `class`) in envtest **before** committing to field names — renames after rollout are breaking.
- **Quantity representation** for memory/capacity: use `resource.Quantity` semantics; do not invent a custom format.
- **Printer columns** for the primary facets (class, model, host, allocation mode, collected).
- **Required vs optional discipline** per class struct; which enum values must be reserved on day one.
- **Object naming derivation** (§5.3) and label normalization rules (slugify, 63-char truncation).
- **Ref semantics for inter-device relationships** (§4.2 item 5): exact ref shape (relation type + name + uid), which relation types ship in v1alpha1 (likely none — NVLink/fabric domains arrive with Topograph/phase 5, VF→parent with the network class), and stale-ref behavior in envtest (target deleted → ref rewritten or dropped on next pass, never blocks, never 404s a consumer).
- **Minimalism test** (from §4 design principle): for each candidate field, name the consumer query or display that needs it; if none, drop it — adding later is additive, removing is breaking.
- **Size discipline**: v1alpha1 should stay around ~100 lines of schema; growth beyond that is the signal to exercise the pre-committed split (§3: extract `Accelerator` into its own kind).

## 5. Collector specification

### 5.1 Deployment shape

A **single cluster-scoped controller Deployment** (controller-runtime), leader-elected. It is NOT a DaemonSet: all sources are already-published Kubernetes objects (Nodes, NodeFeatures, ResourceSlices), readable cluster-wide. Local hardware access is never needed on cluster nodes. (Workstation ingestion is a separate path, §5.5.)

### 5.2 Sources and precedence (accelerator scope)

| Field | Classic node (ClusterPolicy) | DRA node (GPUCluster) |
|---|---|---|
| vendor, model, memory, architecture, computeCapability | GFD labels (`nvidia.com/gpu.product`, `gpu.memory`, `gpu.family`, `nvidia.com/gpu.compute.major/minor`) | ResourceSlice attributes (`productName`, `memory` capacity, `architecture`, `cudaComputeCapability`) |
| `id` (UUID) | GFD/NVML-derived labels where available; else PCI address fallback | ResourceSlice `uuid` attribute |
| pcie root/busID, numaNode | NFD `NodeFeature` `pci.device` instances | standardized `resource.kubernetes.io/pciBusID` / `pcieRoot` / `numaNode` |
| mig geometry | GFD `nvidia.com/mig-*` labels | `mig.nvidia.com` slice devices / parent GPU attributes |
| allocation plane | Node `status.allocatable` (nvidia.com/gpu present → classic) + GFD sharing labels | DeviceClasses present (`gpu.nvidia.com`, `mig.nvidia.com`) + ResourceSlices from `gpu.nvidia.com` |

**Merge rule: ResourceSlice > GFD > NFD** when multiple sources report the same device field (deterministic merge; record contributing sources in `provenance.sources`). In practice per-node exclusivity means one plane dominates per node, but the rule must still be deterministic.

**DRA-mode detection**: a node is in DRA mode for GPUs when ResourceSlices with `spec.driver: gpu.nvidia.com` name it (kubectl-nv's union-membership rule, inverted). Node allocatable containing `nvidia.com/gpu` → classic. Neither → surface nothing for that node (absence of data is **unknown, not zero** — see §5.4).

**Missing CRDs / RBAC**: if the `NodeFeature` CRD is absent (NFD not installed) or ResourceSlices don't exist (pre-1.34 cluster), the collector degrades to whichever sources exist. Degradation MUST be observable: emit an Event / log a condition-level signal on a collector ConfigMap or equivalent; never silently report empty inventory.

### 5.3 Object naming

`metadata.name = <class-prefix>-<id-slug>` where the slug is derived from `spec.id` (GPU UUID is globally unique → `gpu-6d0e4b2a`). When the vendor id is NOT globally unique (future: NIC PCI addresses, disk serials are usually unique but MACs aren't), prefix with the host slug: `nic-node01-0000-51-00-0`. Unit-test the exact group string and name derivation — group name typos are unfixable after rollout.

### 5.4 Lifecycle and write discipline

- **Create** on discovery; **update** when facts change; **delete** when the device disappears from its sources.
- **Heartbeat**: refresh `provenance.collectedAt` on every scan pass, but only write the object if (a) facts changed, or (b) `collectedAt` is older than `maxStale` (default 10m). This bounds API churn (NFD rewrites NodeFeature every 60s per node; our budget is far lower).
- **Freshness contract**: `collectedAt` is *collector observation time*, never copied from a producer (GFD's `gfd.timestamp` may be recorded in `attributes` if useful). A consumer reading a stale `collectedAt` must treat facts as stale, not re-stamp them.
- **Node deletion / name reuse**: devices whose `hostRef.uid` no longer matches a live Node are deleted after a grace period (default 15m). Node name reuse is detected via uid mismatch, never name.
- **Pool completeness (DRA)**: only consume a node's ResourceSlice pool when all slices at the highest `pool.generation` are present (`pool.resourceSliceCount` satisfied). An incomplete pool must NOT partially update devices and must NOT fall back to stale labels (NVSentinel's rule) — skip the node, keep previous facts, mark `provenance.sources` unchanged.
- **Identity anchor for updates**: match existing objects by `spec.id` + `hostRef`, not by name alone.

### 5.5 Workstation / non-cluster hardware (phase 3)

Workstations and cloud instances (the INFERENG-2022 coresite fleet) become HardwareDevices with `hostRef.kind: Host`, `allocation.mode: none`, sourced from the existing INFERENG-11440 telemetry pipeline (Ansible + Vector shipping DCGM/NVML facts) via an ingest bridge that creates/updates objects in the management cluster. Design constraint: the ingest path reuses `provenance.collectedAt` semantics (observation time at ingest). A future `HardwareHost` CRD (same group) may anchor host facts (CPU/RAM/location) and collector heartbeat — reserved, not shipped in v1alpha1.

### 5.6 RBAC

Collector (ClusterRole): `get/list/watch` on `nodes`, `nodefeatures.nfd.k8s.io`, `resourceslices.resource.k8s.io`, `deviceclasses.resource.k8s.io`; `create/update/delete` (or `*`) on `hardwaredevices.infrastructure.platform.opendatahub.io`.

Readers (consumers: config-matrix jobs, dashboards, test selection): `get/list/watch` on `hardwaredevices` only. **This is the point of the design** — consumers never need Node, NodeFeature, or ResourceSlice RBAC (cf. the node get/list RBAC paradox in `references/wiki/rhai/gpuaas/rhai-hwp-v2-redesign.md`).

Watch out for the group-name collision hazard: `infrastructure.platform.opendatahub.io` (this CRD) vs the existing `infrastructure.opendatahub.io` (HardwareProfile). Review every apiGroups string.

## 6. Delivery sequencing

1. **Phase 1 — classic sources**: CRD + collector reading GFD labels + Node status + NFD NodeFeature. Covers most of the current fleet. Ship the source-precedence table (§5.2) in the repo README — it satisfies RHAISTRAT-3018's "authoritative discovery sources are documented" criterion.
2. **Phase 2 — DRA source**: ResourceSlice ingestion (OCP 4.21+ / GPUCluster clusters). This is the piece no existing tool provides.
3. **Phase 3 — workstations**: ingest bridge over the INFERENG-11440 pipeline; `hostRef.kind: Host`; optional `HardwareHost` CRD.
4. **Phase 4 — `network` class**: NIC struct (model, speed, rdma, sriov) + `SriovNetworkNodeState`/dranet/dra-driver-sriov sources; enables the GPU–NIC `pcie.root` affinity query that P/D-disaggregation validation needs. `pcie.root` is captured from day one precisely for this.
5. Later: `storage`, `fabric` (Topograph) classes; RHACM Search / inventory-CR propagation for multi-cluster aggregation.

## 7. Testing strategy

- **envtest**: whatever validation/CEL rules the schema investigation lands on (field-name gotchas included), merge precedence, pool-completeness skip logic, node-reuse deletion, heartbeat write discipline.
- **Fixtures**: [ghostgpu](https://github.com/santimillang/ghostgpu) publishes fake GFD labels + extended resources + ResourceSlices — use it to CI both planes without GPUs. Also fixture Node/NodeFeature/ResourceSlice YAML directly.
- **Degradation tests**: NFD absent, ResourceSlice CRD absent, RBAC denied → unknown, never zero; observable degradation signal.
- **E2E**: kind cluster with labeled fake nodes; if available, one classic GPU cluster + one GPUCluster-mode cluster from the shared fleet.

## 8. Non-goals (v1alpha1)

- Allocation/admission/quota (RHAISTRAT-3020 owns it; Kueue ≥ v0.19 already bridges quota across planes).
- Health/utilization telemetry — that's the observability leg (DCGM → centralized observability stack; see Kyle Lape's comment on RHAISTRAT-3018 re Andy Linfoot's backing store). HardwareDevice carries facts, not metrics.
- Node-level facts (CPU/RAM) — stay on Node.status / future HardwareHost.
- Alpha DRA APIs (`ResourcePoolStatusRequest`, `DRANodeAllocatableResources`, DynamicMIG semantics).
- Auto-derivation of "validated" status from labels — the configuration matrix joins separately-versioned validation records; discovery ≠ validation.

## 9. Open questions (for the iterating agent / humans)

1. **Define the v1alpha1 schema** — run the §4.3 investigation (union mechanics, CEL field-name gotchas, Quantity representation, printer columns, required-field discipline, naming derivation) and propose the concrete CRD for review before implementation.
2. Does the collector run in each fleet cluster, or centrally with per-cluster kubeconfigs? (Per-cluster is simpler and matches RHACM propagation later; central avoids N deployments.)
3. `location` population: who owns it — fleet config map keyed by host, or Metal3/CMDB import?
4. MIG geometry on classic nodes: GFD mixed-MIG labels (`nvidia.com/mig-*.count`) vary by model/driver — needs fixture-driven normalization; the GPU dashboard work (`raw/arch/gpuaas/gpu-dashboard-data-sources.md`) already documents the wildcard-discovery approach.
5. Multi-writer discipline: if a second collector (e.g., SR-IOV shim for phase 4) writes the same kind, ownership is per-`provenance.driver` + per-class — spell out the partition rule before phase 4.
6. Architecture-forum blessing for the new `infrastructure.platform.opendatahub.io` group (implies `platform.opendatahub.io` as an umbrella domain) and cross-linking RHAISTRAT-2519 / RHAISTRAT-2382.

## 10. RHAISTRAT-3018 completion criteria mapping

| Criterion | How this design satisfies it |
|---|---|
| Initial inventory scope and authoritative discovery sources documented | §5.2 source-precedence table; phase 1 README |
| Users and automation can query GPU and NIC capabilities with collection timestamps | `kubectl get hwdev` + `provenance.collectedAt` (NIC in phase 4) |
| A configuration matrix identifies testable hardware combinations and gaps | flat queries over one kind (model/VRAM/MIG/host/pcie filters); validation records join separately |
| Inventory consumers use documented interfaces without duplicating a hardware registry | reader RBAC on `hardwaredevices` only; CRD + query examples are the interface |

## 11. References

- [RHAISTRAT-3018](https://issues.redhat.com/browse/RHAISTRAT-3018) — the initiative; [RHAISTRAT-2519](https://issues.redhat.com/browse/RHAISTRAT-2519) (product-side discovery controller, should become a consumer); [INFERENG-2022](https://issues.redhat.com/browse/INFERENG-2022), [INFERENG-11440](https://issues.redhat.com/browse/INFERENG-11440) (fleet inventory precedents)
- [GPU Operator 26.7 — DRA Driver for NVIDIA GPUs](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/26.7/dra-intro-install.html) — GPUCluster vs ClusterPolicy, GFD placement, exclusivity
- [DRA driver ResourceSlice attributes](https://dra-driver-nvidia-gpu.sigs.k8s.io/docs/reference/resourceslice-attributes/) — DRA-side field source
- [KEP-5004](https://github.com/kubernetes/enhancements/blob/master/keps/sig-scheduling/5004-dra-extended-resource/README.md), [DRA observability](https://kubernetes.io/docs/concepts/resource-management/dynamic-resource-allocation/dra-observability/) — bridging limits, alpha APIs to avoid
- [NFD custom resources](https://kubernetes-sigs.github.io/node-feature-discovery/master/usage/custom-resources.html), [GFD README](https://github.com/NVIDIA/k8s-device-plugin/blob/main/docs/gpu-feature-discovery/README.md) — classic-side field sources
- [NVSentinel](https://github.com/NVIDIA/NVSentinel), [Akri](https://github.com/project-akri/akri), [ghostgpu](https://github.com/santimillang/ghostgpu) — design/testing references
- Vault: `references/wiki/workload/dra/rhai-dra-gpu-partitioning.md`, `references/wiki/workload/dra/nvidia-dra-driver.md` (DRA/OCP maturity timeline), `references/wiki/rhai/gpuaas/rhai-hwp-v2-redesign.md` (RBAC paradox, CRD pattern analysis), `raw/arch/gpuaas/gpu-dashboard-data-sources.md` (classic-side query patterns, MIG wildcard discovery), `raw/arch/gpuaas/dra-sriov-gpudirect-alignment.md` (phase 4 GPU–NIC affinity context)
