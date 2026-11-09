#!/usr/bin/env bash
# fetch-tool.sh URL SHA256 DEST
#
# Puts the file at URL, whose SHA-256 must be SHA256, at DEST.
#
# A CI tool is downloaded once per runner host, not once per job: the file
# is kept in the runners' shared tool cache under its checksum, and a job
# that finds it there copies it without asking the network. Only a file
# whose checksum matches is ever used or kept, whether it came from the
# cache or from the network. A download that fails is retried with growing
# pauses for about five minutes, so a release host's passing errors (a 504
# from a CDN edge, a reset connection) cost a wait instead of a failed job.
#
# The cache is AGENTRY_TOOL_CACHE when set, else /opt/runner/cache/tools
# when that can be written, else none (a runner without the shared cache
# downloads every time, still with the retries).
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: fetch-tool.sh URL SHA256 DEST" >&2
  exit 2
fi
url=$1 sum=$2 dest=$3
case "$sum" in
  *[!0-9a-f]* | "") echo "fetch-tool: $sum is not a lowercase SHA-256" >&2; exit 2 ;;
esac
[ "${#sum}" -eq 64 ] || { echo "fetch-tool: $sum is not a SHA-256" >&2; exit 2; }

# sha256sum on Linux, shasum on macOS.
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}
matches() { [ -f "$1" ] && [ "$(sha256 "$1")" = "$sum" ]; }

cache=${AGENTRY_TOOL_CACHE:-/opt/runner/cache/tools}
if ! mkdir -p "$cache" 2>/dev/null || [ ! -w "$cache" ]; then
  cache=""
fi
name=$(basename "${url%%\?*}")
cached=${cache:+$cache/$sum-$name}

mkdir -p "$(dirname "$dest")"
if [ -n "$cached" ] && matches "$cached"; then
  cp "$cached" "$dest"
  echo "fetch-tool: $name from the runner's tool cache"
  exit 0
fi

tmp=$(mktemp "${dest}.XXXXXX")
trap 'rm -f "$tmp"' EXIT
delay=5
for attempt in 1 2 3 4 5 6 7 8; do
  if curl -fsSL --connect-timeout 20 --max-time 300 -o "$tmp" "$url"; then
    break
  fi
  if [ "$attempt" -eq 8 ]; then
    echo "fetch-tool: $url could not be downloaded after $attempt attempts" >&2
    exit 1
  fi
  echo "fetch-tool: attempt $attempt for $name failed; trying again in ${delay}s" >&2
  sleep "$delay"
  delay=$((delay * 2 > 60 ? 60 : delay * 2))
done

if ! matches "$tmp"; then
  echo "fetch-tool: $name does not have the pinned SHA-256 $sum" >&2
  exit 1
fi
if [ -n "$cached" ]; then
  # Written under a name of its own, then renamed: a job reading the cache
  # never sees a partial file.
  part=$(mktemp "$cached.XXXXXX")
  if ! { cp "$tmp" "$part" && chmod 0644 "$part" && mv -f "$part" "$cached"; }; then
    rm -f "$part"
  fi
fi
mv -f "$tmp" "$dest"
trap - EXIT
echo "fetch-tool: $name downloaded and verified"
