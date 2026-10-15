#!/usr/bin/env bash
# Acceptance: a release cut from main bakes production's registry, one cut
# from dev bakes dev's, and nothing else is a release.
#
#   scripts/acceptance/release-registry.sh
#
# Builds a throwaway origin with the repository's branch model: dev merges
# feature branches with merge commits, main takes only promotions of dev
# with merge commits. It clones it as actions/checkout does (fetch-depth 0,
# origin/* refs), and asks scripts/release-registry.sh about each kind of
# commit a tag could name. Then it builds the CLI with `make dist` and what a
# main commit is given, and reads the registry back from the binary's
# --version. Needs git and go; no network, no docker.
set -euo pipefail

here=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail=0
pass() { echo "ok   $*"; }
bad() { echo "FAIL $*"; fail=1; }

# The committed values are the contract: production's site and dev's.
prod=$(set -a; . "$here/build/release.env"; printf '%s' "$AGENTRY_REGISTRY_PRODUCTION")
devr=$(set -a; . "$here/build/release.env"; printf '%s' "$AGENTRY_REGISTRY_DEV")
[ "$prod" = "https://secureagentry.net" ] && pass "release.env: production is $prod" || bad "release.env: production is '$prod', want https://secureagentry.net"
[ "$devr" = "https://dev.secureagentry.net" ] && pass "release.env: dev is $devr" || bad "release.env: dev is '$devr', want https://dev.secureagentry.net"

g() { git -c user.name=t -c user.email=t@example.test -c init.defaultBranch=main -c commit.gpgsign=false "$@"; }

g init -q "$work/src"
cd "$work/src"
mkdir -p build scripts
cp "$here/build/release.env" build/release.env
cp "$here/scripts/release-registry.sh" scripts/release-registry.sh
g add -A && g commit -qm "c0: before dev"
c0=$(git rev-parse HEAD)
g checkout -qb dev
g checkout -qb feat/1 && echo 1 > f1 && g add f1 && g commit -qm f1
f1=$(git rev-parse HEAD)
g checkout -q dev && g merge -q --no-ff -m "merge: f1 (#1)" feat/1
m1=$(git rev-parse HEAD)
g checkout -q main && g merge -q --no-ff -m "merge: promote dev → main (#2)" dev
p1=$(git rev-parse HEAD)
g checkout -q dev && g checkout -qb feat/3 && echo 3 > f3 && g add f3 && g commit -qm f3
g checkout -q dev && g merge -q --no-ff -m "merge: f3 (#3)" feat/3
m2=$(git rev-parse HEAD)
g checkout -qb feat/4 && echo 4 > f4 && g add f4 && g commit -qm f4
f4=$(git rev-parse HEAD)
g checkout -q main

g clone -q "$work/src" "$work/co"
cd "$work/co"

expect() { # <label> <commit> <channel> <registry>
  local out
  if ! out=$(scripts/release-registry.sh "$2" 2>&1); then bad "$1: refused ($out), want $3"; return; fi
  if [ "$out" = "$(printf 'AGENTRY_RELEASE_CHANNEL=%s\nAGENTRY_DEFAULT_REGISTRY=%s' "$3" "$4")" ]; then
    pass "$1: $3 $4"
  else
    bad "$1: got '$out', want $3 $4"
  fi
}
refuse() { # <label> <commit>
  local out
  if out=$(scripts/release-registry.sh "$2" 2>&1); then bad "$1: accepted ($out), want refused"; else pass "$1: refused"; fi
}

expect "a promotion's merge commit on main" "$p1" production "$prod"
expect "dev's head, not yet promoted"       "$m2" dev "$devr"
expect "a dev head since promoted to main"  "$m1" dev "$devr"
expect "history from before dev branched"   "$c0" production "$prod"
refuse "a feature commit merged into dev"   "$f1"
refuse "an unmerged feature branch"         "$f4"

# Without the origin refs, or with a cut-off history, the rule cannot be
# applied: refuse, never guess.
g clone -q --single-branch --branch dev "$work/src" "$work/onebranch"
cd "$work/onebranch"; refuse "a checkout without origin/main" HEAD
g clone -q --depth 1 --no-single-branch "file://$work/src" "$work/shallow"
cd "$work/shallow"; refuse "a shallow checkout" origin/main

# A main build carries production's registry, end to end: what the script
# gives a main commit, baked by the Makefile's own dist target (the host's
# platform only), read back from --version.
cd "$work/co"
eval "$(scripts/release-registry.sh "$p1")"
plat="$(go env GOOS)/$(go env GOARCH)"
bin="dist/agentry-9.9.9-${plat%/*}-${plat#*/}"; [ "${plat%/*}" = windows ] && bin="$bin.exe"
make -s -C "$here" dist DIST_VERSION=9.9.9 DIST_TARGETS="$plat" AGENTRY_DEFAULT_REGISTRY="$AGENTRY_DEFAULT_REGISTRY" >/dev/null
v=$("$here/$bin" --version)
rm -f "$here/$bin"
if printf '%s\n' "$v" | grep -qxF "default registry: $prod"; then
  pass "a main build's --version names $prod"
else
  bad "a main build's --version says '$v', want 'default registry: $prod'"
fi

[ "$fail" -eq 0 ] && echo "release-registry: all checks passed" || { echo "release-registry: FAILED"; exit 1; }
