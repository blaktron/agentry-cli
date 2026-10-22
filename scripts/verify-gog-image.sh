#!/usr/bin/env bash
# Hold the CLI's pinned Google Workspace server image to its signature, as
# verify-coder-image.sh holds the coder image's.
#
# The Assistant's Google Workspace server runs gog from this image, pulled by
# digest only, with the person's Google sign-in on its volume, so the program
# that holds that sign-in is the bytes this pin names; this check makes sure
# the pin names bytes agentry-dockerimages' gog workflow built from gog's
# pinned source, checked and signed. It reads DefaultGogImage from
# pkg/runner/policy/policy.go and runs `cosign verify` against that
# workflow's keyless identity on main. CI runs it on every pull request.
#
# Needs cosign and the network (the registry and Sigstore). Exit 0 when the
# pin verifies, 1 otherwise.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

IDENTITY=https://github.com/blaktron/agentry-dockerimages/.github/workflows/gog.yaml@refs/heads/main
ISSUER=https://token.actions.githubusercontent.com

ref=$(sed -n 's/^[[:space:]]*DefaultGogImage[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' pkg/runner/policy/policy.go)
if ! [[ "$ref" =~ ^[a-z0-9./-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$ ]]; then
	echo "::error::DefaultGogImage is not one name:tag@sha256:<digest> ref: '$ref'"
	exit 1
fi
digestRef="${ref%%:*}@${ref#*@}"
echo "verifying $digestRef"
echo "  as $IDENTITY"
if ! cosign verify "$digestRef" --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" >/dev/null; then
	echo "::error::the pinned gog image $ref is not signed by the gog workflow on main"
	exit 1
fi
echo "ok: the pinned gog image is signed by the gog workflow on main"
