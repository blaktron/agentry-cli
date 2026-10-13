#!/usr/bin/env bash
# Hold the runner image's pinned ripgrep image to its signature, and extract
# its binary for the real-search tests (agentry-cli#912; plan
# ripgrep-search.md D3).
#
# The runner image copies grep and glob's ripgrep from agentry-ripgrep by
# digest (runnerimage/Dockerfile, ARG RIPGREP_IMAGE), so the bytes a run
# searches with are the bytes this pin names; this check makes sure the pin
# names bytes that agentry-dockerimages' ripgrep workflow built, checked and
# signed on main. It runs `cosign verify` against that workflow's keyless
# identity. With OUT=<dir> it then copies the image's /rg to <dir>/rg, which
# AGENTRY_RIPGREP_BIN names for runnerimage/toolsrv's tests.
#
# Needs cosign, docker and the network (the registry and Sigstore). Exit 0
# when the pin verifies (and the binary was copied), 1 otherwise.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

IDENTITY=https://github.com/blaktron/agentry-dockerimages/.github/workflows/ripgrep.yaml@refs/heads/main
ISSUER=https://token.actions.githubusercontent.com

ref=$(sed -n 's/^ARG RIPGREP_IMAGE=\(.*\)$/\1/p' runnerimage/Dockerfile)
if ! [[ "$ref" =~ ^ghcr\.io/blaktron/[a-z0-9-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$ ]]; then
	echo "::error::runnerimage/Dockerfile's RIPGREP_IMAGE is not one name:tag@sha256:<digest> ref: '$ref'"
	exit 1
fi
digestRef="${ref%%:*}@${ref#*@}"
echo "verifying $digestRef"
echo "  as $IDENTITY"
if ! cosign verify "$digestRef" --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" >/dev/null; then
	echo "::error::the pinned ripgrep image $ref is not signed by the ripgrep workflow on main"
	exit 1
fi
echo "ok: the pinned ripgrep image is signed by the ripgrep workflow on main"

[ -n "${OUT:-}" ] || exit 0
mkdir -p "$OUT"
cid=$(docker create "$digestRef")
trap 'docker rm -fv "$cid" >/dev/null' EXIT
docker cp "$cid:/rg" "$OUT/rg"
echo "ok: the image's ripgrep is $OUT/rg"
