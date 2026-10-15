#!/usr/bin/env bash
# drafter-fixtures.sh OUT [agentry draft flags…] — the Drafter's fixture set:
# each described task in pkg/drafter/fixtures through `agentry draft` into
# OUT/<id>, then the measurement table (tokens, calls, AI Review's counts,
# validate, the shape check).
#
# It runs against the model your runner policy names, with your own keys:
# TYPESAFE_API_KEY (or the policy's judge block) is required, a search key
# only if the policy's webSearch block names a provider. AGENTRY=path picks
# the CLI binary (default: agentry on PATH).
set -uo pipefail
cd "$(dirname "$0")/.."
out=${1:?usage: drafter-fixtures.sh OUT [agentry draft flags…]}
shift
agentry=${AGENTRY:-agentry}
mkdir -p "$out"
out=$(cd "$out" && pwd)
for fx in pkg/drafter/fixtures/*.json; do
  id=$(basename "$fx" .json)
  desc=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["description"])' "$fx")
  hints=()
  while IFS= read -r h; do
    [ -n "$h" ] && hints+=(--hint "$h")
  done < <(python3 -c 'import json,sys; [print(h) for h in json.load(open(sys.argv[1]))["hints"]]' "$fx")
  rm -rf "${out:?}/$id"
  start=$(date +%s)
  "$agentry" draft "$desc" "${hints[@]}" --out "$out/$id" "$@" >"$out/$id.log" 2>&1
  echo "$id: exit $? in $(( $(date +%s) - start ))s"
done
AGENTRY_DRAFTER_RESULTS="$out" GOWORK=off go test -count=1 ./pkg/drafter/ -run TestTheFixtureRuns -v | sed -n 's/^ *\(|.*\)$/\1/p'
