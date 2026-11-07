# opendatahub-hardware-discovery-operator

A standalone, per-cluster hardware discovery service for OpenDataHub: it
enumerates whole physical or simulated GPUs on selected nodes and publishes
them as `HardwareDevice` custom resources, driven by one `HardwareDiscovery`
singleton (`cluster`) per cluster.

The approved PoC contract — API schemas, source profiles, lifecycle rules and
packaging — lives in [`docs/design/poc-contract.md`](docs/design/poc-contract.md).
This repository is a proof of concept: discovery mechanics are demonstrated
against the Run:ai fake-gpu-operator simulator on CPU-only Kind clusters; it
is not production classic hardware discovery.

## APIs

| CRD | Group | Scope | Purpose |
|---|---|---|---|
| `HardwareDevice` (`hwdev`, `hw`) | `infrastructure.platform.opendatahub.io/v1alpha1` | Cluster | One whole device: immutable identity, host, source locator; observed facts in status |
| `HardwareDiscovery` (fixed instance `cluster`) | same | Cluster | Singleton configuration: class, node selector, timings, sources |

Example CRs are in [`config/samples/`](config/samples/).

## Development

```sh
make manifests generate fmt vet lint test build   # everyday loop (cluster-free)
make verify                                       # all cluster-free checks incl. generation drift
```

`make manifests` regenerates the gitignored CRD bases and RBAC role from
markers. Unit tests use Gomega and the race detector and never require a
cluster, kubeconfig or network.

## Run the demonstration

Reproducing the two-plane demonstration from a clean clone:

```sh
make demo
```

Prerequisites: Linux/amd64, [podman](https://podman.io) 6.x (or docker),
~4 GiB free RAM, and nothing else — the harness builds the controller
image locally, creates a run-owned [Kind](https://kind.sigs.k8s.io)
cluster per profile (`kindest/node:v1.34.11`), installs the Run:ai
fake-gpu-operator simulator (test-only) and the service Helm chart, and
never touches your ambient kubeconfig. Each profile takes ~2 minutes;
evidence lands in `dist/e2e/`. `make test-e2e` additionally runs the
harness verification test (four cluster lifecycles total).

What the demo proves — discovery converges to the simulator facts,
identity stays immutable, heartbeats refresh within `maxStale`, failed
observations retain facts, DRA pools retain through incomplete
generations, selector changes prune and reselect, the singleton's
deletion garbage-collects its devices, and uninstall retains the CRDs —
is cataloged in [`docs/testing.md`](docs/testing.md).

Cleanup is automatic: every cluster is run-owned and torn down by the
harness (successful or not), and nothing is written to your ambient
kubeconfig. If you instead install the chart by hand on your own cluster
(`make helm-deploy`, which requires an explicit kubeconfig), the matching
`make helm-uninstall` removes the release while retaining the CRDs and
any live device objects.

## Reading the inventory

With the service installed and the `cluster` singleton applied (see
[`config/samples/`](config/samples/)):

```sh
kubectl get hardwarediscovery cluster -o yaml      # status.nodeSummaries: per-node scan overview
kubectl get hwdev -o wide                          # one line per whole GPU
kubectl describe hwdev <name>                      # facts, conditions, provenance
kubectl get hwdev <name> -o yaml                   # full status incl. collectedAt
```

`hwdev` devices are read-only inventory: identity and locator are
immutable spec, observed facts and `collectedAt` are status. `Ready`
means the observation succeeded — it says nothing about free capacity
(see [`docs/integration.md`](docs/integration.md)).

## Repository layout

See §3 of the design document for the package map and dependency boundaries.

## License

Apache-2.0. See [LICENSE](LICENSE).
