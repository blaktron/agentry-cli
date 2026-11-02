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
#   docker-rpm.asc   Docker's RPM repository key, carried by the CLI
#   daemon.json      the Docker daemon's configuration with gVisor's two
#                    runtimes merged in, when it differs from the server's
#   authorized_keys  the agentry account's run keys, this device's included
#   devices.json     the paired devices, this device's included
#   config.json      the runhost's configuration, merged with the server's
#   agentry-runhost.service  the service's user unit
# Each file the install overwrites comes with the SHA-256 of the one the device
# merged it from (WAS_*, "none" for a file that was not there); a file changed
# on the server since then stops the install rather than being lost.
set -eu
# shellcheck disable=SC1090
. "$1"
: "${STAGE:?}" "${AGENTRY_VERSION:?}" "${AGENTRY_SHA256:?}" "${GVISOR_RELEASE:?}" "${GVISOR_SHA256:?}" "${INSTALL_DOCKER:?}" "${OS_ID:?}" "${PACKAGE:?}"
: "${WAS_AUTHORIZED_KEYS:?}" "${WAS_DEVICES_JSON:?}"

HOME_DIR=/var/lib/agentry
BIN=$HOME_DIR/bin
step() { printf 'step: %s\n' "$*"; }
fail() { printf 'error: %s\n' "$*"; exit 1; }
as_agentry() { runuser -u agentry -- env HOME="$HOME_DIR" XDG_RUNTIME_DIR="/run/user/$AGENTRY_UID" "$@"; }
# unchanged FILE HASH: FILE is still what the device read.
unchanged() {
	if [ "$2" = none ]; then
		[ ! -e "$1" ] || fail "$1 appeared on the server during setup: run setup again"
	else
		echo "$2  $1" | sha256sum -c --quiet - >/dev/null 2>&1 || fail "$1 changed on the server during setup: run setup again"
	fi
}

step "the agentry account"
if id agentry >/dev/null 2>&1; then
	[ "$(getent passwd agentry | cut -d: -f6)" = "$HOME_DIR" ] || fail "an account named agentry already exists with another home; setup will not take it over"
else
	# The login shell becomes the CLI once the CLI is installed, below.
	useradd --system --user-group --create-home --home-dir "$HOME_DIR" --shell /usr/sbin/nologin agentry
fi
# No password, and not a locked one: sshd refuses every sign-in to an account
# whose password is locked, a run key's included.
usermod -p '*' agentry
AGENTRY_UID=$(id -u agentry)
install -d -m 0700 -o agentry -g agentry "$HOME_DIR"
install -d -m 0755 -o agentry -g agentry "$BIN" "$HOME_DIR/cli" "$HOME_DIR/gvisor"

if [ "$INSTALL_DOCKER" = yes ]; then
	step "Docker Engine from Docker's repository"
	: "${DOCKER_REPO:?}"
	case "$PACKAGE" in
	apt)
		: "${OS_CODENAME:?}"
		export DEBIAN_FRONTEND=noninteractive
		install -d -m 0755 /etc/apt/keyrings
		install -m 0644 "$STAGE/docker.asc" /etc/apt/keyrings/docker.asc
		arch=$(dpkg --print-architecture)
		cat >/etc/apt/sources.list.d/docker.sources <<EOF
Types: deb
URIs: https://download.docker.com/linux/$DOCKER_REPO
Suites: $OS_CODENAME
Components: stable
Architectures: $arch
Signed-By: /etc/apt/keyrings/docker.asc
EOF
		apt-get update -qq
		apt-get install -y -qq docker-ce docker-ce-cli containerd.io >/dev/null
		;;
	dnf)
		: "${OS_RELEASE:?}"
		install -d -m 0755 /etc/pki/rpm-gpg
		install -m 0644 "$STAGE/docker-rpm.asc" /etc/pki/rpm-gpg/docker-ce.asc
		# The release is written out rather than left to $releasever, which
		# names a minor release on RHEL and its rebuilds.
		cat >/etc/yum.repos.d/docker-ce.repo <<EOF
[docker-ce-stable]
name=Docker CE Stable - \$basearch
baseurl=https://download.docker.com/linux/$DOCKER_REPO/$OS_RELEASE/\$basearch/stable
enabled=1
gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/docker-ce.asc
EOF
		# Docker's bridge network needs netfilter's addrtype match, which RHEL
		# and its rebuilds ship in kernel-modules-extra: the running kernel's
		# own, since a newer kernel's is no use before a reboot.
		if ! modinfo xt_addrtype >/dev/null 2>&1; then
			dnf -y -q install "kernel-modules-extra-$(uname -r)" >/dev/null 2>&1 || fail "the running kernel $(uname -r) lacks the netfilter modules Docker needs, and its kernel-modules-extra is not in the repositories: reboot into the newest kernel and run setup again"
		fi
		dnf -y -q install docker-ce docker-ce-cli containerd.io >/dev/null
		;;
	*) fail "setup does not install Docker Engine with $PACKAGE" ;;
	esac
	systemctl enable --now docker >/dev/null 2>&1
	# Remembered at once, so a removal can take away the Docker Engine this
	# setup installed even when the install stopped later.
	install -d -m 0700 -o agentry -g agentry "$HOME_DIR/runhost"
	touch "$HOME_DIR/runhost/installed-docker"
fi
command -v dockerd >/dev/null 2>&1 || fail "no Docker Engine on this server"
step "the agentry account in the docker group"
# A user manager already running when the account joins docker lacks the
# group until it restarts.
regroup=no
if ! id -nG agentry | tr ' ' '\n' | grep -qx docker && systemctl is-active --quiet "user@$AGENTRY_UID.service"; then
	regroup=yes
fi
usermod -aG docker agentry

step "gVisor $GVISOR_RELEASE"
gdir=$HOME_DIR/gvisor/$GVISOR_RELEASE
if [ ! -x "$gdir/runsc" ]; then
	echo "$GVISOR_SHA256  $STAGE/gvisor.tar.gz" | sha256sum -c --quiet - || fail "the gVisor archive that arrived is not the one the device verified"
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
# The login shell is the CLI under its shell's name, which runs the forced
# command and nothing else.
ln -sfn agentry "$BIN/agentry-shell"
chown -h agentry:agentry "$BIN/agentry-shell"
usermod --shell "$BIN/agentry-shell" agentry

if [ -f "$STAGE/daemon.json" ]; then
	step "gVisor's runtimes in the Docker daemon"
	: "${WAS_DAEMON_JSON:?}"
	install -d -m 0755 /etc/docker
	unchanged /etc/docker/daemon.json "$WAS_DAEMON_JSON"
	mode=644
	if [ -f /etc/docker/daemon.json ]; then
		mode=$(stat -c %a /etc/docker/daemon.json)
		[ -f /etc/docker/daemon.json.before-agentry ] || cp -p /etc/docker/daemon.json /etc/docker/daemon.json.before-agentry
	fi
	install -m "$mode" "$STAGE/daemon.json" /etc/docker/daemon.json.new
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
install -m 0600 -o agentry -g agentry "$STAGE/config.json" "$HOME_DIR/runhost/config.json"
unchanged "$HOME_DIR/runhost/devices.json" "$WAS_DEVICES_JSON"
install -m 0600 -o agentry -g agentry "$STAGE/devices.json" "$HOME_DIR/runhost/devices.json"
unchanged "$HOME_DIR/.ssh/authorized_keys" "$WAS_AUTHORIZED_KEYS"
install -m 0600 -o agentry -g agentry "$STAGE/authorized_keys" "$HOME_DIR/.ssh/authorized_keys"

# Under SELinux, sshd starts the account's login shell, the CLI, only from a
# file the policy lets a user session begin in, and the home's default label
# is not one: a module of file contexts labels the CLI's directories as
# commands (bin_t), which a relabel keeps and an update's new CLI inherits.
# sshd reads the run keys as ssh_home_t, the policy's own label for them.
if command -v selinuxenabled >/dev/null 2>&1 && selinuxenabled; then
	step "SELinux labels for the CLI"
	cat >"$STAGE/agentry-runhost.cil" <<'EOF'
(filecon "/var/lib/agentry/bin(/.*)?" any (system_u object_r bin_t ((s0) (s0))))
(filecon "/var/lib/agentry/cli(/.*)?" any (system_u object_r bin_t ((s0) (s0))))
EOF
	semodule -i "$STAGE/agentry-runhost.cil" || fail "the SELinux module of the CLI's labels did not load"
	restorecon -R "$HOME_DIR"
fi

step "the account's user manager"
loginctl enable-linger agentry
i=0
until [ -d "/run/user/$AGENTRY_UID" ] && systemctl is-active --quiet "user@$AGENTRY_UID.service"; do
	i=$((i + 1))
	[ $i -lt 30 ] || fail "the agentry account's user manager did not start"
	sleep 1
done
if [ "$regroup" = yes ]; then
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
