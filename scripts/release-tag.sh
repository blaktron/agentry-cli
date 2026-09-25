#!/usr/bin/env bash
# The CLI's release tags (agentry-cli#327; DEV-148): what a tag may be, and
# the next one to cut.
#
#   scripts/release-tag.sh check <tag> [<commit>]   (the release workflow's preflight)
#   scripts/release-tag.sh next dev|main            (an agent cutting a release)
#
# The scheme. A main release is vX.Y.Z on a commit on main's first-parent
# line (production, baking production's registry); a dev release is
# vX.Y.Z-dev.N, N from 1, on dev's first-parent line (baking dev's). The line
# is scripts/release-registry.sh's rule (agentry-cli#240, DR-33); a form that
# disagrees with it, any other form, and any 1.x or later unless
# AGENTRY_ALLOW_V1 is exactly "true" (the repository variable only the
# operator's 1.0 step sets, agentry#522) are refused before anything is built.
#
# check prints, for $GITHUB_OUTPUT:
#   version=X.Y.Z[-dev.N]   the tag without its v: what the binaries report
#   channel=production|dev
#   registry=<the registry build/release.env names for the channel>
#   prerelease=true|false   a -dev.N tag's GitHub Release is a prerelease
#   latest=true|false       only the newest vX.Y.Z becomes the latest release
#   draft=true|false        a minor or major main release from 1.0.0 on
#                           (X.Y.0, X >= 1) is left a draft: the operator
#                           approves its notes and publishes it; every
#                           release before 1.0.0, like every patch and
#                           -dev.N, publishes at once
# The commit defaults to the tag's own. Releases only move forward: a
# vX.Y.Z below the newest vX.Y.Z, and a -dev.N of a version already released,
# are refused (main is one line; there are no maintenance branches).
#
# next prints the tag to cut on that branch's head. X.Y.Z, the version main
# releases next, is:
#   - RELEASE_VERSION when set: the operator moving the minor (or cutting
#     1.0.0), never an agent's choice; it must be above the newest vX.Y.Z;
#   - else the highest X.Y.Z that dev has prereleased above the newest
#     vX.Y.Z (main releases what dev has been testing);
#   - else the newest vX.Y.Z with its patch bumped (agents mint patches);
#   - else 0.6.0, where every component starts (DEV-148).
# dev gets vX.Y.Z-dev.<one more than the highest N>, main gets vX.Y.Z. It
# reads the local tags: `git fetch --tags origin` first.
set -euo pipefail
die() { echo "release-tag: $*" >&2; exit 1; }
here=$(cd "$(dirname "$0")" && pwd)
n='(0|[1-9][0-9]*)'
stable_re="^v$n\.$n\.$n\$"

v1_guard() { # <version>
  local major=${1%%.*}
  if [ "$major" -ge 1 ] && [ "${AGENTRY_ALLOW_V1:-}" != true ]; then
    die "a v$major release is the operator's (set the repository variable AGENTRY_ALLOW_V1=true, agentry#522)"
  fi
}
# The newest vX.Y.Z whose commit is on main's first-parent line: a stray
# vX.Y.Z tag elsewhere (a refused release, a mistake) is not a release, so it
# moves neither latest nor next.
newest_stable() {
  local line t
  line=$(git rev-list --first-parent refs/remotes/origin/main 2>/dev/null) || line=""
  git tag -l 'v*' | { grep -E "$stable_re" || true; } | while read -r t; do
    if [ -z "$line" ] || grep -x "$(git rev-parse "$t^{commit}")" <<<"$line" >/dev/null; then echo "${t#v}"; fi
  done | sort -V | tail -1
}

cmd_check() {
  local tag=${1:?usage: release-tag.sh check <tag> [<commit>]} commit=${2:-}
  [[ $tag =~ ^v$n\.$n\.$n(-dev\.([1-9][0-9]*))?$ ]] || die "$tag is not vX.Y.Z or vX.Y.Z-dev.N"
  local major=${BASH_REMATCH[1]} patch=${BASH_REMATCH[3]} dev=${BASH_REMATCH[4]}
  local version=${tag#v} want prerelease latest=false draft=false
  v1_guard "$version"
  if [ -n "$dev" ]; then want=dev prerelease=true; else want=production prerelease=false; fi
  if [ -z "$commit" ]; then
    commit=$(git rev-parse -q --verify "refs/tags/$tag^{commit}") || die "no tag $tag here, and no commit named"
  fi
  local out channel registry
  out=$("$here/release-registry.sh" "$commit")
  channel=$(sed -n 's/^AGENTRY_RELEASE_CHANNEL=//p' <<<"$out")
  registry=$(sed -n 's/^AGENTRY_DEFAULT_REGISTRY=//p' <<<"$out")
  [ "$channel" = "$want" ] || {
    if [ "$want" = dev ]; then die "$tag is a dev prerelease, but $commit is on main's line: a dev release is cut from dev's head"
    else die "$tag is a main release, but $commit is on dev's line: a main release is cut from main's head (a promotion)"; fi
  }
  local newest xyz=${version%-dev.*}
  newest=$(newest_stable)
  if [ -n "$newest" ] && [ "$(printf '%s\n%s\n' "$newest" "$xyz" | sort -V | tail -1)" != "$xyz" ]; then
    die "$tag is below the newest release v$newest: releases only move forward"
  fi
  if [ "$prerelease" = true ] && [ "$xyz" = "$newest" ]; then
    die "$tag is a prerelease of v$newest, which is already released: the next dev prerelease is $("$0" next dev 2>/dev/null || echo "the next version's")"
  fi
  if [ "$prerelease" = false ]; then
    latest=true
    # Before 1.0.0 nothing is held (operator, 2026-09-25): only a minor or
    # major from 1.0.0 on waits for the operator's notes.
    [ "$patch" = 0 ] && [ "$major" -ge 1 ] && draft=true
  fi
  printf 'version=%s\nchannel=%s\nregistry=%s\nprerelease=%s\nlatest=%s\ndraft=%s\n' \
    "$version" "$channel" "$registry" "$prerelease" "$latest" "$draft"
}

cmd_next() {
  local branch=${1:-}
  case "$branch" in dev|main) ;; *) die "usage: release-tag.sh next dev|main" ;; esac
  local s next d ma mi pa
  s=$(newest_stable)
  if [ -n "${RELEASE_VERSION:-}" ]; then
    [[ $RELEASE_VERSION =~ ^$n\.$n\.$n$ ]] || die "RELEASE_VERSION '$RELEASE_VERSION' is not X.Y.Z"
    if [ -n "$s" ] && { [ "$s" = "$RELEASE_VERSION" ] || [ "$(printf '%s\n%s\n' "$s" "$RELEASE_VERSION" | sort -V | tail -1)" != "$RELEASE_VERSION" ]; }; then
      die "RELEASE_VERSION $RELEASE_VERSION is not above the newest release $s"
    fi
    next=$RELEASE_VERSION
  else
    d=$(git tag -l 'v*-dev.*' | sed -nE "s/^v($n\.$n\.$n)-dev\.[1-9][0-9]*$/\1/p" | sort -uV \
        | while read -r v; do
            if [ -z "$s" ] || { [ "$v" != "$s" ] && [ "$(printf '%s\n%s\n' "$s" "$v" | sort -V | tail -1)" = "$v" ]; }; then echo "$v"; fi
          done | tail -1)
    if [ -n "$d" ]; then next=$d
    elif [ -n "$s" ]; then IFS=. read -r ma mi pa <<<"$s"; next="$ma.$mi.$((pa + 1))"
    else next=0.6.0
    fi
  fi
  v1_guard "$next"
  if [ "$branch" = main ]; then
    ! git rev-parse -q --verify "refs/tags/v$next" >/dev/null || die "v$next exists already"
    echo "v$next"
  else
    local last
    last=$(git tag -l "v$next-dev.*" | sed -nE "s/^v${next//./\\.}-dev\.([0-9]+)$/\1/p" | sort -n | tail -1)
    echo "v$next-dev.$(( ${last:-0} + 1 ))"
  fi
}

case "${1:-}" in
  check) shift; cmd_check "$@" ;;
  next) shift; cmd_next "$@" ;;
  *) die "usage: release-tag.sh check <tag> [<commit>] | next dev|main" ;;
esac
