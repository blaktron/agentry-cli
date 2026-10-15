#!/usr/bin/env bash
# Hold the CLI's pinned browser image to its signature, as
# verify-hostlink-image.sh holds the hostlink image's.
#
# A run that declares capabilities.browser pulls the browser server's image
# by digest only, so the Chromium and the Playwright MCP server a run's
# browser runs are the bytes this pin names; this check makes sure the pin
# names bytes agentry-dockerimages' browser workflow built, checked and
# signed. It reads DefaultBrowserImage from pkg/runner/policy/policy.go and
# runs `cosign verify` against that workflow's keyless identity on main. CI
# runs it on every pull request. It checks this CLI's default only: an
# operator's images.browser is pinned by digest, and its signature is the
# operator's.
#
# Needs cosign and the network (the registry and Sigstore). Exit 0 when the
# pin verifies, 1 otherwise.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

IDENTITY=https://github.com/blaktron/agentry-dockerimages/.github/workflows/browser.yaml@refs/heads/main
ISSUER=https://token.actions.githubusercontent.com

ref=$(sed -n 's/^[[:space:]]*DefaultBrowserImage[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' pkg/runner/policy/policy.go)
if ! [[ "$ref" =~ ^[a-z0-9./-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$ ]]; then
	echo "::error::DefaultBrowserImage is not one name:tag@sha256:<digest> ref: '$ref'"
	exit 1
fi
digestRef="${ref%%:*}@${ref#*@}"
echo "verifying $digestRef"
echo "  as $IDENTITY"
if ! cosign verify "$digestRef" --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" >/dev/null; then
	echo "::error::the pinned browser image $ref is not signed by the browser workflow on main"
	exit 1
fi
echo "ok: the pinned browser image is signed by the browser workflow on main"
