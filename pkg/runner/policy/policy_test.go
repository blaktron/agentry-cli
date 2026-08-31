// Policy tests: the generated default file must parse back to exactly
// Default() (what first use writes is what runs), and the validator must
// name its refusals.
package policy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultFileMatchesDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	pol, sha, err := EnsureDefault(path)
	if err != nil {
		t.Fatalf("EnsureDefault: %v", err)
	}
	if sha == "" {
		t.Fatal("no policy hash returned")
	}
	if !reflect.DeepEqual(pol, Default()) {
		t.Fatalf("generated default file parses to %+v, want Default() %+v", pol, Default())
	}
	// Second load reuses the file without rewriting it.
	before, _ := os.ReadFile(path)
	if _, _, err := EnsureDefault(path); err != nil {
		t.Fatalf("reload: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("EnsureDefault rewrote an existing policy file")
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	bad := strings.Replace(defaultFileText, "allowWildcardEgress: false",
		"allowWildcardEgres: true", 1) // typo must not silently disable the gate
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil {
		t.Fatal("typo'd policy key accepted")
	}
}

func TestValidateTable(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Policy)
		wantErr string
	}{
		{"missing api key env", func(p *Policy) { p.Model.APIKeyEnv = "" }, "apiKeyEnv"},
		{"empty proxy allow", func(p *Policy) { p.Model.ProxyAllow = nil }, "proxyAllow"},
		{"bad proxy entry", func(p *Policy) { p.Model.ProxyAllow = []string{"http://x"} }, "proxyAllow"},
		{"bad secret mode", func(p *Policy) { p.SecretSource.Mode = "vault" }, "secretSource.mode"},
		{"envFile without path", func(p *Policy) { p.SecretSource = SecretSource{Mode: "envFile"} }, "envFile"},
		{"env with stray path", func(p *Policy) { p.SecretSource = SecretSource{Mode: "env", EnvFile: "x"} }, "envFile"},
		{"zero timeout", func(p *Policy) { p.Budgets.TimeoutSeconds = 0 }, "timeoutSeconds"},
		{"negative turns", func(p *Policy) { p.Budgets.MaxTurns = -1 }, "maxTurns"},
		{"missing image", func(p *Policy) { p.Images.Gateway = "" }, "images"},
		{"wrong version", func(p *Policy) { p.PolicyVersion = "2" }, "policyVersion"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := Default()
			tc.mutate(p)
			err := p.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want mention of %q", err, tc.wantErr)
			}
		})
	}
}

func TestSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep", "policy.yaml") // Save must create the directory
	p := Default()
	p.Model.Provider, p.Model.Name = "openai", "gpt-5.2"
	p.DataAccessServers = map[string][]string{"tickets": {"/etc/agentry/jira.yaml"}}
	p.Profiles.KeepByDefault = boolPtr(false)
	if err := Save(path, p); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "# SecureAgentry Runner operator policy") ||
		!strings.Contains(string(raw), "comments are not preserved") {
		t.Errorf("generated header missing:\n%s", raw)
	}

	got, sha, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if sha == "" {
		t.Error("Load returned no policy hash")
	}
	if !reflect.DeepEqual(got, p) {
		t.Fatalf("round-trip mismatch:\ngot  %+v\nwant %+v", got, p)
	}
	// The Profiles pointer must survive as an explicit false, not revert to
	// the unset-means-true default.
	if got.Profiles.KeepByDefault == nil || *got.Profiles.KeepByDefault {
		t.Error("keepByDefault: false did not round-trip")
	}
}

func TestSaveRefusesInvalidPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	p := Default()
	p.Model.APIKeyEnv = ""
	if err := Save(path, p); err == nil || !strings.Contains(err.Error(), "apiKeyEnv") {
		t.Fatalf("Save of an invalid policy: err = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("invalid policy was still written to disk")
	}
}

func TestSaveCreateDirError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Save(filepath.Join(blocker, "sub", "policy.yaml"), Default())
	if err == nil || !strings.Contains(err.Error(), "create policy dir") {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".agentry", "runner", "policy.yaml"); got != want {
		t.Errorf("DefaultPath = %q, want %q", got, want)
	}

	t.Setenv("HOME", "")
	if _, err := DefaultPath(); err == nil {
		t.Fatal("DefaultPath with no HOME must error")
	}
}

func TestKeepProfilesByDefault(t *testing.T) {
	p := &Policy{}
	if !p.KeepProfilesByDefault() {
		t.Error("unset keepByDefault must read as true (P2 §P8)")
	}
	p.Profiles.KeepByDefault = boolPtr(true)
	if !p.KeepProfilesByDefault() {
		t.Error("explicit true read as false")
	}
	p.Profiles.KeepByDefault = boolPtr(false)
	if p.KeepProfilesByDefault() {
		t.Error("explicit false read as true")
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, _, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil || !os.IsNotExist(err) {
		t.Fatalf("err = %v, want the raw not-exist error for callers to branch on", err)
	}
}

func TestLoadRefusesTrailingDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	two := defaultFileText + "\n---\nallowWildcardEgress: true\n"
	if err := os.WriteFile(path, []byte(two), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "trailing YAML document") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureDefaultUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not refuse writes")
	}
	// The policy path's directory does not exist yet and cannot be created:
	// stat reports not-exist, MkdirAll is refused.
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	_, _, err := EnsureDefault(filepath.Join(parent, "runner", "policy.yaml"))
	if err == nil || !strings.Contains(err.Error(), "create policy dir") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureDefaultWriteError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not refuse writes")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	_, _, err := EnsureDefault(filepath.Join(dir, "policy.yaml"))
	if err == nil || !strings.Contains(err.Error(), "write default policy") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunnerImageTag(t *testing.T) {
	p := Default()
	if got := p.RunnerImageTag("1.2.3"); got != "agentry-runner:1.2.3" {
		t.Errorf("tag = %q", got)
	}
	if got := p.RunnerImageTag("1.2.3+dirty!"); got != "agentry-runner:dev" {
		t.Errorf("unsafe version should fall back to dev, got %q", got)
	}
	if got := p.RunnerImageTag(""); got != "agentry-runner:dev" {
		t.Errorf("empty version should fall back to dev, got %q", got)
	}
	p.Images.Runner = "custom:tag"
	if got := p.RunnerImageTag("1.2.3"); got != "custom:tag" {
		t.Errorf("override ignored, got %q", got)
	}
}
