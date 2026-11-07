# Development

Durable contributor rules. Mechanics, pins and defaults live in the code:
`go.mod`, the `Makefile` and its `make/` fragments, and `hack/scripts/`
are the source of truth — never copy their tables here. Run `make help`
for the full target list.

## Everyday loop

```sh
make manifests generate fmt vet lint test   # cluster-free checks
make verify                                 # the above plus tracked-tree drift
make build                                  # bin/manager
make build-installer IMG=...                # dist/install.yaml (never contacts a cluster)
```

- Tools run as pinned `go run module@version` — never `@latest`, unpinned
  curl installers, or hidden local installations.
- `make verify` checks that regeneration leaves the tracked tree
  byte-identical; it does not regenerate the Helm chart (that is
  `make helm-chart`, reviewed separately).
- There are deliberately no `install`/`deploy`/`undeploy` targets:
  cluster mutation belongs to the Helm chart and the Kind harness, which
  require an explicit kubeconfig/context.

## Architecture rules

- **Framework-first.** Manager, client, builder and action pipelines come
  from `odh-platform-utilities/framework`; register controllers through
  the framework builder, keep the inventory scan serialized
  (`GroupKindConcurrency["HardwareDiscovery…"] = 1`), and preserve
  cache/watch representation coherence. The service deploys no operands
  and has no finalizer responsibilities; node selection happens at
  reconcile time so it can change without a restart.
- **Root-module imports need approval.** Direct imports from the root
  `odh-platform-utilities` module (including convenience imports of its
  constants/types) require substantive justification and review; do not
  assume named root/framework types are interchangeable — use the actual
  framework contracts.
- **Core purity.** `pkg/discovery` imports only the standard library and
  `apimachinery` (enforced by `TestPackageBoundary`); domain enums and
  facts convert to API types at the codec/controller boundary, never
  inside the core. API packages hold definitions, accessors and generated
  plumbing — no business logic, no tests.
- **Sources stay read-only and optional.** A missing or forbidden
  optional source API (e.g. `resource.k8s.io/v1`) must not block manager
  startup.

## Code style

- Explicit injected dependencies and clocks; copy data before mutation;
  writes are idempotent; errors are wrapped, contextual and classifiable
  with `errors.Is`.
- Every new or touched parameter declares its own type (no boolean
  sandwiches, no struct-blob parameters).
- Prefer existing meaningful abstractions; no trivial forwarding or
  nil-guard wrapper layers, no speculative frameworks, no unrelated
  refactors.
- Typed API boundaries, minimal additive supported facts, scoped RBAC.
  Identity, class and host UID are immutable — that does not make every
  spec/locator field immutable (the inventory controller maintains device
  specs). Conditions are the contract; `status.phase` is framework-derived.

## Packaging

- Generated artifacts (`config/crd/bases/`, `config/rbac/role.yaml`,
  chart templates) come from source manifests and markers: never
  hand-edit them. Regenerate with `make manifests generate`, then
  `make helm-chart` (plugin generation plus curation).
- `dist/chart/` is reviewed, tracked output: `make helm-review-chart`
  regenerates and requires byte-identity with the committed chart.
  `make helm-lint`, `make helm-render`, `make helm-package` and
  `make module-lint` are the remaining cluster-free packaging targets.
- Controller image values stay whole (`controllerImage.repository` /
  `.tag`, never colon-split from a combined reference).
- Lifecycle targets (`helm-deploy` etc., in `make/helm.mk` with shell
  logic in `hack/scripts/`) are deliberately inconvenient: explicit
  `KUBECONFIG` and `HELM_KUBECONTEXT` always, `HELM_CHART_REVIEWED=yes`
  for mutating ones.
- `make module-lint` requires a pinned external checkout of
  `odh-platform-tools` (`ODH_PLATFORM_TOOLS_DIR`) — it is not
  self-contained, and passing it is packaging evidence, not completed
  ODH onboarding. See `packaging/module.yaml`.

## Hygiene

Never commit `.context/`, credentials, kubeconfigs, binaries, or `dist/`
outputs other than the reviewed `dist/chart/` source. Do not encode old
task IDs, workstation paths or plan-run orchestration rules as project
policy.
