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
