#!/usr/bin/env bash
# Hold the CLI's pinned hostlink image to its signature (agentry-cli#861;
# plan runtime-hosts.md D5; DR-91), as verify-files-image.sh holds the files
# image's.
#
# The host link's image is built on this machine from the hostlink image,
# pulled by digest only, so the OpenSSH client a run reaches a target host
# with is the bytes this pin names; this check makes sure the pin names
# bytes agentry-dockerimages' hostlink workflow built, checked and signed.
# It reads DefaultHostlinkImage from pkg/runner/policy/policy.go and runs
# `cosign verify` against that workflow's keyless identity on main. CI runs
# it on every pull request. It checks this CLI's default only: an
# operator's images.hostlink is pinned by digest, and its signature is the
# operator's.
#
# Needs cosign and the network (the registry and Sigstore). Exit 0 when the
# pin verifies, 1 otherwise.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

IDENTITY=https://github.com/blaktron/agentry-dockerimages/.github/workflows/hostlink.yaml@refs/heads/main
ISSUER=https://token.actions.githubusercontent.com

ref=$(sed -n 's/^[[:space:]]*DefaultHostlinkImage[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' pkg/runner/policy/policy.go)
if ! [[ "$ref" =~ ^[a-z0-9./-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$ ]]; then
	echo "::error::DefaultHostlinkImage is not one name:tag@sha256:<digest> ref: '$ref'"
	exit 1
fi
digestRef="${ref%%:*}@${ref#*@}"
echo "verifying $digestRef"
echo "  as $IDENTITY"
if ! cosign verify "$digestRef" --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" >/dev/null; then
	echo "::error::the pinned hostlink image $ref is not signed by the hostlink workflow on main"
	exit 1
fi
echo "ok: the pinned hostlink image is signed by the hostlink workflow on main"
