#!/usr/bin/env bash
# Hold the CLI's pinned Codex harness image to its signature.
#
# A run pulls the codex-image adapter's image by digest only, so the bytes
# it runs are the bytes this pin names; this check makes sure the pin names
# bytes that agentry-dockerimages' codex workflow built, checked (OpenAI's
# signature on the Codex binary inside it included) and signed. It reads the
# codex-image entry of DefaultHarnessImages from pkg/runner/policy/signin.go
# and runs `cosign verify` against that workflow's keyless identity on main.
# CI runs it on every pull request, so a pin to an unsigned or foreign
# digest cannot merge. It checks this CLI's default only: an operator's
# images.harnessImages entry is pinned by digest, and its signature is the
# operator's.
#
# Needs cosign and the network (the registry and Sigstore). Exit 0 when the
# pin verifies, 1 otherwise.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

IDENTITY=https://github.com/blaktron/agentry-dockerimages/.github/workflows/codex.yaml@refs/heads/main
ISSUER=https://token.actions.githubusercontent.com

ref=$(sed -n 's/^[[:space:]]*"codex-image":[[:space:]]*{Ref:[[:space:]]*"\([^"]*\)".*/\1/p' pkg/runner/policy/signin.go)
if ! [[ "$ref" =~ ^[a-z0-9./-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$ ]]; then
	echo "::error::the codex-image default is not one name:tag@sha256:<digest> ref: '$ref'"
	exit 1
fi
digestRef="${ref%%:*}@${ref#*@}"
echo "verifying $digestRef"
echo "  as $IDENTITY"
if ! cosign verify "$digestRef" --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" >/dev/null; then
	echo "::error::the pinned Codex image $ref is not signed by the codex workflow on main"
	exit 1
fi
echo "ok: the pinned Codex image is signed by the codex workflow on main"
