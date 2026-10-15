#!/usr/bin/env bash
# Copies the core's pkg/judge and pkg/websearch into the runner image's build
# context, byte for byte. The image is compiled inside `docker build` from
# stdlib-only sources with no module download, so the tools server cannot
# import the core; it carries these two packages instead, and a local scan
# asks exactly what the registry asks. It copies pkg/reportdesign too, with
# its built-in themes, so the tools server holds a report design to the same
# rules the core stored it under. runnerimage's TestTheImageJudgeIsTheCores
# fails until this is run after the core dependency moves.
set -euo pipefail
cd "$(dirname "$0")/.."
dir=$(GOWORK=off go list -m -f '{{.Dir}}' github.com/blaktron/agentry)
for pkg in judge websearch reportdesign; do
  mkdir -p "runnerimage/$pkg"
  rm -f "runnerimage/$pkg"/*.go "runnerimage/$pkg"/*.json
  for f in "$dir/pkg/$pkg"/*.go "$dir/pkg/$pkg"/*.json; do
    [ -e "$f" ] || continue
    case "$f" in *_test.go) continue ;; esac
    install -m 0644 "$f" "runnerimage/$pkg/"
  done
done
mkdir -p runnerimage/reportdesign/builtins
rm -f runnerimage/reportdesign/builtins/*.json
install -m 0644 "$dir/pkg/reportdesign/builtins"/*.json runnerimage/reportdesign/builtins/
echo "runnerimage/judge, runnerimage/websearch and runnerimage/reportdesign now match $(GOWORK=off go list -m -f '{{.Path}}@{{.Version}}' github.com/blaktron/agentry)"
