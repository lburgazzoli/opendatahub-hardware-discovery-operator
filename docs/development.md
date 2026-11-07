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
