#!/usr/bin/env bash
# Copies the core's pkg/judge and pkg/websearch into the runner image's build
# context, byte for byte (DR-43; agentry-cli#345). The image is compiled inside
# `docker build` from stdlib-only sources with no module download (DR-2), so
# the tools server cannot import the core; it carries these two packages
# instead, and a local scan asks exactly what the registry asks (plan
# builder-rework.md D25). runnerimage's TestTheImageJudgeIsTheCores fails
# until this is run after the core dependency moves.
set -euo pipefail
cd "$(dirname "$0")/.."
dir=$(GOWORK=off go list -m -f '{{.Dir}}' github.com/blaktron/agentry)
for pkg in judge websearch; do
  mkdir -p "runnerimage/$pkg"
  rm -f "runnerimage/$pkg"/*.go
  for f in "$dir/pkg/$pkg"/*.go; do
    case "$f" in *_test.go) continue ;; esac
    install -m 0644 "$f" "runnerimage/$pkg/"
  done
done
echo "runnerimage/judge and runnerimage/websearch now match $(GOWORK=off go list -m -f '{{.Path}}@{{.Version}}' github.com/blaktron/agentry)"
