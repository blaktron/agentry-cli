// `agentry profiles` (RUNNER-PLAN-2 §P2): listing shows keep/remove intent
// and secret NAMES only (values never print anywhere), a broken profile is
// shown rather than hidden, and remove selects by ref, ref@version, or
// --all — exactly one selector.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/profile"
)

// seedProfile stores one profile under the runner home for list/remove
// tests, returning its directory.
func seedProfile(t *testing.T, home, ref, version, digest12 string, keep bool, secrets map[string]string) string {
	t.Helper()
	base, _, _ := strings.Cut(ref, "@")
	ns, name, _ := strings.Cut(base, "/")
	dir := profile.Dir(home, ns, name, version, digest12)
	p := profile.New(ref, "sha256:"+digest12, keep, secrets)
	if err := p.Save(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestProfilesListEmpty(t *testing.T) {
	isolateEnv(t)
	isolateRunnerHome(t)
	stdout, _, err := runCLI(t, nil, "profiles", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "no MCP profiles stored") {
		t.Errorf("empty list = %q", stdout)
	}
}

func TestProfilesListShowsNamesNeverValues(t *testing.T) {
	isolateEnv(t)
	home, _ := isolateRunnerHome(t)
	seedProfile(t, home, "acme/demo@1.0.0", "1.0.0", "aaaaaaaaaaaa", true,
		map[string]string{"API_TOKEN": "sekret-value"})
	seedProfile(t, home, "acme/other@2.0.0", "2.0.0", "bbbbbbbbbbbb", false, nil)

	// A profile that no longer parses is surfaced, not silently skipped.
	brokenDir := profile.Dir(home, "acme", "broken", "3.0.0", "cccccccccccc")
	if err := os.MkdirAll(brokenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brokenDir, "profile.json"), []byte(`{"profileVersion":"99"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runCLI(t, nil, "profiles", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"acme/demo@1.0.0", "digest aaaaaaaaaaaa", "keep", "secrets: API_TOKEN", "last used: never used",
		"acme/other@2.0.0", "remove-on-completion", "secrets: none",
		"UNREADABLE  " + brokenDir,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("list lacks %q:\n%s", want, stdout)
		}
	}
	// The §P2 invariant: values never reach any output surface.
	if strings.Contains(stdout, "sekret-value") {
		t.Error("a secret VALUE was printed by profiles list")
	}
}

func TestProfilesRemoveSelectors(t *testing.T) {
	isolateEnv(t)
	home, _ := isolateRunnerHome(t)
	d1 := seedProfile(t, home, "acme/demo@1.0.0", "1.0.0", "aaaaaaaaaaaa", true, nil)
	d2 := seedProfile(t, home, "acme/demo@2.0.0", "2.0.0", "bbbbbbbbbbbb", true, nil)
	d3 := seedProfile(t, home, "acme/other@1.0.0", "1.0.0", "cccccccccccc", true, nil)

	// ref@version removes exactly that signature.
	stdout, _, err := runCLI(t, nil, "profiles", "remove", "acme/demo@2.0.0")
	if err != nil || !strings.Contains(stdout, "removed profile acme/demo@2.0.0") {
		t.Fatalf("versioned remove: %v\n%s", err, stdout)
	}
	if _, err := os.Stat(d2); !os.IsNotExist(err) {
		t.Error("versioned remove left the profile")
	}
	if _, err := os.Stat(d1); err != nil {
		t.Error("versioned remove took an unrelated version")
	}

	// Bare ns/name removes every version of that agent, nothing else.
	if _, _, err := runCLI(t, nil, "profiles", "remove", "acme/demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d1); !os.IsNotExist(err) {
		t.Error("ns/name remove missed a version")
	}
	if _, err := os.Stat(d3); err != nil {
		t.Error("ns/name remove took another agent's profile")
	}

	// Nothing matched is a statement, not a silent success.
	stdout, _, err = runCLI(t, nil, "profiles", "remove", "acme/demo")
	if err != nil || !strings.Contains(stdout, "nothing matched") {
		t.Errorf("no-match remove: %v\n%s", err, stdout)
	}

	// --all sweeps the store.
	if _, _, err := runCLI(t, nil, "profiles", "remove", "--all"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d3); !os.IsNotExist(err) {
		t.Error("--all left a profile behind")
	}

	// Exactly one selector: neither, or both, is a usage error.
	if _, _, err := runCLI(t, nil, "profiles", "remove"); err == nil ||
		!strings.Contains(err.Error(), "exactly one of") {
		t.Errorf("selector-less remove: %v", err)
	}
	if _, _, err := runCLI(t, nil, "profiles", "remove", "acme/demo", "--all"); err == nil ||
		!strings.Contains(err.Error(), "exactly one of") {
		t.Errorf("double-selector remove: %v", err)
	}
}

func TestShortDigestSuffixOf(t *testing.T) {
	tests := map[string]string{
		"/home/x/profiles/acme/demo/1.0.0-abc123def456": "abc123def456",
		"1.0.0-abc": "abc",
		"nodash":    "nodash",
		"trail-":    "trail-", // nothing after the dash: shown whole
	}
	for dir, want := range tests {
		if got := shortDigestSuffixOf(dir); got != want {
			t.Errorf("shortDigestSuffixOf(%q) = %q, want %q", dir, got, want)
		}
	}
}
