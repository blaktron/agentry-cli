#!/bin/sh
# A device's pairing with a runhost set up before, run as root by
# `agentry runhost pair`. It installs nothing but the device's two keys, and,
# when the device binds the runhost to its account, the configuration.
#
# Inputs: the environment file $1 and, in $STAGE, authorized_keys and
# devices.json with the device's keys merged in, and config.json when the
# owner changes. Each comes with the SHA-256 of the file the device merged
# it from (WAS_*); a file changed on the server since then stops the pairing.
set -eu
# shellcheck disable=SC1090
. "$1"
: "${STAGE:?}" "${WAS_AUTHORIZED_KEYS:?}" "${WAS_DEVICES_JSON:?}"
HOME_DIR=/var/lib/agentry
step() { printf 'step: %s\n' "$*"; }
fail() { printf 'error: %s\n' "$*"; exit 1; }
unchanged() {
	if [ "$2" = none ]; then
		[ ! -e "$1" ] || fail "$1 appeared on the server during pairing: pair again"
	else
		echo "$2  $1" | sha256sum -c --quiet - >/dev/null 2>&1 || fail "$1 changed on the server during pairing: pair again"
	fi
}

[ -f "$HOME_DIR/runhost/config.json" ] && [ -x "$HOME_DIR/bin/agentry" ] || fail "this server is not a runhost: set it up with agentry runhost add"
[ "$(getent passwd agentry | cut -d: -f6)" = "$HOME_DIR" ] || fail "the agentry account here is not a runhost's"

step "this device's run key and sealing key"
if [ -f "$STAGE/config.json" ]; then
	: "${WAS_CONFIG_JSON:?}"
	unchanged "$HOME_DIR/runhost/config.json" "$WAS_CONFIG_JSON"
	install -m 0600 -o agentry -g agentry "$STAGE/config.json" "$HOME_DIR/runhost/config.json"
fi
unchanged "$HOME_DIR/runhost/devices.json" "$WAS_DEVICES_JSON"
install -m 0600 -o agentry -g agentry "$STAGE/devices.json" "$HOME_DIR/runhost/devices.json"
install -d -m 0700 -o agentry -g agentry "$HOME_DIR/.ssh"
unchanged "$HOME_DIR/.ssh/authorized_keys" "$WAS_AUTHORIZED_KEYS"
install -m 0600 -o agentry -g agentry "$STAGE/authorized_keys" "$HOME_DIR/.ssh/authorized_keys"
if command -v restorecon >/dev/null 2>&1 && command -v selinuxenabled >/dev/null 2>&1 && selinuxenabled; then
	restorecon -R "$HOME_DIR/.ssh" "$HOME_DIR/runhost"
fi
echo "done"
