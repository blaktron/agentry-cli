#!/usr/bin/env bash
# Hold the CLI's pinned files image to its signature (agentry-cli#559; plan
# file-handling.md §3 invariant 9, as the operator decided on 2026-10-02).
#
# A run pulls the files image by digest only, so the bytes it runs are the
# bytes this pin names; this check makes sure the pin names bytes that
# agentry-dockerimages' files workflow built, checked and signed. It reads
# DefaultFilesImage from pkg/runner/policy/policy.go and runs
# `cosign verify` against that workflow's keyless identity on main. CI runs
# it on every pull request, so a pin to an unsigned or foreign digest cannot
# merge. The sandbox hosts are to run the same verify when they pre-pull the
# image (agentry-web#946). It checks this CLI's default only: an operator's
# images.files is pinned by digest, and its signature is the operator's.
#
# Needs cosign and the network (the registry and Sigstore). Exit 0 when the
# pin verifies, 1 otherwise.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

IDENTITY=https://github.com/blaktron/agentry-dockerimages/.github/workflows/files.yaml@refs/heads/main
ISSUER=https://token.actions.githubusercontent.com

ref=$(sed -n 's/^[[:space:]]*DefaultFilesImage[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' pkg/runner/policy/policy.go)
if ! [[ "$ref" =~ ^[a-z0-9./-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$ ]]; then
	echo "::error::DefaultFilesImage is not one name:tag@sha256:<digest> ref: '$ref'"
	exit 1
fi
digestRef="${ref%%:*}@${ref#*@}"
echo "verifying $digestRef"
echo "  as $IDENTITY"
if ! cosign verify "$digestRef" --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" >/dev/null; then
	echo "::error::the pinned files image $ref is not signed by the files workflow on main"
	exit 1
fi
echo "ok: the pinned files image is signed by the files workflow on main"
