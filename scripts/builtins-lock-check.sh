#!/usr/bin/env bash
# builtins.lock is append-only: every line the base
# revision holds must still be in the working tree, unchanged. A line that
# changed means a built-in's tree changed under a version the registry may
# already hold, and a published version is never rebuilt; give the built-in
# a new version instead. TestTheBuiltinsLockHoldsEveryBuiltin checks the
# lock against the tree; this checks it against its own history.
#
# Usage: scripts/builtins-lock-check.sh [BASE]   (default HEAD^1: a pull
# request's merge ref's first parent is the base branch, a push's is the
# commit before it).
set -euo pipefail
lock=pkg/runner/localrun/builtins.lock
base=${1:-HEAD^1}

if ! git rev-parse --verify -q "$base^{commit}" >/dev/null; then
  echo "builtins.lock: base $base is not in this clone (fetch-depth too shallow?)" >&2
  exit 1
fi
if ! old=$(git show "$base:$lock" 2>/dev/null); then
  echo "builtins.lock: $base has none; nothing to hold it to"
  exit 0
fi

bad=0
while IFS= read -r line; do
  case "$line" in ''|'#'*) continue ;; esac
  if ! grep -qxF -- "$line" "$lock"; then
    echo "::error file=$lock::a line of $base's builtins.lock changed or went: '$line'. Never edit a line; give the built-in a new version and append its line." >&2
    bad=1
  fi
done <<<"$old"
[ "$bad" = 0 ] && echo "builtins.lock: every line of $base's lock is still here"
exit "$bad"
