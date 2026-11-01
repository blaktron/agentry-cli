#!/bin/sh
# A remote runhost's removal, run as root by `agentry runhost remove`. It
# stops the service, takes away the agentry account and everything in its
# home, and takes gVisor's two runtimes out of the Docker daemon. Docker
# Engine and the images stay unless the person chose otherwise.
#
# Inputs: the environment file $1 (REMOVE_IMAGES, REMOVE_DOCKER) and, in
# $STAGE, daemon.json: the daemon's configuration without the two runtimes,
# when the server's has them.
set -eu
# shellcheck disable=SC1090
. "$1"
: "${STAGE:?}" "${REMOVE_IMAGES:?}" "${REMOVE_DOCKER:?}"
step() { printf 'step: %s\n' "$*"; }

if id agentry >/dev/null 2>&1; then
	uid=$(id -u agentry)
	step "the agentry-runhost service and the account's user manager"
	loginctl disable-linger agentry 2>/dev/null || true
	systemctl stop "user@$uid.service" 2>/dev/null || true
	pkill -KILL -u agentry 2>/dev/null || true
	step "the agentry account and its home"
	userdel --remove agentry 2>/dev/null || userdel agentry
	rm -rf /var/lib/agentry
fi

if [ -f "$STAGE/daemon.json" ]; then
	step "gVisor's runtimes out of the Docker daemon"
	install -m 0644 "$STAGE/daemon.json" /etc/docker/daemon.json.new
	mv /etc/docker/daemon.json.new /etc/docker/daemon.json
	systemctl reload docker 2>/dev/null || true
fi
rm -f /etc/docker/daemon.json.before-agentry

if [ "$REMOVE_IMAGES" = yes ] && command -v docker >/dev/null 2>&1; then
	step "the images runs used"
	docker images --format '{{.Repository}}:{{.Tag}}@{{.ID}}' |
		awk -F@ '$1 ~ /^(ghcr\.io\/blaktron\/agentry-|agentry-)/ {print $2}' |
		sort -u | xargs -r docker image rm -f >/dev/null 2>&1 || true
fi

if [ "$REMOVE_DOCKER" = yes ]; then
	step "Docker Engine"
	export DEBIAN_FRONTEND=noninteractive
	apt-get purge -y -qq docker-ce docker-ce-cli containerd.io >/dev/null 2>&1 || true
	rm -f /etc/apt/sources.list.d/docker.sources /etc/apt/keyrings/docker.asc
fi
echo "done"
