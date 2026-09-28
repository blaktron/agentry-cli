#!/usr/bin/env bash
# winetest.sh PKG... — run Go packages' tests as Windows binaries under Wine,
# inside a throwaway container (agentry-winebox), never on the host: Wine
# maps Z: to / and follows links when it deletes. Run from the module root.
set -euo pipefail
out=$(mktemp -d -p "$HOME/scratch")
trap 'rm -rf "$out"' EXIT
st=0
for pkg in "$@"; do
  name=$(echo "$pkg" | tr '/.' '__')
  GOOS=windows GOWORK=off go test -c -o "$out/$name.test.exe" "$pkg" || { st=1; continue; }
  [ -f "$out/$name.test.exe" ] || { echo "== $pkg: no test files"; continue; }
  dir=$(GOWORK=off go list -f '{{.Dir}}' "$pkg")
  echo "== $pkg"
  docker run --rm --init --network none --memory 4g --pids-limit 512 --security-opt label=disable \
    -v "$dir":/work/pkg:ro -v "$out":/work/bin:ro -w /work/pkg \
    agentry-winebox:local sh -c "timeout 600 wine /work/bin/$name.test.exe -test.timeout 300s ${WINETEST_ARGS:-}" || st=1
done
exit $st
