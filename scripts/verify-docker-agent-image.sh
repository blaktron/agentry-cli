#!/usr/bin/env bash
# Hold the CLI's pinned default harness image to its signature.
#
# A run pulls the docker-agent harness by the digest policy.DefaultHarnessImage
# names, so the bytes it runs are the bytes this pin names; this check makes
# sure the pin names bytes that agentry-dockerimages' docker-agent workflow
# built from Docker Agent's pinned upstream commit, checked and signed. It reads
# DefaultHarnessImage from pkg/runner/policy/policy.go and runs `cosign verify`
# against that workflow's keyless identity on main. CI runs it on every pull
# request, so a pin to an unsigned or foreign digest cannot merge. It checks
# this CLI's default only: an operator's images.harnessImages.docker-agent is
# pinned by digest, and its signature is the operator's.
#
# Needs cosign and the network (the registry and Sigstore). Exit 0 when the
# pin verifies, 1 otherwise.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

IDENTITY=https://github.com/blaktron/agentry-dockerimages/.github/workflows/docker-agent.yaml@refs/heads/main
ISSUER=https://token.actions.githubusercontent.com

ref=$(sed -n 's/^[[:space:]]*DefaultHarnessImage[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' pkg/runner/policy/policy.go)
if ! [[ "$ref" =~ ^[a-z0-9./-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$ ]]; then
	echo "::error::the default harness is not one name:tag@sha256:<digest> ref: '$ref'"
	exit 1
fi
digestRef="${ref%%:*}@${ref#*@}"
echo "verifying $digestRef"
echo "  as $IDENTITY"
if ! cosign verify "$digestRef" --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" >/dev/null; then
	echo "::error::the pinned default harness $ref is not signed by the docker-agent workflow on main"
	exit 1
fi
echo "ok: the pinned default harness is signed by the docker-agent workflow on main"
