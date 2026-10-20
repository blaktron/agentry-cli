#!/usr/bin/env bash
# Hold the CLI's pinned Coding agent toolchain image to its signature, as
# verify-browser-image.sh holds the browser image's.
#
# The Coding agent's exec container is built on this image, pulled by digest
# only, so the compilers and interpreters its commands run are the bytes this
# pin names; this check makes sure the pin names bytes agentry-dockerimages'
# coder workflow built, checked and signed. It reads DefaultCoderImage from
# pkg/runner/policy/policy.go and runs `cosign verify` against that
# workflow's keyless identity on main. CI runs it on every pull request. It
# checks this CLI's default only: an operator's images.coder is pinned by
# digest, and its signature is the operator's.
#
# Needs cosign and the network (the registry and Sigstore). Exit 0 when the
# pin verifies, 1 otherwise.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

IDENTITY=https://github.com/blaktron/agentry-dockerimages/.github/workflows/coder.yaml@refs/heads/main
ISSUER=https://token.actions.githubusercontent.com

ref=$(sed -n 's/^[[:space:]]*DefaultCoderImage[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' pkg/runner/policy/policy.go)
if ! [[ "$ref" =~ ^[a-z0-9./-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$ ]]; then
	echo "::error::DefaultCoderImage is not one name:tag@sha256:<digest> ref: '$ref'"
	exit 1
fi
digestRef="${ref%%:*}@${ref#*@}"
echo "verifying $digestRef"
echo "  as $IDENTITY"
if ! cosign verify "$digestRef" --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" >/dev/null; then
	echo "::error::the pinned coder image $ref is not signed by the coder workflow on main"
	exit 1
fi
echo "ok: the pinned coder image is signed by the coder workflow on main"
