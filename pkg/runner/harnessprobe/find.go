package harnessprobe

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// Source says where a harness executable was found. The values are wire
// values: doctor prints them and the desktop's Coding agent step reads
// them from the probe's JSON.
type Source string

const (
	// SourcePolicy is the operator policy's harness.<name>.path.
	SourcePolicy Source = "policy"
	// SourcePath is the process's PATH.
	SourcePath Source = "PATH"
	// SourceKnown is one of the adapter's well-known install locations — the
	// native installer's directory, or a desktop app's bundled build — which a
	// PATH-less GUI launch never sees.
	SourceKnown Source = "known location"
)

// The seams the tests set: the home directory and the platform whose
// well-known locations apply.
var (
	homeDir = os.UserHomeDir
	goos    = runtime.GOOS
)

// Find resolves the executable behind adapter name: the policy's
// harness.<name>.path when set (an unusable one is an error, never a
// fallback — the operator said where it is), else PATH, else the
// adapter's well-known locations in order. It reads and runs nothing but
// os.Stat. The engine and the probe both use it, so `agentry run` finds
// exactly what `agentry doctor` reported (and what the desktop's wizard
// showed).
func Find(pol *policy.Policy, name string) (path string, source Source, err error) {
	ad, err := adapter.Get(name)
	if err != nil {
		return "", "", err
	}
	hh, ok := ad.(adapter.HostHarness)
	if !ok {
		return "", "", fmt.Errorf("%s is a containerised harness; there is no executable to find", name)
	}
	return find(pol, name, hh)
}

// FindExecutable is Find for the engine, which holds the plan's command
// name as well as the adapter's: when the adapter is not registered as a
// host harness in this build, the lookup is the same rule over that
// command name alone (no well-known locations, which only an adapter can
// name), so the run and the probe never word a refusal differently.
func FindExecutable(pol *policy.Policy, name, command string) (string, Source, error) {
	if ad, err := adapter.Get(name); err == nil {
		if hh, ok := ad.(adapter.HostHarness); ok {
			return find(pol, name, hh)
		}
	}
	return find(pol, name, commandOnly(command))
}

// commandOnly is a HostHarness with a name and no locations, for
// FindExecutable's stand-in.
type commandOnly string

func (c commandOnly) Executable() string           { return string(c) }
func (commandOnly) ProbeVersion() string           { return "" }
func (commandOnly) VersionArgv() []string          { return nil }
func (commandOnly) InstallURL() string             { return "" }
func (commandOnly) DisplayName() string            { return "" }
func (commandOnly) MCPCommand() string             { return "" }
func (commandOnly) WellKnownPaths(string) []string { return nil }

func find(pol *policy.Policy, name string, hh adapter.HostHarness) (string, Source, error) {
	if pol != nil {
		if hp := pol.Harness[name]; hp.Path != "" {
			if _, statErr := os.Stat(hp.Path); statErr != nil {
				return "", SourcePolicy, fmt.Errorf("policy harness.%s.path %s: %w", name, hp.Path, statErr)
			}
			return hp.Path, SourcePolicy, nil
		}
	}
	if p, lerr := lookPath(hh.Executable()); lerr == nil {
		return p, SourcePath, nil
	}
	if p, ok := wellKnown(hh.WellKnownPaths(goos)); ok {
		return p, SourceKnown, nil
	}
	return "", "", fmt.Errorf("%s is not installed on this machine (not on PATH, and not in its usual install locations) — install it, or set harness.%s.path in the policy",
		hh.Executable(), name)
}

// wellKnown returns the first template that expands to an existing
// regular file, applying the rule adapter.HostHarness.WellKnownPaths
// states — that comment is the definition; nothing here adds to it.
func wellKnown(templates []string) (string, bool) {
	for _, tpl := range templates {
		p, ok := expand(tpl)
		if !ok {
			continue
		}
		if strings.HasSuffix(p, "*") {
			p, ok = newestVersion(strings.TrimSuffix(p, "*"))
			if !ok {
				continue
			}
		}
		if isFile(p) {
			return p, true
		}
	}
	return "", false
}

// bracedVar matches the one variable form the rule admits, "${NAME}".
var bracedVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expand resolves a template into a path for this platform: the braced
// variables first, then a whole-element leading "~"; false when a
// variable is unset or empty, or the home directory cannot be found.
func expand(tpl string) (string, bool) {
	skip := false
	tpl = bracedVar.ReplaceAllStringFunc(tpl, func(m string) string {
		v, ok := os.LookupEnv(m[2 : len(m)-1])
		if !ok || v == "" {
			skip = true
		}
		return v
	})
	if skip {
		return "", false
	}
	if tpl == "~" || strings.HasPrefix(tpl, "~/") {
		home, err := homeDir()
		if err != nil || home == "" {
			return "", false
		}
		tpl = home + tpl[1:]
	}
	return filepath.FromSlash(tpl), true
}

// newestVersion picks, in a directory of version-named entries (a
// desktop app keeps the builds it has downloaded side by side, the
// newest being the one it runs), the file whose name sorts highest as a
// dotted version; "2.1.275.exe" and "2.1.275" both read as 2.1.275.
func newestVersion(dir string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	best, bestVer, bestExe := "", []int(nil), false
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if !isFile(p) {
			continue
		}
		v := versionOf(e.Name())
		if v == nil {
			continue
		}
		exe := hasExecutableExt(e.Name())
		switch c := compareVersions(v, bestVer); {
		case best == "", c > 0, c == 0 && exe && !bestExe:
			// A newer build, or the same build under the name that
			// runs: on Windows "2.1.275" beside "2.1.275.exe" is the
			// one that does not.
			best, bestVer, bestExe = p, v, exe
		}
	}
	return best, best != ""
}

// executableExts are the Windows extensions a build may carry; stripped
// before the version is read (filepath.Ext would take ".275" off
// "2.1.275" and make every build the same version).
var executableExts = []string{".exe", ".cmd", ".bat"}

func hasExecutableExt(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range executableExts {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// versionOf reads the leading dotted number of a file name ("2.1.275",
// "2.1.275.exe", "2.1.275-beta" → [2 1 275]); nil when the name does not
// start with one. Each segment is read up to its first non-digit, so a
// prerelease suffix ends the number without demoting the build.
func versionOf(name string) []int {
	lower := strings.ToLower(name)
	for _, ext := range executableExts {
		if strings.HasSuffix(lower, ext) {
			name = name[:len(name)-len(ext)]
			break
		}
	}
	var v []int
	for _, part := range strings.Split(name, ".") {
		digits := part
		if i := strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
			digits = part[:i]
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			break
		}
		v = append(v, n)
		if len(digits) != len(part) {
			break // "275-beta": the number ends here
		}
	}
	return v
}

func compareVersions(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

// isFile follows a link and asks for a regular file: a directory named
// like the executable, or a dangling link, is not an installation.
func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}
