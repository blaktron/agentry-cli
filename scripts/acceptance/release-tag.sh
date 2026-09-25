#!/usr/bin/env bash
# Acceptance for agentry-cli#327: a release tag's form must agree with the
# branch line its commit is on, 1.x waits for the operator, and the next tag
# an agent cuts follows the scheme (DEV-148).
#
#   scripts/acceptance/release-tag.sh
#
# Builds a throwaway origin with the estate's branch model (as
# release-registry.sh's acceptance does), clones it as actions/checkout does,
# and asks scripts/release-tag.sh about each tag form on each line, then
# about the next tag as releases accumulate. Needs git; no network, no go.
set -euo pipefail

here=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
unset AGENTRY_ALLOW_V1 RELEASE_VERSION
fail=0
pass() { echo "ok   $*"; }
bad() { echo "FAIL $*"; fail=1; }

g() { git -c user.name=t -c user.email=t@example.test -c init.defaultBranch=main -c commit.gpgsign=false "$@"; }

g init -q "$work/src"
cd "$work/src"
mkdir -p build scripts
cp "$here/build/release.env" build/release.env
cp "$here/scripts/release-registry.sh" "$here/scripts/release-tag.sh" scripts/
g add -A && g commit -qm "c0: before dev"
g checkout -qb dev
g checkout -qb feat/1 && echo 1 > f1 && g add f1 && g commit -qm f1
f1=$(git rev-parse HEAD)
g checkout -q dev && g merge -q --no-ff -m "merge: f1 (#1)" feat/1
m1=$(git rev-parse HEAD)
g checkout -q main && g merge -q --no-ff -m "merge: promote dev → main (#2)" dev
p1=$(git rev-parse HEAD)
g checkout -q main

g clone -q "$work/src" "$work/co"
cd "$work/co"

check() { scripts/release-tag.sh check "$@"; }
expect() { # <label> <want> <cmd…>
  local label=$1 want=$2; shift 2
  local out
  if ! out=$("$@" 2>&1); then bad "$label: refused ($out)"; return; fi
  [ "$out" = "$want" ] && pass "$label" || bad "$label: got '$out', want '$want'"
}
refuse() { # <label> <cmd…>
  local label=$1; shift
  local out
  if out=$("$@" 2>&1); then bad "$label: accepted ($out), want refused"; else pass "$label: refused"; fi
}
field() { sed -n "s/^$1=//p"; }
is() { [ "$3" = "$2" ] && pass "$1" || bad "$1: got '$3', want '$2'"; } # <label> <want> <got>

# The four combinations: each form on each line.
is "vX.Y.Z on main's line" production "$(check v0.6.0 "$p1" | field channel)"
refuse "vX.Y.Z on dev's line" check v0.6.0 "$m1"
is "vX.Y.Z-dev.N on dev's line" dev "$(check v0.6.1-dev.1 "$m1" | field channel)"
refuse "vX.Y.Z-dev.N on main's line" check v0.6.1-dev.1 "$p1"
refuse "any tag on a feature commit" check v0.6.1-dev.1 "$f1"

# What check prints for the workflow.
out=$(check v0.6.1-dev.1 "$m1")
expect "a dev tag is a prerelease, never latest" "true false" echo "$(field prerelease <<<"$out") $(field latest <<<"$out")"
expect "it bakes dev's registry" "$(set -a; . build/release.env; printf '%s' "$AGENTRY_REGISTRY_DEV")" field registry <<<"$out"
expect "its version is the tag without the v" 0.6.1-dev.1 field version <<<"$out"
out=$(check v0.6.0 "$p1")
expect "a main tag is a full release, latest when newest" "false true" echo "$(field prerelease <<<"$out") $(field latest <<<"$out")"
expect "a minor main release waits for the operator's notes (draft)" true field draft <<<"$out"
out=$(check v0.6.2 "$p1")
expect "a patch main release publishes at once" false field draft <<<"$out"
out=$(check v0.6.2-dev.3 "$m1")
expect "a dev prerelease publishes at once" false field draft <<<"$out"

for t in v0.6 0.6.0 v0.6.0-rc.1 v0.6.0-dev.0 v0.6.0-dev v01.6.0 v0.6.0-dev.1-x v0.6.0+b; do
  refuse "the form $t" check "$t" "$p1"
done
refuse "v1.0.0 without AGENTRY_ALLOW_V1" check v1.0.0 "$p1"
refuse "v1.0.0-dev.1 without AGENTRY_ALLOW_V1" check v1.0.0-dev.1 "$m1"
refuse "v1.0.0 with AGENTRY_ALLOW_V1=yes (only true opens it)" env AGENTRY_ALLOW_V1=yes scripts/release-tag.sh check v1.0.0 "$p1"
is "v1.0.0 with AGENTRY_ALLOW_V1=true" production "$(AGENTRY_ALLOW_V1=true scripts/release-tag.sh check v1.0.0 "$p1" | field channel)"
out=$(AGENTRY_ALLOW_V1=true scripts/release-tag.sh check v1.0.0 "$p1")
expect "1.0.0 waits for the operator's notes (draft)" true field draft <<<"$out"

# The next tag an agent cuts.
next() { scripts/release-tag.sh next "$@"; }
expect "no release yet: main's first is v0.6.0 (DEV-148)" v0.6.0 next main
expect "no release yet: dev's first is v0.6.0-dev.1" v0.6.0-dev.1 next dev
g tag v0.6.0 "$p1"
expect "after v0.6.0, dev's next is v0.6.1-dev.1" v0.6.1-dev.1 next dev
expect "after v0.6.0, main's next is v0.6.1" v0.6.1 next main
g tag v0.6.1-dev.1 "$m1"; g tag v0.6.1-dev.4 "$m1"
expect "N is one more than the highest" v0.6.1-dev.5 next dev
expect "main's next releases what dev has been prereleasing" v0.6.1 next main
expect "the operator moves the minor with RELEASE_VERSION" v0.7.0-dev.1 env RELEASE_VERSION=0.7.0 scripts/release-tag.sh next dev
refuse "RELEASE_VERSION at or below the newest release" env RELEASE_VERSION=0.6.0 scripts/release-tag.sh next main
g tag v0.7.0-dev.1 "$m1"
expect "dev prereleases of a higher version name main's next" v0.7.0 next main
g tag v0.7.0 "$p1"
expect "and after it, the patch bump" v0.7.1-dev.1 next dev
g tag v0.99.0 "$f1"
expect "a stray vX.Y.Z on a feature commit is not a release" v0.7.1 next main
g tag -d v0.99.0 >/dev/null
refuse "a main release below the newest one" check v0.6.1 "$p1"
refuse "a dev prerelease of a released version" check v0.7.0-dev.2 "$m1"
refuse "a dev prerelease below the newest release" check v0.6.9-dev.1 "$m1"
expect "a rebuild of the newest release's own tag passes" true field latest <<<"$(check v0.7.0 "$p1")"
expect "0.10 sorts above 0.9" v0.10.1 sh -c "git tag v0.9.3 '$p1' && git tag v0.10.0 '$p1' && scripts/release-tag.sh next main"
refuse "next into 1.x without AGENTRY_ALLOW_V1" env RELEASE_VERSION=1.0.0 scripts/release-tag.sh next main

[ "$fail" -eq 0 ] && echo "release-tag: all checks passed" || { echo "release-tag: FAILED"; exit 1; }
