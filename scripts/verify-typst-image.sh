#!/usr/bin/env bash
# Hold the runner image's pinned Typst image to its signature, and extract
# its files for the real-render tests.
#
# The runner image copies export_pdf's renderer, fonts and packages from
# agentry-typst by digest (runnerimage/Dockerfile, ARG TYPST_IMAGE), so the
# bytes a run renders with are the bytes this pin names; this check makes
# sure the pin names bytes that agentry-dockerimages' typst workflow built,
# checked and signed on main. It runs `cosign verify` against that
# workflow's keyless identity. With OUT=<dir> it then copies the image's
# /typst, /fonts and /typst-packages into <dir>/typst, <dir>/fonts and
# <dir>/packages, the layout AGENTRY_TYPST_DIR names for
# runnerimage/toolsrv's tests.
#
# Needs cosign, docker and the network (the registry and Sigstore). Exit 0
# when the pin verifies (and the files were copied), 1 otherwise.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

IDENTITY=https://github.com/blaktron/agentry-dockerimages/.github/workflows/typst.yaml@refs/heads/main
ISSUER=https://token.actions.githubusercontent.com

ref=$(sed -n 's/^ARG TYPST_IMAGE=\(.*\)$/\1/p' runnerimage/Dockerfile)
if ! [[ "$ref" =~ ^ghcr\.io/blaktron/[a-z0-9-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$ ]]; then
	echo "::error::runnerimage/Dockerfile's TYPST_IMAGE is not one name:tag@sha256:<digest> ref: '$ref'"
	exit 1
fi
digestRef="${ref%%:*}@${ref#*@}"
echo "verifying $digestRef"
echo "  as $IDENTITY"
if ! cosign verify "$digestRef" --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" >/dev/null; then
	echo "::error::the pinned Typst image $ref is not signed by the typst workflow on main"
	exit 1
fi
echo "ok: the pinned Typst image is signed by the typst workflow on main"

[ -n "${OUT:-}" ] || exit 0
mkdir -p "$OUT"
cid=$(docker create "$digestRef")
trap 'docker rm -fv "$cid" >/dev/null' EXIT
docker cp "$cid:/typst" "$OUT/typst"
docker cp "$cid:/fonts" "$OUT/fonts"
docker cp "$cid:/typst-packages" "$OUT/packages"
echo "ok: the image's renderer, fonts and packages are in $OUT"
