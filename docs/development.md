# Development — runtime interface

This document records the delivered runtime interface of the hardware
discovery service (task 008). Identifiers are derived from the actual
manifests and runtime in this repository; packaging work (task 009)
consumes — never redefines — them.

## Runtime interface table

| Interface | Value | Source |
|---|---|---|
| Deployment | `opendatahub-hardware-discovery-operator-controller-manager` (namePrefix applied to `controller-manager`) | `config/manager/manager.yaml` via `config/default` |
| Container | `manager` | `config/manager/manager.yaml` |
| Container command | `[/manager, operator]` | `config/manager/manager.yaml`; root command in `cmd/main.go`, subcommand in `cmd/operator/` |
| Container args | `--leader-elect`, `--health-probe-bind-address=:8081`, `--metrics-bind-address=:8443`, `--metrics-secure`, `--webhook-port=-1` | `config/manager/manager.yaml` (single definition; the chart must not duplicate them in extra args) |
| Health port | `8081` (`/healthz`, `/readyz`) | manager arg + container port |
| Metrics port | `8443`, HTTPS with authn/authz filtering (metrics RBAC in `config/rbac/metrics_auth_role.yaml`) | manager arg + `config/default/metrics_service.yaml` |
| Webhook server | disabled (`--webhook-port=-1`); the service registers no webhooks and starts nothing certificate-dependent | `cmd/operator/options.go` |
| Service account | `opendatahub-hardware-discovery-operator-controller-manager` | `config/rbac/service_account.yaml` |
| Leader election | enabled by default; Lease ID `hardware-discovery.platform.opendatahub.io` in the resolved namespace | `cmd/operator/runtime.go` |
| Namespace inputs | precedence: `--namespace` flag → `APPLICATIONS_NAMESPACE` env (ODH orchestrator injection) → in-cluster pod namespace; failure is loud, never a silent `default` | `cmd/operator/options.go` (`resolveNamespace`) |
| `APPLICATIONS_NAMESPACE` behavior | places the leader-election Lease only. Both CRDs are cluster-scoped: the value never narrows a cache, a watch or reconciliation | `cmd/operator/options.go` |
| Cache policy | unrestricted (no default namespaces, no per-object selectors). The singleton's node selector is reconcile-time logic that can change without a restart | `cmd/operator/runtime.go` (`managerOptions`) |
| Registered controllers | exactly `inventory` and `device`, both through the framework builder (`ReconcilerFor` → `Build`) over the framework manager's cache-coherent client | `cmd/operator/registration.go` |
| Source inputs | one shared source factory over `mgr.GetAPIReader()` — scan-driven direct reads; no source informers, so an absent or forbidden optional API (resource.k8s.io/v1) never blocks manager startup | `cmd/operator/registration.go` |
| Concurrency | inventory scans serialized per kind (`HardwareDiscovery.infrastructure.platform.opendatahub.io` = 1) even under a larger global default; device controller keeps the global default | `internal/controller/inventory` (`ManagerControllerOptions`) |
| Singleton creation | never at process startup; the singleton is publisher/orchestrator-owned input | runtime creates no HardwareDiscovery |
| Image substitution | owned by packaging: `RELATED_IMAGE_HARDWARE_DISCOVERY_CONTROLLER` / chart `controllerImage.{repository,tag}` as whole values. The runtime itself consumes no related images (it deploys nothing) | task 009 (`packaging/module.yaml`) |
| Image | `controller:latest` in source; overridden by `make build-installer IMG=...` / chart values | `config/manager` |

## Build and run

- `make build` — builds `bin/manager` from `./cmd`.
- `make run` — runs the `operator` subcommand from the host (requires a
  resolvable kubeconfig and namespace; see namespace inputs above).
- `make build-installer` — renders `dist/install.yaml` (CRDs + RBAC +
  Deployment). It never contacts a cluster.
- There are deliberately no `install`/`deploy`/`uninstall`/`undeploy`
  targets: cluster mutation belongs to the Helm chart and the Kind
  harness, which require an explicit kubeconfig/context.

## Dockerfile

The image builds from clean source (`.dockerignore` filters local
state) and runs as non-root distroless with a read-only root filesystem;
the security context in `config/manager/manager.yaml` follows the
restricted Pod Security Standard. The image contains only the manager
binary — no simulator or driver artifacts.

## Packaging (task 009)

Helm is the deployment vehicle for the service itself. The chart is
generated into `dist/chart/` by the pinned Kubebuilder `helm/v2-alpha`
plugin (v4.16.0) from the same source config as `dist/install.yaml`,
then curated to the packaging contract; its templates and preserved
metadata are committed and reviewed like source. The plugin's own
Makefile block (curl-based `install-helm`, colon-splitting image
overrides, ambient-kubeconfig lifecycle targets) and its generated
`.github/workflows/test-chart.yml` were reviewed and removed —
`make/helm.mk` is the single curated Helm make definition.

Cluster-free pipeline (no target here contacts a cluster):

```sh
make manifests generate build
make helm-chart          # regenerate dist/chart (plugin) + curate
make helm-review-chart   # regenerate and require byte-identity with the committed chart
make helm-lint           # helm lint dist/chart (pinned Helm v4.3.1 via go run)
make helm-render         # helm template -> dist/rendered.yaml
make helm-package        # helm package -> dist/*.tgz + archive inventory and standalone checks
make module-lint         # odh-platform-tools `module lint` against packaging/module.yaml
```

Lifecycle targets (`helm-deploy`, `helm-status`, `helm-history`,
`helm-rollback`, `helm-uninstall`) are deliberately inconvenient: every
one requires an explicit `KUBECONFIG` and `HELM_KUBECONTEXT`, and the
mutating ones additionally require `HELM_CHART_REVIEWED=yes` after
`make helm-review-chart`. The controller image is passed as whole
`CONTROLLER_IMAGE_REPOSITORY` / `CONTROLLER_IMAGE_TAG` values — never
split from a combined reference.

### Package-contract checklist

| ODH requirement | Delivered state |
|---|---|
| Module CRD identity | `packaging/module.yaml` selects the singleton GVK `HardwareDiscovery` (`infrastructure.platform.opendatahub.io/v1alpha1`), instance name `cluster` (CEL-enforced). The chart bundles exactly that CRD plus the `HardwareDevice` inventory CRD (the one additionally permitted kind). Module CRD and inventory CRD stay distinct: the module CR the ODH orchestrator reconciles is the HardwareDiscovery singleton, never a device. |
| Singleton absence | The chart ships no `HardwareDiscovery` instance and the module tool creates no module CR; the singleton is publisher/orchestrator-owned input. Samples apply separately after CRDs are established. |
| Chart / Deployment / container inputs | One Deployment `opendatahub-hardware-discovery-operator-controller-manager`, container `manager`, command `[/manager, operator]` — byte-identical names to `dist/install.yaml` and the runtime interface table above, because the chart pins `fullnameOverride` (release-name independent). Chart-rendered args (`--metrics-bind-address=:8443`, `--health-probe-bind-address=:8081`, `--webhook-port=-1`, `--leader-elect`; `--metrics-secure` defaults to true in the CLI) are the canonical runtime flags; they are never duplicated in `manager.args`. |
| Image inputs | Chart values `controllerImage.{repository,tag}` as whole values (no colon splitting; a digest reference goes in the repository and suppresses the tag; registry ports render correctly). Module related image `RELATED_IMAGE_HARDWARE_DISCOVERY_CONTROLLER` with a whole `{{...}}` token resolved at deploy time. No production ODH image digest or source pin exists yet — the placeholder `controller` repository is render-time fallback only, and none was invented. |
| Namespace / config alignment | The Deployment lands in the release namespace; `APPLICATIONS_NAMESPACE` injection places only the leader-election Lease (both CRDs are cluster-scoped); namespace precedence is owned by the runtime (`cmd/operator/options.go`), not the chart. |
| Service permissions | Controller ClusterRole limited to real inputs/status/children (nodes; hardwaredevices incl. `/status`; hardwarediscoveries incl. `/status`; resourceslices; events), the leader-election Role (configmaps/leases/events) and metrics auth/reader roles. No framework deploy/label-GC privileges, no simulator ConfigMap reads, no wildcards. |
| CRD lifecycle | CRDs ship as reviewed generated templates in `templates/crd/` with `helm.sh/resource-policy: keep` (retained on uninstall); no routine CRD deletion, no automatic adoption, no mixing Helm release resources with direct Kustomize apply/delete of the same objects. |
| Lease | Not shipped (not on the module-lint allowlist); created at runtime by the leader-electing manager. |
| Chart envelope scope | This chart is the operator-installation envelope (controller + CRDs + RBAC). It deploys the discovery service, never operands: discovery controllers render/deploy no Helm charts themselves. Future ODH orchestration and release/image publishing ownership is out of scope; module-lint success is packaging evidence, not orchestrator onboarding and not a runtime check. |

### Packaging notes and known constraints

- The module-lint tool (`odh-platform-tools` at pseudo-version
  `v0.0.0-20261028215308-948ecd9b3ace`) cannot be built from the module
  proxy: its CRD bases are gitignored upstream but `go:embed`ed.
  `make module-lint` therefore verifies the pinned commit of the local
  checkout (`ODH_PLATFORM_TOOLS_DIR`, default
  `~/work/dev/rhai/odh-platform-tools`), extracts exactly that commit
  into `dist/module-tool`, generates the tool's own CRDs there and runs
  it from there — the user's checkout is never mutated.
- The module tool renders charts in strict template mode
  (`missingkey=error`), so `values.yaml` declares every referenced key;
  empty values keep the same "unset" semantics for the Helm CLI.
- Regeneration is deterministic: `make helm-chart` is idempotent and
  `make helm-review-chart` fails on any drift between the regenerated
  chart and the committed reviewed one.
