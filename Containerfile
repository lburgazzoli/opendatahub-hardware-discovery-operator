# Build the manager binary.
#
# Multi-platform builds are driven by Podman, e.g.:
#   podman build --platform linux/amd64,linux/arm64 --manifest <ref> .
#
# The builder stage always runs on the native platform (BUILDPLATFORM) so its
# RUN steps never require emulation, while TARGETOS/TARGETARCH select the
# architecture of the cross-compiled Go binary. The runtime parent resolves to
# the platform of the manifest child being built, and the final stage runs no
# commands, so no foreign-architecture execution is ever needed.
#
# Override BASE_IMAGE to build with another builder image, e.g.
# docker.io/library/golang:1.26
ARG BASE_IMAGE=registry.access.redhat.com/ubi10/go-toolset:latest
FROM --platform=$BUILDPLATFORM ${BASE_IMAGE} AS builder

# Go Toolset runs as UID 1001 by default while /workspace and the copy targets
# are root-owned; run the build as root so the workspace and the Go caches
# below are writable. This affects the builder stage only, never the final
# runtime image.
USER root

# Automatic platform args must be redeclared inside the stage to be visible to
# RUN. They select the target of the Go cross-compilation; the default target
# OS stays linux when unset.
ARG TARGETOS
ARG TARGETARCH

# Deterministic, writable cache locations for the root build user.
ENV GOCACHE=/tmp/go-build-cache \
    GOMODCACHE=/tmp/go-mod-cache

WORKDIR /workspace

# Copy the Go Modules manifests
COPY go.mod go.sum ./
# cache deps before building and copying source so that we don't need to
# re-download as much and so that source changes don't invalidate our
# downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build a static binary for the selected target platform; the builder itself
# keeps running natively, only the emitted binary architecture changes.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o /workspace/manager ./cmd

# Use UBI 10 Minimal as the runtime base. Podman resolves this parent to the
# platform of the manifest child being built, matching the Go binary above.
FROM registry.access.redhat.com/ubi10/ubi-minimal:latest

WORKDIR /

COPY --from=builder /workspace/manager .

# Numeric non-root identity; no passwd entry is required in UBI Minimal.
USER 65532:65532

# UBI Minimal inherits `CMD ["/bin/bash"]` from its parent layers; clear it so
# nothing is appended as an unexpected argument to the /manager entrypoint.
CMD []

ENTRYPOINT ["/manager"]
