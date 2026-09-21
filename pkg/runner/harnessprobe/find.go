package harnessprobe

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// Source says where a harness executable was found.
type Source string

const (
	// SourcePolicy is the operator policy's harness.<name>.path.
	SourcePolicy Source = "policy"
	// SourcePath is the process's PATH.
	SourcePath Source = "PATH"
	// SourceKnown is one of the adapter's well-known install locations
	// (agentry-cli#167) — the native installer's directory, or a desktop
	// app's bundled build — which a PATH-less GUI launch never sees.
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
// regular file. A template whose environment variable is unset, or whose
// home directory cannot be found, is skipped rather than resolved to a
// wrong place.
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

// expand resolves "~" and "${VAR}" in a template into a path for this
// platform; false when something it names does not exist.
func expand(tpl string) (string, bool) {
	if strings.HasPrefix(tpl, "~") {
		home, err := homeDir()
		if err != nil || home == "" {
			return "", false
		}
		tpl = home + tpl[1:]
	}
	unset := false
	tpl = os.Expand(tpl, func(name string) string {
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			unset = true
		}
		return v
	})
	if unset {
		return "", false
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
	best, bestVer := "", []int(nil)
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if !isFile(p) {
			continue
		}
		v := versionOf(e.Name())
		if v == nil {
			continue
		}
		if best == "" || compareVersions(v, bestVer) > 0 {
			best, bestVer = p, v
		}
	}
	return best, best != ""
}

// versionOf reads the leading dotted number of a file name ("2.1.275",
// "2.1.275.exe" → [2 1 275]); nil when the name does not start with one.
// Only an executable's extension is stripped: filepath.Ext would take
// ".275" off "2.1.275" and make every build the same version.
func versionOf(name string) []int {
	for _, ext := range []string{".exe", ".cmd", ".bat"} {
		if strings.HasSuffix(strings.ToLower(name), ext) {
			name = name[:len(name)-len(ext)]
			break
		}
	}
	var v []int
	for _, part := range strings.Split(name, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			break
		}
		v = append(v, n)
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
