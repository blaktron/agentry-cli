# The agentry-runner image. This file ships EMBEDDED in
# the agentry CLI: `runnerimage.Materialize` writes this build context to
# ~/.agentry/runner/image/ on first use, and the engine builds
# `agentry-runner:<version>` locally from it. Treat the materialized copy
# as generated output — the sources of record live under runnerimage/ in
# the agentry-cli repo.
#
# The context is self-contained on purpose: module sources plus a
# generated go.mod, no third-party imports, so the build needs no module
# proxy. What it does fetch: the two base images below, the Typst image and
# the ripgrep image, which come from Agentry's registry
# (ghcr.io/blaktron), and nothing else. The
# file deliberately carries no `# syntax=` parser directive: one would make
# BuildKit pull its frontend image (docker.io/docker/dockerfile:1, by a tag
# that can move) before every build and let that image's code parse this
# file, and nothing here needs more than the daemon's built-in frontend
# (TestDockerfileNamesNoExternalFrontend holds it).
#
# One binary serves the `tools` (MCP stdio), `proxy` (CONNECT allowlist),
# and `probe` roles; the gateway and the engine pick the role via argv.
#
# The runtime stage stays root DELIBERATELY: the gateway bind-mounts
# each run's workspace/out directories with whatever ownership the host
# gave them, and a fixed non-root UID would turn every uid-mismatched host
# into EACCES at the first fs_write. The gateway launches tool servers
# with no-new-privileges, the container is one-shot, and the mount
# table, not the container user, is the enforcement boundary.

# Both bases come from Agentry's own mirrors (blaktron/agentry-dockerimages),
# digest-verified equal to the upstream images they mirror, so neither base is
# pulled from Docker Hub, which rate-limits anonymous pulls at 100 per IP (the
# registry's header gives the window as an hour, Docker's documentation as six
# hours; a shared address can reach either). With no `# syntax=` directive
# (above), nothing else is either. Both bases are pinned by digest
# (name:tag@sha256:…, the digests recorded in that repository's images.tsv):
# the digest fixes the bytes, and the tag is only a label for a reader.
#
# The two arguments are NOT equally reachable. GO_IMAGE is set by the CLI on
# every build from the policy's images.goBuilder, so an operator repoints the
# builder in policy.yaml. RUNTIME_IMAGE has no policy field and the CLI never
# passes it: the default below is always what a run's image is built on, and
# repointing it means building this context yourself with
# --build-arg RUNTIME_IMAGE=… and naming the result in the policy's
# images.runner. Nothing in policy.yaml changes it.
ARG GO_IMAGE=ghcr.io/blaktron/agentry-golang-alpine:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb
ARG RUNTIME_IMAGE=ghcr.io/blaktron/agentry-alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
# export_pdf's renderer: agentry-dockerimages' files-only image of Typst
# built from pinned source without its package downloader, the Noto fonts
# for every script, the report designs' fourteen open-licence families and
# the vendored cmarker, signed by its typst workflow on main
# (scripts/verify-typst-image.sh holds the pin to that signature). Like
# RUNTIME_IMAGE it has no policy field: repointing it means building this
# context yourself.
ARG TYPST_IMAGE=ghcr.io/blaktron/agentry-typst:2026.10.30.3@sha256:2ab41811f54525a0e29256e3d5fc7a9dee79ff8a5f6e6b45f89f5cebe1c63e42
# grep and glob's search engine: agentry-dockerimages' files-only image of
# ripgrep 15.2.0 built from pinned source with its default features (no
# PCRE2), static, signed by its ripgrep workflow on main
# (scripts/verify-ripgrep-image.sh holds the pin to that signature). Like
# TYPST_IMAGE it has no policy field.
ARG RIPGREP_IMAGE=ghcr.io/blaktron/agentry-ripgrep:2026.10.30.3@sha256:afa712469aa8f18c42c3f2729d731be2f3c53a3c7f0fa9c70242c106c26c812e
FROM ${TYPST_IMAGE} AS typst
FROM ${RIPGREP_IMAGE} AS ripgrep

FROM ${GO_IMAGE} AS build
# Every image the runner builds carries org.agentry.image=<kind>, in every
# stage that runs, so a stage the classic builder leaves untagged is found
# by label too (pkg/runner/imagelabel).
LABEL org.agentry.image=runner
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /agentry-runner ./runnerimage
# Assert the artefact is an executable ELF binary before anything copies it.
# Not redundant: in the agentry-cli repo runnerimage/main.go
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
LABEL org.agentry.image=runner
# ca-certificates: fetch and probe speak TLS from inside this image; the
# llm-proxy only tunnels, but the CA bundle costs nothing and removes a
# whole class of "works on host, fails in container" reports.
RUN apk add --no-cache ca-certificates
COPY --from=build /agentry-runner /usr/local/bin/agentry-runner
# Outside every bin directory (toolsrv.TypstDir): an exec image's minimize
# keeps it, and no declared command finds it on a PATH. The licences travel
# with the binary and the fonts.
COPY --from=typst /typst /opt/agentry/typst/typst
COPY --from=typst /fonts /opt/agentry/typst/fonts
COPY --from=typst /typst-packages /opt/agentry/typst/packages
COPY --from=typst /licenses /opt/agentry/typst/licenses
# Outside every bin directory too (toolsrv.RipgrepDir): only the tools
# server runs it, for grep and glob, never a declared command.
COPY --from=ripgrep /rg /opt/agentry/ripgrep/rg
COPY --from=ripgrep /licenses /opt/agentry/ripgrep/licenses
ENTRYPOINT ["/usr/local/bin/agentry-runner"]
