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

## Repository layout

See §3 of the design document for the package map and dependency boundaries.

## License

Apache-2.0. See [LICENSE](LICENSE).
