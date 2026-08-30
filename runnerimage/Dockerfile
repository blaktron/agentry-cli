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
ARG GO_IMAGE=golang:1.27-alpine
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /agentry-runner ./runnerimage

FROM alpine:3.22
# ca-certificates: fetch and probe speak TLS from inside this image; the
# llm-proxy only tunnels, but the CA bundle costs nothing and removes a
# whole class of "works on host, fails in container" reports.
RUN apk add --no-cache ca-certificates
COPY --from=build /agentry-runner /usr/local/bin/agentry-runner
ENTRYPOINT ["/usr/local/bin/agentry-runner"]
