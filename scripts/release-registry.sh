#!/usr/bin/env bash
# The registry a release bakes in, chosen by the branch it is cut from
# (agentry-cli#240; the twin of agentry-desktop#183, which adopts this script
# and this build/release.env shape unchanged).
#
#   scripts/release-registry.sh [<commit>]        (default HEAD)
#
# prints, for $GITHUB_ENV or eval:
#
#   AGENTRY_RELEASE_CHANNEL=production|dev
#   AGENTRY_DEFAULT_REGISTRY=<the registry build/release.env names for it>
#
# The rule: a commit is a production release when it has been the head of
# main — it lies on origin/main's first-parent line — and a dev release when
# it has been the head of dev (origin/dev's first-parent line). Every commit
# main takes is a promotion's merge commit (`gh pr merge --merge`, never a
# fast-forward), so a dev commit that has since been promoted is reachable
# from main but is not on its first-parent line: it stays a dev build. A
# commit on neither line (a feature branch, a PR head) is refused: nothing
# else is a release. A commit on both (history from before dev branched off
# main) is production.
#
# Why not a per-branch value of build/release.env: main takes nothing but
# promotions of dev, so a line that differs between the branches could only
# be set on main by a direct commit, and a later edit to it on dev would ride
# the next promotion into production. One file, the same on both branches,
# names both registries; the history decides which one a build carries.
#
# It needs the remote-tracking refs origin/main and origin/dev and the whole
# history, which actions/checkout fetches with fetch-depth: 0; a shallow
# clone is refused, since a cut-off first-parent line could read a main
# commit as dev's.
#
# What it does not stop: the script and build/release.env are read from the
# commit being released, and the `release` environment admits any v* tag. A
# tag on a feature commit that edits this file could bake whatever it says.
# That was as true of the single-line release.env before; closing it needs a
# tag ruleset (unavailable on the repositories' current plan), not a script.
set -euo pipefail

commit=${1:-HEAD}
root=$(git rev-parse --show-toplevel)
env_file="$root/build/release.env"

[ -f "$env_file" ] || { echo "release-registry: no $env_file" >&2; exit 1; }
# shellcheck disable=SC1090
AGENTRY_REGISTRY_PRODUCTION=$(set -a; . "$env_file"; printf '%s' "${AGENTRY_REGISTRY_PRODUCTION:-}")
# shellcheck disable=SC1090
AGENTRY_REGISTRY_DEV=$(set -a; . "$env_file"; printf '%s' "${AGENTRY_REGISTRY_DEV:-}")
for v in AGENTRY_REGISTRY_PRODUCTION AGENTRY_REGISTRY_DEV; do
  val=${!v}
  case "$val" in
    https://*) [ "${val//[[:space:]]/}" = "$val" ] || { echo "release-registry: $v carries whitespace" >&2; exit 1; } ;;
    *) echo "release-registry: build/release.env must set $v to an https:// URL (it is '${val}')" >&2; exit 1 ;;
  esac
done

[ "$(git rev-parse --is-shallow-repository)" = false ] \
  || { echo "release-registry: this is a shallow clone; the rule needs the whole history (fetch-depth: 0)" >&2; exit 1; }

sha=$(git rev-parse --verify --quiet "${commit}^{commit}") || { echo "release-registry: $commit is not a commit" >&2; exit 1; }

on_first_parent_line() { # <branch>
  local ref="refs/remotes/origin/$1"
  git rev-parse --verify --quiet "$ref" >/dev/null \
    || { echo "release-registry: no $ref here (check out with fetch-depth: 0)" >&2; exit 1; }
  # grep without -q reads to the end: an early exit would SIGPIPE rev-list,
  # and pipefail would turn a match into a failure.
  git rev-list --first-parent "$ref" | grep -x "$sha" >/dev/null
}

if on_first_parent_line main; then
  channel=production; registry=$AGENTRY_REGISTRY_PRODUCTION
elif on_first_parent_line dev; then
  channel=dev; registry=$AGENTRY_REGISTRY_DEV
else
  echo "release-registry: $sha has never been the head of main or dev; a release is cut from one of them" >&2
  exit 1
fi

echo "AGENTRY_RELEASE_CHANNEL=$channel"
echo "AGENTRY_DEFAULT_REGISTRY=$registry"
