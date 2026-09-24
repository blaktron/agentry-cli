# syntax=docker/dockerfile:1
#
# The agentry-runner image (RUNNER-PLAN §R7). This file ships EMBEDDED in
# the agentry CLI: `runnerimage.Materialize` writes this build context to
# ~/.agentry/runner/image/ on first use, and the engine builds
# `agentry-runner:<version>` locally from it. Treat the materialized copy
# as generated output — the sources of record live under runnerimage/ in
# the agentry-cli repo.
#
# The context is self-contained on purpose (RQ1): module sources plus a
# generated go.mod, no third-party imports, so the build needs network
# access for the two public base images below and for nothing else — no
# module proxy, no registries of ours.
#
# One binary serves the `tools` (MCP stdio), `proxy` (CONNECT allowlist),
# and `probe` roles; the gateway and the engine pick the role via argv.
#
# The runtime stage stays root DELIBERATELY in v1: the gateway bind-mounts
# each run's workspace/out directories with whatever ownership the host
# gave them, and a fixed non-root UID would turn every uid-mismatched host
# into EACCES at the first fs_write. The gateway launches tool servers
# with no-new-privileges, the container is one-shot, and §R11's mount
# table — not the container user — is the enforcement boundary.

# Both bases come from Agentry's own mirrors (blaktron/agentry-dockerimages),
# digest-verified equal to the upstream images they mirror, so this embedded
# context pulls nothing from Docker Hub — which rate-limits anonymous pulls at
# 100 per hour per IP, a limit one build of this image can reach on a shared
# address. Both are pinned by digest (name:tag@sha256:…, the digests recorded
# in that repository's images.tsv; agentry-cli#269, DR-32): the digest fixes
# the bytes, and the tag is only a label for a reader. GO_IMAGE is overridden
# at build time from the policy's images.goBuilder; RUNTIME_IMAGE is an
# argument for the same reason, so an operator who mirrors elsewhere can point
# both at their own registry.
ARG GO_IMAGE=ghcr.io/blaktron/agentry-golang-alpine:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414
ARG RUNTIME_IMAGE=ghcr.io/blaktron/agentry-alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /agentry-runner ./runnerimage
# Assert the artefact is an executable ELF binary before anything copies it
# (agentry-cli#235). Not redundant: in the agentry-cli repo runnerimage/main.go
# carries `//go:build ignore` and runnerimage.Materialize strips it, and
# neither is visible from here. Build this Dockerfile from the repository root
# instead of from the materialized context and ./runnerimage is a LIBRARY
# package: `go build -o` then writes a non-executable `!<arch>` archive, exits
# 0, and the image builds "successfully" with an entrypoint that cannot run.
# `test -x` rules out the 0644 archive; the ELF magic rules out anything else
# that is not a Linux binary (an archive someone chmodded, a truncated file).
# TestDockerfileAssertsExecutable runs this exact line against both kinds.
RUN test -x /agentry-runner && [ "$(head -c 4 /agentry-runner)" = "$(printf '\177ELF')" ] || { echo "agentry-runner: the build produced no executable ELF binary (a library archive?) - build from the context runnerimage.Materialize writes, not the repository root" >&2; exit 1; }

FROM ${RUNTIME_IMAGE}
# ca-certificates: fetch and probe speak TLS from inside this image; the
# llm-proxy only tunnels, but the CA bundle costs nothing and removes a
# whole class of "works on host, fails in container" reports.
RUN apk add --no-cache ca-certificates
COPY --from=build /agentry-runner /usr/local/bin/agentry-runner
ENTRYPOINT ["/usr/local/bin/agentry-runner"]
