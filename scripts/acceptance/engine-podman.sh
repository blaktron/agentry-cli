#!/usr/bin/env bash
# The container engine conformance (engine.sh) against a rootless podman of
# its own, so a machine that has only Docker Engine checks the podman side
# too:
#
#   scripts/acceptance/engine-podman.sh
#
# A container from podman's own image runs podman as its unprivileged user:
# the API service on that user's socket, podman's default namespaces for the
# containers it starts (the image's own configuration shares its namespaces,
# for podman in podman, which a run's posture refuses), and gVisor's runsc,
# fetched from its release and held to the SHA-512 pinned here, for the
# sandboxed run. engine.sh then runs there with AGENTRY_ENGINE=podman: every
# command the engine client issues, and one QuickAgent with declared egress
# and exec under runsc with the run's docker filter in front of podman.
#
# Only the docker CLI is needed here. The checkout's files go to the
# container on stdin, not as a mount, so a daemon that cannot see this
# filesystem (a CI job inside a runner container) works the same. The
# container is privileged so podman inside it can make user namespaces,
# mounts and gVisor's sandbox; it runs this checkout's script and binary and
# nothing else, and is removed with its storage at the end, the images the
# run pulled included.
#
# AGENTRY (default bin/agentry, built when missing) is the CLI under test; it
# must be a Linux binary for this machine's architecture. ENGINE_PODMAN_NAME
# names the container, for a caller that removes it after a cancelled run.
# The exit status is engine.sh's.
set -euo pipefail
cd "$(dirname "$0")/../.."

PODMAN_IMAGE=quay.io/podman/stable@sha256:2fb39ca923dd9e0ee33f9e319c3a107a2bfde022a6a31560cbd56a862821e7d3 # podman 5.8.7
GVISOR_RELEASE=20261012.0
GVISOR_SHA512=9606445eed5aedfcf05310e7ccef61d994089914c6622fc82624c24d2b18db1b2210f40e40a7be27b8b7772859de7d95c501b4eba66423053c960bf671377c5e # x86_64/gvisor.tar.bz2

case "$(uname -m)" in
  x86_64) ;;
  *) echo "SKIP: gVisor's release is pinned for x86_64 only, not $(uname -m)" >&2; exit 0 ;;
esac
command -v docker >/dev/null || { echo "SKIP: no docker on PATH" >&2; exit 0; }

AGENTRY=${AGENTRY:-bin/agentry}
[ -x "$AGENTRY" ] || make build >/dev/null

NAME=${ENGINE_PODMAN_NAME:-agentry-engine-podman-$$}
cleanup() { docker rm -fv "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

# The files the check needs, at the paths engine.sh expects them.
pack() {
  local bin; bin=$(basename "$AGENTRY")
  tar -c scripts/acceptance/engine.sh testdata/runner/sandbox-egress \
    -C "$(dirname "$AGENTRY")" --transform "s|^$bin\$|bin/agentry|" "$bin"
}

pack | docker run --rm -i --name "$NAME" --privileged --user podman \
  -e GVISOR_RELEASE="$GVISOR_RELEASE" -e GVISOR_SHA512="$GVISOR_SHA512" \
  "$PODMAN_IMAGE" bash -euo pipefail -c '
    src=$HOME/src; mkdir -p "$src" && tar -x -C "$src"

    # podman as a host has it: its own namespaces per container, its default
    # cgroups, its sysctls, and nothing of the host mounted in. Only what a
    # container without systemd needs stays: the log driver, the cgroup
    # manager and the events log.
    export CONTAINERS_CONF=$HOME/containers.conf
    printf "[containers]\nlog_driver = \"k8s-file\"\n[engine]\ncgroup_manager = \"cgroupfs\"\nevents_logger = \"file\"\n" > "$CONTAINERS_CONF"

    export XDG_RUNTIME_DIR=$HOME/run
    mkdir -p -m 700 "$XDG_RUNTIME_DIR/podman"
    sock=$XDG_RUNTIME_DIR/podman/podman.sock
    podman system service --time=0 "unix://$sock" &
    for _ in $(seq 100); do curl -sf --unix-socket "$sock" http://d/_ping >/dev/null && break; sleep 0.2; done

    gv=$HOME/gvisor; mkdir -p "$gv"
    curl -fsSL -o "$gv/gvisor.tar.bz2" "https://storage.googleapis.com/gvisor/releases/release/$GVISOR_RELEASE/x86_64/gvisor.tar.bz2"
    echo "$GVISOR_SHA512  $gv/gvisor.tar.bz2" | sha512sum -c --quiet
    tar -xjf "$gv/gvisor.tar.bz2" -C "$gv"

    cd "$src"
    RUNSC=$gv/runsc AGENTRY=$src/bin/agentry AGENTRY_ENGINE=podman scripts/acceptance/engine.sh
  '
