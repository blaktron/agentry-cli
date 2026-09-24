package version

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A stamped release names itself: its version is unique to the commit it
// was built from, so a plan it compiled is recognised by that alone.
func TestBuildIDOfAStampedReleaseIsItsVersion(t *testing.T) {
	exe := writeExe(t, "binary one")
	for _, v := range []string{"1.2.3", "v0.4.0-3-gabc1234"} {
		if got := buildID(v, func() (string, error) { return exe, nil }); got != v {
			t.Errorf("buildID(%q) = %q, want the version unchanged", v, got)
		}
	}
}

// An unstamped or dirty build carries the same version string across code
// changes (the hosted worker's CLI reported 0.0.0-dev for every build,
// agentry-cli#144), so its identity adds a digest of the binary: another
// binary is another build, the same binary is the same build.
func TestBuildIDOfAnUnstampedOrDirtyBuildNamesTheBinary(t *testing.T) {
	one, two := writeExe(t, "binary one"), writeExe(t, "binary two")
	for _, v := range []string{devVersion, "v0.4.0-3-gabc1234-dirty"} {
		a := buildID(v, func() (string, error) { return one, nil })
		again := buildID(v, func() (string, error) { return one, nil })
		b := buildID(v, func() (string, error) { return two, nil })
		if !strings.HasPrefix(a, v+"+exe.") {
			t.Errorf("buildID(%q) = %q, want %q+exe.<digest>", v, a, v)
		}
		if a != again {
			t.Errorf("the same binary gave two identities: %q, %q", a, again)
		}
		if a == b {
			t.Errorf("two different binaries share the identity %q", a)
		}
	}
}

// When the binary cannot be read the identity falls back to the version:
// the plan check then behaves as it did before, never worse.
func TestBuildIDFallsBackToTheVersion(t *testing.T) {
	if got := buildID(devVersion, func() (string, error) { return "", errors.New("no executable") }); got != devVersion {
		t.Errorf("unreadable executable: %q", got)
	}
	if got := buildID(devVersion, func() (string, error) { return filepath.Join(t.TempDir(), "gone"), nil }); got != devVersion {
		t.Errorf("missing executable: %q", got)
	}
}

func TestBuildIsStableWithinAProcess(t *testing.T) {
	first, second := Build(), Build()
	if first == "" || first != second {
		t.Fatalf("Build() = %q, then %q", first, second)
	}
}

func writeExe(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agentry")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil { //nolint:gosec // a test binary stand-in
		t.Fatal(err)
	}
	return p
}
