#!/usr/bin/env bash
# The CI supply chain's pins, checked (agentry-desktop#128; copied for agentry-cli#165). Run daily by
# .github/workflows/pins.yaml, and by hand from the repository root.
#
#   1. Every `uses:` in .github/workflows is pinned to a full commit SHA with
#      its tag in a trailing comment (`owner/repo@<40 hex> # vX.Y.Z`), and
#      that tag, resolved now, is still that commit. A tag that has moved is
#      exactly what a pin protects against, so it is an error, not a note.
#   2. Each pinned action and each CI tool in ci.yaml's `env:` block
#      (GOLANGCI_LINT_VERSION, GOVULNCHECK_VERSION, GITLEAKS_VERSION) is
#      compared with the newest release tag upstream. A newer release in the
#      same major is an error (bump the pin: a new SHA and its tag comment,
#      or the version in ci.yaml, plus GITLEAKS_SHA256 for gitleaks). A new
#      major is a warning: it can change behaviour and gets its own change.
#
# Needs only git and the network (tags come from `git ls-remote`, which needs
# no token), so it runs on the bare runner container. Exit 0 when current,
# 1 when anything above is wrong.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
wf_dir=${PINS_WORKFLOWS_DIR:-.github/workflows}
ci_file="$wf_dir/ci.yaml"

errors=0
warnings=0
err() { echo "::error::$*"; errors=$((errors + 1)); }
warn() { echo "::warning::$*"; warnings=$((warnings + 1)); }

# stable_tags <owner/repo>: "<sha> <tag>" per vX.Y.Z tag, the SHA peeled to
# the commit for annotated tags.
declare -A tag_cache
stable_tags() {
  local repo=$1
  if [ -z "${tag_cache[$repo]+x}" ]; then
    tag_cache[$repo]=$(git ls-remote --tags "https://github.com/$repo" |
      awk '{ ref=$2; sub("refs/tags/", "", ref); peeled = sub(/\^\{\}$/, "", ref); if (ref ~ /^v[0-9]+\.[0-9]+\.[0-9]+$/) { if (peeled) p[ref]=$1; else l[ref]=$1 } }
           END { for (t in l) print ((t in p) ? p[t] : l[t]), t }')
    [ -n "${tag_cache[$repo]}" ] || { err "$repo: no release tags readable upstream"; }
  fi
  printf '%s\n' "${tag_cache[$repo]}"
}

major() { local v=${1#v}; echo "${v%%.*}"; }

# compare_newest <label> <owner/repo> <current vX.Y.Z>
compare_newest() {
  local label=$1 repo=$2 cur=$3 tags newest newest_same
  tags=$(stable_tags "$repo" | awk '{print $2}' | sort -V)
  [ -n "$tags" ] || return 0
  newest=$(tail -1 <<<"$tags")
  newest_same=$(grep -E "^v$(major "$cur")\." <<<"$tags" | tail -1 || true)
  if [ -n "$newest_same" ] && [ "$newest_same" != "$cur" ] &&
     [ "$(printf '%s\n%s\n' "$cur" "$newest_same" | sort -V | tail -1)" = "$newest_same" ]; then
    err "$label is at $cur; $newest_same is out (same major): bump the pin"
  fi
  if [ "$(major "$newest")" -gt "$(major "$cur")" ]; then
    warn "$label is on major v$(major "$cur"); $newest is out (a new major: its own change)"
  fi
}

# 1 and 2 for the actions.
mapfile -t uses < <(grep -hE '^\s*-?\s*uses:' "$wf_dir"/*.yaml | sed -E 's/^\s*-?\s*uses:\s*//' | sort -u)
for u in "${uses[@]}"; do
  if [[ ! $u =~ ^([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)(/[^@]*)?@([0-9a-f]{40})\ +#\ +(v[0-9]+\.[0-9]+\.[0-9]+)$ ]]; then
    err "not pinned as owner/repo@<sha> # vX.Y.Z: uses: $u"
    continue
  fi
  repo=${BASH_REMATCH[1]} sha=${BASH_REMATCH[3]} tag=${BASH_REMATCH[4]}
  at=$(stable_tags "$repo" | awk -v t="$tag" '$2 == t {print $1}')
  if [ -z "$at" ]; then
    err "$repo: the pin's comment names $tag, which is not a tag upstream"
  elif [ "$at" != "$sha" ]; then
    err "$repo: $tag is now $at upstream, not the pinned $sha (a moved tag: find out why before re-pinning)"
  fi
  compare_newest "$repo" "$repo" "$tag"
done

# 2 for the tools pinned in ci.yaml's env block.
envval() { sed -nE "s/^  $1: *([^ #]+).*/\1/p" "$ci_file" | head -1; }
check_tool() {
  local var=$1 repo=$2 prefix=$3 v
  v=$(envval "$var")
  if [ -z "$v" ]; then err "$ci_file: $var is not set in the workflow env block"; return; fi
  compare_newest "$var" "$repo" "$prefix${v#v}"
}
check_tool GOLANGCI_LINT_VERSION golangci/golangci-lint v
check_tool GOVULNCHECK_VERSION golang/vuln v
check_tool GITLEAKS_VERSION gitleaks/gitleaks v

# Nothing installs at an unpinned version.
if grep -nE '@latest|version: *latest' "$wf_dir"/*.yaml; then
  err "a workflow installs a tool at latest: pin it in ci.yaml's env block"
fi

if [ "$errors" -gt 0 ]; then
  echo "pins: $errors error(s), $warnings warning(s)"
  exit 1
fi
echo "pins: every action is SHA-pinned to its tag and current within its major ($warnings warning(s))"
