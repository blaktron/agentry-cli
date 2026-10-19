#!/usr/bin/env bash
# Fail when the committed THIRD_PARTY_NOTICES is not what
# scripts/notices/generate.sh produces from the current module graph, so a
# dependency bump cannot ship without its licence text.
#
# Needs the toolchain and the Go module cache (see generate.sh).
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
file=THIRD_PARTY_NOTICES
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

scripts/notices/generate.sh "$tmp" >/dev/null
if ! cmp -s "$file" "$tmp"; then
  echo "$file is out of date with go.mod and the modules' licence files; run scripts/notices/generate.sh and commit the result." >&2
  diff -u "$file" "$tmp" | head -40 >&2 || true
  exit 1
fi
echo "$file is current"
