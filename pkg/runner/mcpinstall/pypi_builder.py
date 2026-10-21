# The PyPI MCP-server builder. It runs in the policy's Python runtime image as
#
#   python -c <this file> PREFIX PACKAGE VERSION INDEX ONLY_BINARY
#
# so every value arrives as its own argv element and reaches pip as one, never
# through a shell. Wheels only unless ONLY_BINARY is empty (policy
# pypiAllowSourceDists), and the file `pip download --no-deps` picked must be
# one the registry advertised for the pin, with that digest, before anything is
# installed. The advertised files and their digests arrive on stdin, one
# `<sha256>  <filename>` line each, and are kept as PREFIX/artifacts.sha256.
#
# The install is written inside the container's own filesystem, at PREFIX, and
# the builder ends by writing that tree to stdout as one tar archive, which the
# runner unpacks into the server's store. Everything else it and pip print goes
# to stderr. The tree holds no link, which a store on Windows could not keep:
# the environment copies its interpreter rather than linking to the image's
# (venv --copies), and the lib64 alias venv makes for lib is removed.
#
# Exit 3: a file the registry did not advertise; exit 4: a digest mismatch;
# otherwise pip's own exit code.
import hashlib
import os
import stat
import subprocess
import sys
import tarfile

# Kept in step with artifactSumsFile and consoleScriptsFile in
# mcpinstall.go (a test holds them together).
ARTIFACT_SUMS = "artifacts.sha256"
CONSOLE_SCRIPTS_FILE = "console-scripts"

# Run by the venv's own interpreter, where the installed distribution is:
# the console scripts it declares, one per line.
CONSOLE_SCRIPTS = (
    "import importlib.metadata as m, sys\n"
    "eps = sorted(e.name for e in m.distribution(sys.argv[1]).entry_points"
    " if e.group == 'console_scripts')\n"
    "open(sys.argv[2], 'w').write('\\n'.join(eps) + '\\n')\n"
)


def say(message):
    print(message, file=sys.stderr, flush=True)


def run(argv):
    code = subprocess.run(argv, stdin=subprocess.DEVNULL, stdout=sys.stderr).returncode
    if code != 0:
        sys.exit(code)


def archive(prefix, out):
    """Write the tree under prefix to out as a tar archive: folders, regular
    files with their executable bits, and any link as the link it is (which
    the runner refuses), in sorted order, each named relative to prefix. Hard
    links are written as files of their own."""
    with tarfile.open(fileobj=out, mode="w|", format=tarfile.PAX_FORMAT) as tar:
        for top, dirs, files in os.walk(prefix):
            dirs.sort()
            names = sorted(dirs + files)
            for name in names:
                full = os.path.join(top, name)
                rel = os.path.relpath(full, prefix)
                st = os.lstat(full)
                info = tarfile.TarInfo(rel)
                info.mtime = 0
                if stat.S_ISLNK(st.st_mode):
                    info.type = tarfile.SYMTYPE
                    info.linkname = os.readlink(full)
                    info.mode = 0o777
                    tar.addfile(info)
                elif stat.S_ISDIR(st.st_mode):
                    info.type = tarfile.DIRTYPE
                    info.mode = 0o755
                    tar.addfile(info)
                elif stat.S_ISREG(st.st_mode):
                    info.type = tarfile.REGTYPE
                    info.mode = 0o755 if st.st_mode & 0o111 else 0o644
                    info.size = st.st_size
                    with open(full, "rb") as f:
                        tar.addfile(info, f)
                else:
                    say(f"mcp install: {rel} is neither a file, a folder nor a link; left out")
            # os.walk lists a link to a folder among the folders; it is
            # archived as the link it is and never descended into.
            dirs[:] = [d for d in dirs if not os.path.islink(os.path.join(top, d))]


def main():
    prefix, package, version, index, only_binary = sys.argv[1:6]
    # Whoever the image runs as, HOME may not exist in the image.
    os.environ["HOME"] = "/tmp"
    venv = os.path.join(prefix, "venv")
    dl = os.path.join(prefix, "dl")
    pip = os.path.join(venv, "bin", "pip")
    wheels = [only_binary] if only_binary else []
    requirement = package + "==" + version

    os.makedirs(prefix, exist_ok=True)
    sums = sys.stdin.read()
    with open(os.path.join(prefix, ARTIFACT_SUMS), "w") as f:
        f.write(sums)

    run([sys.executable, "-m", "venv", "--copies", venv])
    lib64 = os.path.join(venv, "lib64")
    if os.path.islink(lib64):
        os.unlink(lib64)
    os.makedirs(dl, exist_ok=True)
    run([pip, "download", "--quiet", "--no-cache-dir", "--no-deps", *wheels,
         "--index-url", index, "--dest", dl, requirement])

    advertised = {}
    for line in sums.splitlines():
        digest, sep, name = line.partition("  ")
        if sep:
            advertised[name] = digest
    for name in sorted(os.listdir(dl)):
        want = advertised.get(name)
        if not want:
            say(f"mcp install: {name} is not an artifact the registry advertised for {requirement}")
            sys.exit(3)
        with open(os.path.join(dl, name), "rb") as f:
            got = hashlib.sha256(f.read()).hexdigest()
        if got != want:
            say(f"mcp install: {name} does not match the advertised digest")
            sys.exit(4)

    run([pip, "install", "--quiet", "--no-cache-dir", "--no-compile", *wheels,
         "--index-url", index, "--find-links", dl, requirement])
    # Compiled to be loaded without a check of the source's modification
    # time, which the archive does not carry to the store: a timestamp-checked
    # module would be found stale and recompiled at every start, from a store
    # the server can only read. A module that does not compile (some
    # packages ship files for another Python) is left to its source, as pip
    # leaves it; compileall's exit code is not the install's.
    subprocess.run([os.path.join(venv, "bin", "python"), "-m", "compileall", "-qq",
                    "--invalidation-mode", "unchecked-hash", os.path.join(venv, "lib")],
                   stdin=subprocess.DEVNULL, stdout=sys.stderr)
    run([os.path.join(venv, "bin", "python"), "-c", CONSOLE_SCRIPTS,
         package, os.path.join(prefix, CONSOLE_SCRIPTS_FILE)])

    archive(prefix, sys.stdout.buffer)
    sys.stdout.buffer.flush()


main()
