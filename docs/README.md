# Hardware discovery service

Orientation for agents and contributors: what this project is, how it is
built, and where the rules live. Mechanics live in the code — this file
stays a map, not a second implementation.

- Contributor rules: [`development.md`](development.md)
- Testing rules: [`testing.md`](testing.md)

## What the service is

A per-cluster, read-only hardware inventory publisher for OpenDataHub.
One cluster-scoped `HardwareDiscovery` singleton (fixed name `cluster`)
selects nodes and configures discovery sources; the service enumerates
whole GPUs on those nodes and publishes each as a cluster-scoped
`HardwareDevice` owned by the singleton.

Inventory is existence and facts, not health, utilization, free capacity,
scheduling, or hardware validation. `Ready` on a device means the
observation succeeded — consumers that need liveness check
`status.collectedAt` against `spec.maxStale` themselves.

This is a proof of concept: discovery mechanics are demonstrated against
the Run:ai fake-gpu-operator simulator on CPU-only Kind clusters, not
against production GPU stacks.

## Architecture

One leader-elected process runs two controllers, both registered through
the odh-platform-utilities framework builder in
[`cmd/operator/registration.go`](../cmd/operator/registration.go):

- **inventory** ([`internal/controller/inventory/`](../internal/controller/inventory/)) —
  owns the singleton's node selection and scan, device lifecycle
  (create/prune), device spec/metadata maintenance, and singleton status.
  Scans are serialized (per-kind concurrency 1): one singleton, one writer.
- **device** ([`internal/controller/device/`](../internal/controller/device/)) —
  observes one device and writes its `/status` only (facts, `collectedAt`,
  conditions through the framework's status apply).

The core domain lives in [`pkg/discovery/`](../pkg/discovery/) and stays
API- and controller-independent (stdlib + apimachinery only, enforced by
test): identity derivation, facts, locators, and plane resolution as a
source-driven priority fold. Sources are pluggable `Locator`
implementations — `pkg/discovery/classic` (Node extended resources and
GFD-style labels) and `pkg/discovery/dra` (native
`resource.k8s.io/v1` ResourceSlices of allow-listed drivers). Production
reads only Nodes and ResourceSlices; the simulator is test-only
([`test/support/`](../test/support/)).

DRA evidence takes precedence over classic; an incomplete selected DRA
pool yields retention, never partial updates or fallback.

### Safety boundaries

- **Unknown beats empty.** Loss or ambiguity of evidence retains devices
  and facts — it never fabricates a zero. Only a read that is
  disappearance proof for its source kind prunes: a successful DRA
  zero-advertisement read can; missing or malformed classic `gpu.count`
  evidence cannot (a valid classic zero differs from an unreadable one).
- **Node identity is uid-scoped.** `spec.host.uid` is immutable; node
  replacement is delete + recreate of the device. Same-name device
  replacement may transiently receive stale status; the accepted model is
  convergence through fresh evaluation, not an atomic per-write UID guard.
- **Ownership does the cleanup.** Deleting the singleton garbage-collects
  owned devices through Kubernetes owner references; the service has no
  finalizer and never adopts or prunes foreign objects.
- **Failed observations retain facts and `collectedAt`.** `collectedAt` is
  collector-owned and advances only on initial, changed, or
  heartbeat-expired successful observations; ordinary framework status
  applies do not imply fact refreshes.

### Identity caveats

Classic identity is a synthetic node-scoped index (`<node>-gpu-<index>`)
because stock GFD/NFD labels are homogeneous per node and publish no
per-GPU UUID. The simulator's DRA UUIDs are simulator-generated and do
not prove real vendor identity extraction.

## Packaging and deployment

The Helm chart ([`dist/chart/`](../dist/chart/)) installs the controller,
CRDs and RBAC — never the `cluster` singleton, which is
publisher/orchestrator-owned input (see
[`config/samples/`](../config/samples/)). CRDs are retained on uninstall.
Kustomize sources under [`config/`](../config/) generate
`dist/install.yaml`. The controller image is built with Podman only,
for both `linux/amd64` and `linux/arm64`, and published separately
(`make container-build` / `make container-push`; e2e runs use fresh
ephemeral ttl.sh references — see
[`development.md`](development.md), Packaging).
[`packaging/module.yaml`](../packaging/module.yaml)
is ODH module metadata: integration into `opendatahub-io/opendatahub-operator`
is the destination, not completed onboarding.

Known limitations: simulator-only proof (no production hardware or
upgrade/migration guarantee); outstanding ODH operator-side registration
and image wiring; the framework event recorder lacks events RBAC in the
`default` namespace for cluster-scoped `HardwareDevice` events (known,
deliberately unfixed here — belongs to a chart/operator review).

## Code map

| Path | Contents |
|---|---|
| `api/infrastructure/v1alpha1/` | CRD types, CEL validation, generated plumbing |
| `cmd/`, `cmd/operator/` | entrypoints, controller registration, runtime options |
| `internal/controller/{inventory,device}/` | the two controllers |
| `pkg/discovery/` | pure domain core; `classic/` and `dra/` source packages |
| `config/` | Kustomize sources, RBAC, samples |
| `hack/scripts/` | reviewed shell workflow: image build/publish, e2e orchestration, chart curation, their offline contract tests |
| `dist/chart/` | reviewed, generated-then-curated Helm chart |
| `packaging/module.yaml` | ODH module metadata |
| `test/` | unit tests, e2e suite (`test/e2e/`), simulator and harness support |
