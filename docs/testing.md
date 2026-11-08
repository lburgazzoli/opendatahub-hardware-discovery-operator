# Testing

Testing rules and the essential commands. What the suites prove — and
deliberately do not — is summarized at the end.

| Level | Command | Needs | Proves |
|---|---|---|---|
| Unit | `make test` | nothing (no cluster, no kubeconfig; initial tool downloads may need network) | Controllers, codecs, identity, lifecycle and observation semantics — with the race detector |
| Drift | `make verify` | nothing | Regeneration leaves the tracked tree byte-identical (does not regenerate the chart) |
| e2e compile | `make test-e2e-compile` | nothing | The e2e-tagged suite builds and vets |
| Container workflow | `make test-container-workflow` | nothing (no engine, no network) | The build/publish workflow contract: two-platform manifests, `BASE_IMAGE` forwarding, all-member push, override and non-Podman rejection, run-scoped ttl.sh reference grammar, fail-closed behavior |
| Demonstration | `make demo` | podman 6.x, ~4 GiB RAM, network (ttl.sh publish and image pulls) | Both simulator profiles through the Helm chart on run-owned Kind clusters |
| Full e2e gate | `make test-e2e` | same as the demonstration | The demonstration plus harness verification |

## Unit rules

- **Observe behavior, not structure.** Assert on objects produced through
  the production reconciliation paths of fake clients. Interceptor-wrapped
  clients are the sanctioned instrumentation for bounded failure, write
  and race checks. No bespoke SSA emulators, no reflection into
  controller-runtime or framework internals, no started-controller
  composition tests, no retesting library implementation details.
- **Fixtures never share the implementation's oracle.** Expected
  identities, names and facts are derived independently in the test
  (e.g. hash-truncated device names recomputed from the documented
  derivation), never read from the production constants the assertion
  would circularly confirm.
- **Expectations are readable at the call site.** Plain Go testing with
  Gomega; compose the existing jq/k8s/condition matchers
  (`lburgazzoli/gomega-matchers`) instead of hand-rolled deep compares.
  Map-backed conditions hold JSON strings: pass
  `string(metav1.ConditionTrue)`, not the typed enum.
- **No fixed sleeps, no list-order assumptions.** Inject clocks; poll
  with bounded contextual `Eventually` and fresh reads. Timeouts are
  failure-detection bounds, never scheduling mechanisms.
- **Helpers stay proportional.** Fixtures take the testing context
  explicitly; they never store `testing.T` or `Gomega` instances.
- **Scope discipline.** Discovery e2e tests never seed expected
  `HardwareDevice` outputs (independent of implementation normalizers);
  lifecycle/device unit tests may seed existing devices. No tests under
  `api/`.
- Add regression tests for changed behavior — especially
  unknown-versus-empty retention, lifecycle and status ownership.

## What fakes cannot prove

Fake clients prove project logic only. Real API defaulting, CEL, SSA,
RBAC and GC semantics are proven by the Kind e2e suite. Do not impose
`managedFields` or `status.phase` assertions as standing policy.

## E2e rules

- The suite (tag `e2e`, `test/e2e/`) installs the reviewed Helm chart
  with its own ServiceAccount/RBAC plus the test-only fake-gpu-operator
  simulator on separate run-owned classic/DRA Kind clusters. The service
  controller image is built for both `linux/amd64` and `linux/arm64`
  and published under a fresh run-scoped anonymous ttl.sh reference
  (six-hour expiry); each cluster pulls the matching native child. The
  pinned Kind node image is still loaded locally as harness cargo.
  Simulator source pins, frozen profiles and the fact-mutation protocol
  live in `test/support/`.
- `make test-e2e` and `make demo` own their image reference: any
  explicitly supplied `E2E_CONTROLLER_IMAGE*` value (environment or
  command line) is rejected before side effects. Standalone
  `make e2e-image` accepts such overrides for local, never-published
  builds only. Direct tagged-suite invocations require explicit whole
  separate `E2E_CONTROLLER_IMAGE_REPOSITORY`/`E2E_CONTROLLER_IMAGE_TAG`
  values.
- Private kubeconfigs under `/tmp`; the ambient kubeconfig is never
  touched. Bounded operations only: no ambient-cluster or prefix-wide
  deletion, no fake ResourceSlice CRD, no real GPU/DCGM/CUDA stack.
  Missing prerequisites (runtime, network, published image) are
  failures, never skips.
- On failure, collect diagnostics (events, pod/node logs) for the exact
  run-owned cluster before tearing only that cluster down.

## Evidence honesty

Distinguish compiled, executed and not-run checks in any report; do not
maintain historical pass logs or scenario catalogs as documentation.
Executed results live in the run's evidence documents under `dist/e2e/`
(gitignored), not in this file.
