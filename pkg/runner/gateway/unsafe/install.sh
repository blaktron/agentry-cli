#!/bin/sh
# The install step of an Unsafe Mode exec image (agentry-cli#490; agentry-notes
# plans/unsafe-mode.md §4.3, D4, D8). Generated contexts copy it unchanged:
# every value it acts on is a line of a data file beside it, read into a
# variable and quoted, never spelled into a shell line, and each was held to
# a grammar by the CLI before it was written (invariant 3).
#
#   packages.txt  one Kali package a line
#   pip.txt       command<TAB>name==version
#   links.txt     command<TAB>package<TAB>path inside the package
#   git           present when git is declared
#
# Each package and each extra is installed on its own, and one that fails is
# recorded and skipped: the build goes on and the run goes without that tool
# (D8). The records are read back into the run's receipt:
#
#   /etc/agentry/exec-packages  package=version, and pip name==version sha256:…
#   /etc/agentry/exec-failed    what<TAB>why, one line per failure
set -u
ctx=/agentry-build
rec=/etc/agentry
mkdir -p "$rec"
: >"$rec/exec-packages"
: >"$rec/exec-failed"
tab=$(printf '\t')
export DEBIAN_FRONTEND=noninteractive

fail() { printf '%s\t%s\n' "$1" "$(printf '%s' "$2" | tr '\t\n' '  ' | cut -c1-300)" >>"$rec/exec-failed"; }
lastline() { printf '%s\n' "$1" | sed '/^[[:space:]]*$/d' | tail -n 1; }

# A mirror caught mid-sync serves an index that disagrees with its
# InRelease, which apt refuses; another try reaches another mirror.
updated=0
if [ -s "$ctx/packages.txt" ]; then
	for try in 1 2 3 4 5; do
		if apt-get update >/tmp/apt-update.log 2>&1 </dev/null; then
			updated=1
			break
		fi
		[ "$try" = 5 ] || sleep 20
	done
fi
while IFS= read -r p; do
	[ -n "$p" ] || continue
	if [ "$updated" != 1 ]; then
		fail "$p" "apt-get update failed: $(lastline "$(cat /tmp/apt-update.log)")"
		continue
	fi
	if out=$(apt-get install -y --no-install-recommends "$p" 2>&1 </dev/null); then
		dpkg-query -W -f='${Package}=${Version}\n' "$p" >>"$rec/exec-packages" 2>/dev/null
	else
		fail "$p" "$(lastline "$out")"
	fi
done <"$ctx/packages.txt"

if [ -s "$ctx/pip.txt" ]; then
	if venv=$(python3 -m venv /opt/agentry/venv 2>&1 </dev/null); then
		while IFS="$tab" read -r cmd req; do
			[ -n "$cmd" ] || continue
			if out=$(/opt/agentry/venv/bin/pip install --no-cache-dir --disable-pip-version-check --report /tmp/pip-report.json "$req" 2>&1 </dev/null); then
				python3 "$ctx/pip-record.py" /tmp/pip-report.json >>"$rec/exec-packages"
				if [ -x "/opt/agentry/venv/bin/$cmd" ]; then
					ln -sf "/opt/agentry/venv/bin/$cmd" "/usr/local/bin/$cmd"
				else
					fail "$cmd" "pip installed $req, which puts no command named $cmd in the venv"
				fi
			else
				fail "$cmd" "$(lastline "$out")"
			fi
		done <"$ctx/pip.txt"
	else
		while IFS="$tab" read -r cmd req; do
			[ -n "$cmd" ] && fail "$cmd" "python3 -m venv failed: $(lastline "$venv")"
		done <"$ctx/pip.txt"
	fi
fi

while IFS="$tab" read -r cmd pkg path; do
	[ -n "$cmd" ] || continue
	if [ -e "/$path" ]; then
		ln -sf "/$path" "/usr/local/bin/$cmd"
	else
		fail "$cmd" "/$path is not in the image: the package $pkg did not install, or no longer ships it"
	fi
done <"$ctx/links.txt"

if [ -e "$ctx/git" ]; then
	# A bind-mounted workspace owned by the host user, read by root inside
	# the container, is "dubious ownership" to git; exactly the run's mounts
	# are trusted, as in the catalogue's exec image.
	for dir in /workspace '/workspace/*' '/inputs/*'; do
		git config --system --add safe.directory "$dir" || fail git "git config safe.directory $dir failed"
	done
fi

apt-get clean >/dev/null 2>&1
rm -rf /var/lib/apt/lists/* /tmp/apt-update.log /tmp/pip-report.json
exit 0
