# opendatahub-hardware-discovery-operator

A standalone, per-cluster hardware discovery service for OpenDataHub: it
enumerates whole physical or simulated GPUs on selected nodes and publishes
them as `HardwareDevice` custom resources, driven by one `HardwareDiscovery`
singleton (`cluster`) per cluster.

This repository is a proof of concept: discovery mechanics are demonstrated
against the Run:ai fake-gpu-operator simulator on CPU-only Kind clusters; it
is not production classic hardware discovery.

Start with [`docs/README.md`](docs/README.md) — what the service is and is
not, the architecture, safety boundaries and a code map. Contributor rules
are in [`docs/development.md`](docs/development.md); testing rules and
verification levels in [`docs/testing.md`](docs/testing.md).

## APIs

| CRD | Group | Scope | Purpose |
|---|---|---|---|
| `HardwareDevice` (`hwdev`, `hw`) | `infrastructure.platform.opendatahub.io/v1alpha1` | Cluster | One whole device: identity, host, source locator in spec; observed facts in status |
| `HardwareDiscovery` (fixed instance `cluster`) | same | Cluster | Singleton configuration: class, node selector, timings, sources |

Example CRs are in [`config/samples/`](config/samples/).

## Development

```sh
make manifests generate fmt vet lint test   # everyday loop (cluster-free)
make verify                                 # all cluster-free checks incl. generation drift
```

`make manifests` regenerates the gitignored CRD bases and RBAC role from
markers. Unit tests use Gomega and the race detector and never require a
cluster, kubeconfig or network.

## Run the demonstration

```sh
make demo
```

Prerequisites: Linux, [podman](https://podman.io) 6.x (the only supported
container engine), ~4 GiB free RAM, and network access — the harness
builds the controller image for both `linux/amd64` and `linux/arm64`,
publishes it under a fresh anonymous [ttl.sh](https://ttl.sh) reference
(unique per run, six-hour expiry, no secrets in the image), creates a
run-owned [Kind](https://kind.sigs.k8s.io) cluster per profile
(`kindest/node:v1.34.11`), and the cluster pulls the matching native
child. It installs the Run:ai fake-gpu-operator simulator (test-only)
and the service Helm chart, and never touches your ambient kubeconfig.
Each profile takes ~2 minutes; evidence lands in `dist/e2e/`.
`make test-e2e` additionally runs the harness verification test (four
cluster lifecycles total).

Cleanup is automatic: every cluster is run-owned and torn down by the
harness (successful or not). If you instead install the chart by hand on
your own cluster (`make helm-deploy`, which requires an explicit
kubeconfig), the matching `make helm-uninstall` removes the release while
retaining the CRDs and any live device objects.

## Reading the inventory

With the service installed and the `cluster` singleton applied (see
[`config/samples/`](config/samples/)):

```sh
kubectl get hardwarediscovery cluster -o yaml      # status.nodeSummaries: per-node scan overview
kubectl get hwdev -o wide                          # one line per whole GPU
kubectl describe hwdev <name>                      # facts, conditions, provenance
kubectl get hwdev <name> -o yaml                   # full status incl. collectedAt
```

`hwdev` devices are read-only inventory: `Ready` means the observation
succeeded — it says nothing about health or free capacity, and consumers
check `collectedAt` freshness themselves (see
[`docs/README.md`](docs/README.md)).

## License

Apache-2.0. See [LICENSE](LICENSE).
