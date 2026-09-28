#!/usr/bin/env bash
# run.sh PKG... — run Go packages' tests as Windows binaries under Wine,
# inside a throwaway container, never on the host (agentry-cli#421): Wine
# maps Z: to the host's / and follows directory links when it deletes, so a
# test that links to a system path would meet the host's files.
#
#   docker build -t agentry-winebox:local scripts/windows-tests   # once
#   scripts/windows-tests/run.sh ./pkg/knowledge/ ./pkg/plainpath/
#   WINETEST_ARGS='-test.v -test.run TestX' scripts/windows-tests/run.sh ./pkg/x/
#
# Run from the module root. The box has no network, no capabilities and
# only read-only mounts; never give it a writable one, since its user has
# the host user's uid and SELinux labelling is off for the mounts. The whole
# module is mounted (read-only), so keep the module root free of secrets.
set -euo pipefail
docker image inspect agentry-winebox:local >/dev/null 2>&1 || {
  echo "no agentry-winebox:local image: docker build -t agentry-winebox:local scripts/windows-tests" >&2
  exit 2
}
mkdir -p "$HOME/scratch"
out=$(mktemp -d -p "$HOME/scratch")
chmod 755 "$out"
trap 'rm -rf "$out"' EXIT
st=0
# The module root is mounted, not the package alone: a test may read a
# sibling package's testdata (the engine reads the adapters' recordings).
root=$(GOWORK=off go list -m -f '{{.Dir}}')
for pkg in "$@"; do
  name=$(echo "$pkg" | tr '/.' '__')
  dir=$(GOWORK=off go list -f '{{.Dir}}' "$pkg") || { st=1; continue; }
  rel=${dir#"$root"}
  GOOS=windows GOWORK=off go test -c -o "$out/$name.test.exe" "$pkg" || { st=1; continue; }
  [ -f "$out/$name.test.exe" ] || { echo "== $pkg: no test files"; continue; }
  echo "== $pkg"
  # --init: without a real PID 1, timeout would be it, Wine's server would
  # daemonize under it, and the run would exit 125 with no word.
  docker run --rm --init --network none --memory 4g --pids-limit 512 \
    --cap-drop ALL --security-opt no-new-privileges --security-opt label=disable \
    -e WINETEST_ARGS="${WINETEST_ARGS:-}" -e TEST_EXE="/work/bin/$name.test.exe" \
    -v "$root":/work/mod:ro -v "$out":/work/bin:ro -w "/work/mod$rel" \
    agentry-winebox:local sh -c 'timeout 600 wine "$TEST_EXE" -test.timeout 300s $WINETEST_ARGS' || st=1
done
exit $st
