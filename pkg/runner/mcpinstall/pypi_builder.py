# The PyPI MCP-server builder. It runs in the policy's Python runtime image as
#
#   python -c <this file> PREFIX PACKAGE VERSION INDEX ONLY_BINARY
#
# so every value arrives as its own argv element and reaches pip as one, never
# through a shell. Wheels only unless ONLY_BINARY is empty (policy
# pypiAllowSourceDists), and the file `pip download --no-deps` picked must be
# one the registry advertised for the pin, with that digest, before anything is
# installed. Exit 3: a file the registry did not advertise; exit 4: a digest
# mismatch; otherwise pip's own exit code.
import hashlib
import os
import subprocess
import sys

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


def run(argv):
    code = subprocess.run(argv).returncode
    if code != 0:
        sys.exit(code)


def main():
    prefix, package, version, index, only_binary = sys.argv[1:6]
    # As the invoking user, whose HOME does not exist in the image.
    os.environ["HOME"] = "/tmp"
    venv = os.path.join(prefix, "venv")
    dl = os.path.join(prefix, "dl")
    pip = os.path.join(venv, "bin", "pip")
    wheels = [only_binary] if only_binary else []
    requirement = package + "==" + version

    run([sys.executable, "-m", "venv", venv])
    os.makedirs(dl, exist_ok=True)
    run([pip, "download", "--quiet", "--no-cache-dir", "--no-deps", *wheels,
         "--index-url", index, "--dest", dl, requirement])

    advertised = {}
    with open(os.path.join(prefix, ARTIFACT_SUMS)) as f:
        for line in f:
            digest, sep, name = line.rstrip("\n").partition("  ")
            if sep:
                advertised[name] = digest
    for name in sorted(os.listdir(dl)):
        want = advertised.get(name)
        if not want:
            print(f"mcp install: {name} is not an artifact the registry advertised for {requirement}")
            sys.exit(3)
        with open(os.path.join(dl, name), "rb") as f:
            got = hashlib.sha256(f.read()).hexdigest()
        if got != want:
            print(f"mcp install: {name} does not match the advertised digest")
            sys.exit(4)

    run([pip, "install", "--quiet", "--no-cache-dir", *wheels,
         "--index-url", index, "--find-links", dl, requirement])
    run([os.path.join(venv, "bin", "python"), "-c", CONSOLE_SCRIPTS,
         package, os.path.join(prefix, CONSOLE_SCRIPTS_FILE)])


main()
