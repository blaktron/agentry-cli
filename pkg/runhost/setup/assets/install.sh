#!/bin/sh
# A remote runhost's install, run as root by `agentry runhost add`. Every step
# is idempotent: a second run over a finished install changes nothing but what
# the device staged anew. Each step says `step: …` as it starts; the last line
# is `done`.
#
# Inputs: the environment file $1 (written by the device), and the files the
# device staged beside this script in $STAGE:
#   agentry          the CLI release for this server, verified on the device
#   gvisor.tar.gz    runsc and its helpers, verified on the device
#   docker.asc       Docker's apt repository key, carried by the CLI
#   daemon.json      the Docker daemon's configuration with gVisor's two
#                    runtimes merged in, when it differs from the server's
#   authorized_keys  the agentry account's run keys, this device's included
#   devices.json     the paired devices, this device's included
#   config.json      the runhost's configuration, for a first install
#   agentry-runhost.service  the service's user unit
set -eu
# shellcheck disable=SC1090
. "$1"
: "${STAGE:?}" "${AGENTRY_VERSION:?}" "${AGENTRY_SHA256:?}" "${GVISOR_RELEASE:?}" "${INSTALL_DOCKER:?}" "${OS_ID:?}"

HOME_DIR=/var/lib/agentry
BIN=$HOME_DIR/bin
step() { printf 'step: %s\n' "$*"; }
fail() { printf 'error: %s\n' "$*"; exit 1; }
as_agentry() { runuser -u agentry -- env HOME="$HOME_DIR" XDG_RUNTIME_DIR="/run/user/$AGENTRY_UID" "$@"; }

step "the agentry account"
if id agentry >/dev/null 2>&1; then
	[ "$(getent passwd agentry | cut -d: -f6)" = "$HOME_DIR" ] || fail "an account named agentry already exists with another home; setup will not take it over"
	usermod --shell "$BIN/agentry" agentry
else
	useradd --system --user-group --create-home --home-dir "$HOME_DIR" --shell "$BIN/agentry" agentry
fi
# No password, and not a locked one: sshd refuses every sign-in to an account
# whose password is locked, a run key's included.
usermod -p '*' agentry
AGENTRY_UID=$(id -u agentry)
install -d -m 0700 -o agentry -g agentry "$HOME_DIR"
install -d -m 0755 -o agentry -g agentry "$BIN" "$HOME_DIR/cli" "$HOME_DIR/gvisor"

if [ "$INSTALL_DOCKER" = yes ]; then
	step "Docker Engine from Docker's repository"
	export DEBIAN_FRONTEND=noninteractive
	install -d -m 0755 /etc/apt/keyrings
	install -m 0644 "$STAGE/docker.asc" /etc/apt/keyrings/docker.asc
	arch=$(dpkg --print-architecture)
	cat >/etc/apt/sources.list.d/docker.sources <<EOF
Types: deb
URIs: https://download.docker.com/linux/$OS_ID
Suites: $OS_CODENAME
Components: stable
Architectures: $arch
Signed-By: /etc/apt/keyrings/docker.asc
EOF
	apt-get update -qq
	apt-get install -y -qq docker-ce docker-ce-cli containerd.io >/dev/null
	systemctl enable --now docker >/dev/null 2>&1
fi
command -v dockerd >/dev/null 2>&1 || fail "no Docker Engine on this server"
step "the agentry account in the docker group"
regroup=no
id -nG agentry | tr ' ' '\n' | grep -qx docker || regroup=yes
usermod -aG docker agentry

step "gVisor $GVISOR_RELEASE"
gdir=$HOME_DIR/gvisor/$GVISOR_RELEASE
if [ ! -x "$gdir/runsc" ]; then
	rm -rf "$gdir.new"
	install -d -m 0755 -o agentry -g agentry "$gdir.new"
	tar -xzf "$STAGE/gvisor.tar.gz" -C "$gdir.new" --no-same-owner
	[ -x "$gdir.new/runsc" ] || fail "the gVisor archive holds no runsc"
	chown -R agentry:agentry "$gdir.new"
	mv "$gdir.new" "$gdir"
fi
ln -sfn "../gvisor/$GVISOR_RELEASE/runsc" "$BIN/runsc"
chown -h agentry:agentry "$BIN/runsc"
"$BIN/runsc" --version >/dev/null || fail "runsc does not run on this server"

step "the Agentry CLI $AGENTRY_VERSION"
cdir=$HOME_DIR/cli/$AGENTRY_VERSION
echo "$AGENTRY_SHA256  $STAGE/agentry" | sha256sum -c --quiet - || fail "the CLI that arrived is not the one the device verified"
install -d -m 0755 -o agentry -g agentry "$cdir"
install -m 0755 -o agentry -g agentry "$STAGE/agentry" "$cdir/agentry"
ln -sfn "../cli/$AGENTRY_VERSION/agentry" "$BIN/agentry"
chown -h agentry:agentry "$BIN/agentry"
got=$(runuser -u agentry -- "$BIN/agentry" --version 2>&1) || fail "the CLI does not run on this server: $got"
case "$got" in *"$AGENTRY_VERSION"*) ;; *) fail "the CLI reports $got, not $AGENTRY_VERSION" ;; esac

if [ -f "$STAGE/daemon.json" ]; then
	step "gVisor's runtimes in the Docker daemon"
	install -d -m 0755 /etc/docker
	if [ -f /etc/docker/daemon.json ] && [ ! -f /etc/docker/daemon.json.before-agentry ]; then
		cp -p /etc/docker/daemon.json /etc/docker/daemon.json.before-agentry
	fi
	install -m 0644 "$STAGE/daemon.json" /etc/docker/daemon.json.new
	mv /etc/docker/daemon.json.new /etc/docker/daemon.json
	# Runtimes are reloadable: a reload changes no running container.
	if systemctl is-active --quiet docker; then systemctl reload docker; else systemctl start docker; fi
fi
i=0
until docker info --format '{{json .Runtimes}}' 2>/dev/null | grep -q '"runsc-uds"'; do
	i=$((i + 1))
	[ $i -lt 30 ] || fail "the Docker daemon does not list runsc-uds after its reload"
	sleep 1
done

step "the runhost's state and this device's run key"
install -d -m 0700 -o agentry -g agentry "$HOME_DIR/runhost" "$HOME_DIR/.ssh"
if [ ! -f "$HOME_DIR/runhost/config.json" ]; then
	install -m 0600 -o agentry -g agentry "$STAGE/config.json" "$HOME_DIR/runhost/config.json"
fi
install -m 0600 -o agentry -g agentry "$STAGE/devices.json" "$HOME_DIR/runhost/devices.json"
install -m 0600 -o agentry -g agentry "$STAGE/authorized_keys" "$HOME_DIR/.ssh/authorized_keys"

step "the account's user manager"
loginctl enable-linger agentry
i=0
until [ -d "/run/user/$AGENTRY_UID" ] && systemctl is-active --quiet "user@$AGENTRY_UID.service"; do
	i=$((i + 1))
	[ $i -lt 30 ] || fail "the agentry account's user manager did not start"
	sleep 1
done
if [ "$regroup" = yes ]; then
	# A user manager started before the account joined docker lacks the group.
	systemctl restart "user@$AGENTRY_UID.service"
fi

step "the images runs need (this can take some minutes)"
as_agentry "$BIN/agentry" runhost service prepare

step "the agentry-runhost service"
install -d -m 0755 -o agentry -g agentry "$HOME_DIR/.config" "$HOME_DIR/.config/systemd" "$HOME_DIR/.config/systemd/user"
install -m 0644 -o agentry -g agentry "$STAGE/agentry-runhost.service" "$HOME_DIR/.config/systemd/user/agentry-runhost.service"
as_agentry systemctl --user daemon-reload
as_agentry systemctl --user enable agentry-runhost.service >/dev/null 2>&1
as_agentry systemctl --user restart agentry-runhost.service
i=0
until [ -S "/run/user/$AGENTRY_UID/agentry-runhost.sock" ]; do
	i=$((i + 1))
	[ $i -lt 30 ] || fail "the service did not open its socket: $(as_agentry systemctl --user status agentry-runhost.service --no-pager 2>&1 | tail -5)"
	sleep 1
done

printf 'fact: listening_after=%s\n' "$(ss -H -ltnu 2>/dev/null | awk '{print $1"/"$5}' | sort -u | tr '\n' ' ')"
printf 'fact: agentry_uid=%s\n' "$AGENTRY_UID"
echo "done"
