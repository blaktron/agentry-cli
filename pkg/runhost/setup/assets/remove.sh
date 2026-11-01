#!/bin/sh
# A remote runhost's removal, run as root by `agentry runhost remove`. It
# stops the service, takes away the agentry account and everything in its
# home, and takes gVisor's two runtimes out of the Docker daemon. Docker
# Engine and the images stay unless the person chose otherwise.
#
# Inputs: the environment file $1 (REMOVE_IMAGES, REMOVE_DOCKER, and
# WAS_DAEMON_JSON, the SHA-256 of the daemon configuration the device read)
# and, in $STAGE, daemon.json: the daemon's configuration without the two
# runtimes, when the server's has them. Docker Engine is removed only when
# setup installed it.
set -eu
# shellcheck disable=SC1090
. "$1"
: "${STAGE:?}" "${REMOVE_IMAGES:?}" "${REMOVE_DOCKER:?}"
step() { printf 'step: %s\n' "$*"; }

# Whether setup installed Docker Engine, read before the home it is kept in
# goes.
installed_docker=no
if [ -f /var/lib/agentry/runhost/installed-docker ] || grep -q '"installedDocker": true' /var/lib/agentry/runhost/config.json 2>/dev/null; then
	installed_docker=yes
fi

if id agentry >/dev/null 2>&1; then
	# Only the account setup made: another agentry account is not ours to take.
	[ "$(getent passwd agentry | cut -d: -f6)" = /var/lib/agentry ] || { printf 'error: %s\n' "the agentry account here has another home; it is not a runhost's, and is left alone"; exit 1; }
	uid=$(id -u agentry)
	step "the agentry-runhost service and the account's user manager"
	loginctl disable-linger agentry 2>/dev/null || true
	systemctl stop "user@$uid.service" 2>/dev/null || true
	pkill -KILL -u agentry 2>/dev/null || true
	step "the agentry account and its home"
	userdel agentry
	rm -rf /var/lib/agentry
fi

if [ -f "$STAGE/daemon.json" ]; then
	: "${WAS_DAEMON_JSON:?}"
	echo "$WAS_DAEMON_JSON  /etc/docker/daemon.json" | sha256sum -c --quiet - >/dev/null 2>&1 || { printf 'error: %s\n' "/etc/docker/daemon.json changed on the server during removal: run it again"; exit 1; }
	step "gVisor's runtimes out of the Docker daemon"
	install -m "$(stat -c %a /etc/docker/daemon.json)" "$STAGE/daemon.json" /etc/docker/daemon.json.new
	mv /etc/docker/daemon.json.new /etc/docker/daemon.json
	systemctl reload docker 2>/dev/null || true
fi
rm -f /etc/docker/daemon.json.before-agentry

if [ "$REMOVE_IMAGES" = yes ] && command -v docker >/dev/null 2>&1; then
	step "the images runs used"
	# By name, not by id: a tag of the person's own on the same image stays.
	docker images --format '{{.Repository}}:{{.Tag}}' |
		grep -E '^(ghcr\.io/blaktron/agentry-[a-z0-9-]+|agentry-runner):[A-Za-z0-9._-]+$' |
		xargs -r docker image rm >/dev/null 2>&1 || true
fi

if [ "$REMOVE_DOCKER" = yes ] && [ "$installed_docker" = no ]; then
	step "Docker Engine stays: setup did not install it"
fi
if [ "$REMOVE_DOCKER" = yes ] && [ "$installed_docker" = yes ]; then
	step "Docker Engine"
	export DEBIAN_FRONTEND=noninteractive
	apt-get purge -y -qq docker-ce docker-ce-cli containerd.io >/dev/null 2>&1 || true
	rm -f /etc/apt/sources.list.d/docker.sources /etc/apt/keyrings/docker.asc
fi
echo "done"
