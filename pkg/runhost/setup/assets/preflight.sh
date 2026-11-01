#!/bin/sh
# A remote runhost's preflight: what the server is, read only. It changes
# nothing. Each fact is one `key=value` line; a fact that cannot be read is
# left out, and the device decides what is missing. Run as root.
#
# Inputs (environment): AGENTRY_PREFLIGHT_HOSTS, the hosts whose port 443
# must be reachable, space separated.
set -u
fact() { printf '%s=%s\n' "$1" "$(printf '%s' "$2" | tr '\n' ' ')"; }

if [ -r /etc/os-release ]; then
	# shellcheck disable=SC1091
	. /etc/os-release
	fact os_id "${ID:-}"
	fact os_version_id "${VERSION_ID:-}"
	fact os_pretty "${PRETTY_NAME:-}"
	fact os_like "${ID_LIKE:-}"
	fact os_codename "${VERSION_CODENAME:-${UBUNTU_CODENAME:-}}"
fi
fact arch "$(uname -m)"
fact kernel "$(uname -r)"
if command -v systemctl >/dev/null 2>&1; then
	fact systemd "$(systemctl --version 2>/dev/null | awk 'NR==1{print $2}')"
	fact systemd_running "$( [ -d /run/systemd/system ] && echo yes || echo no )"
fi
fact uid "$(id -u)"
if [ -e /dev/tpmrm0 ] || [ -e /dev/tpm0 ]; then fact tpm2 yes; else fact tpm2 no; fi
df_dir=/var/lib
[ -d /var/lib/agentry ] && df_dir=/var/lib/agentry
fact disk_free_kb "$(df -Pk "$df_dir" 2>/dev/null | awk 'NR==2{print $4}')"
fact mem_available_kb "$(awk '/^MemAvailable:/{print $2}' /proc/meminfo 2>/dev/null)"
fact mem_total_kb "$(awk '/^MemTotal:/{print $2}' /proc/meminfo 2>/dev/null)"

# Container engines already here.
if command -v dockerd >/dev/null 2>&1; then
	fact dockerd yes
	fact docker_version "$(dockerd --version 2>/dev/null)"
	if command -v docker >/dev/null 2>&1; then
		fact docker_runtimes "$(docker info --format '{{json .Runtimes}}' 2>/dev/null)"
		fact docker_server "$(docker version --format '{{.Server.Version}}' 2>/dev/null)"
	fi
else
	fact dockerd no
fi
for e in podman nerdctl containerd crio; do
	if command -v "$e" >/dev/null 2>&1; then fact "engine_$e" yes; fi
done
# LXD and Incus count only when their daemon runs: Ubuntu's images carry an
# lxd command that only installs it.
for s in /var/snap/lxd/common/lxd/unix.socket /var/lib/lxd/unix.socket; do
	[ -S "$s" ] && fact engine_lxd yes
done
[ -S /var/lib/incus/unix.socket ] && fact engine_incus yes
[ -r /etc/docker/daemon.json ] && fact daemon_json yes

# The service's account, if an earlier setup made it.
if id agentry >/dev/null 2>&1; then
	fact agentry_user yes
	fact agentry_uid "$(id -u agentry)"
	fact agentry_shell "$(getent passwd agentry | cut -d: -f7)"
	fact agentry_home "$(getent passwd agentry | cut -d: -f6)"
else
	fact agentry_user no
fi
command -v apt-get >/dev/null 2>&1 && fact apt yes
command -v dnf >/dev/null 2>&1 && fact dnf yes
command -v sshd >/dev/null 2>&1 && fact sshd_allow "$(sshd -T 2>/dev/null | awk '$1=="allowusers"||$1=="allowgroups"||$1=="denyusers"||$1=="denygroups"{printf "%s %s; ", $1, $2}')"

# Every listening socket, so setup can show what is open and prove after the
# install that it added none.
if command -v ss >/dev/null 2>&1; then
	fact listening "$(ss -H -ltnu 2>/dev/null | awk '{print $1"/"$5}' | sort -u | tr '\n' ' ')"
fi

# Outbound HTTPS, as TCP reachability of port 443.
for h in ${AGENTRY_PREFLIGHT_HOSTS:-}; do
	if command -v timeout >/dev/null 2>&1 && command -v bash >/dev/null 2>&1; then
		if timeout 8 bash -c "exec 3<>/dev/tcp/$h/443" 2>/dev/null; then fact "reach_$h" yes; else fact "reach_$h" no; fi
	fi
done
exit 0
