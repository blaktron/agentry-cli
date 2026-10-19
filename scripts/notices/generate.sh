#!/usr/bin/env bash
# Regenerate THIRD_PARTY_NOTICES from the module graph and the modules'
# own licence files. The output is deterministic: run twice, same bytes.
#
# Usage: scripts/notices/generate.sh [OUTPUT]   (default THIRD_PARTY_NOTICES
# at the repository root)
#
# Needs the toolchain and the Go module cache. Modules that only some
# systems compile (the windows-only ones, the linux-only ones) are listed
# for every target, so a clean cache downloads them on first use; nothing
# else touches the network.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
out=${1:-THIRD_PARTY_NOTICES}
tmp=$(mktemp "$out.XXXXXX")
trap 'rm -f "$tmp"' EXIT

GOWORK=off ${GO:-go} run ./scripts/notices >"$tmp"
chmod 0644 "$tmp"
mv "$tmp" "$out"
trap - EXIT
echo "wrote $out"
