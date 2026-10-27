package compile

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

func TestAliasURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:11434/v1":    "http://agentry-host.internal:11434/v1",
		"http://LOCALHOST/v1":          "http://agentry-host.internal/v1",
		"http://127.0.0.1:1234/v1":     "http://agentry-host.internal:1234/v1",
		"http://127.1.2.3:8000":        "http://agentry-host.internal:8000",
		"http://[::1]:8080/v1":         "http://agentry-host.internal:8080/v1",
		"https://localhost:8443/v1":    "https://agentry-host.internal:8443/v1",
		"http://studio.local:8000/v1":  "http://studio.local:8000/v1",
		"http://192.168.1.20:8000/v1":  "http://192.168.1.20:8000/v1",
		"http://my.localhost:8000/v1":  "http://my.localhost:8000/v1",
		"https://api.anthropic.com/v1": "https://api.anthropic.com/v1",
		"":                             "",
	} {
		if got, _ := aliasURL(in); got != want {
			t.Errorf("aliasURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAliasEntry(t *testing.T) {
	for in, want := range map[string]string{
		"localhost:11434": "agentry-host.internal:11434",
		"127.0.0.1:1234":  "agentry-host.internal:1234",
		"[::1]:8080":      "agentry-host.internal:8080",
		"localhost":       "agentry-host.internal",
		"studio.local:80": "studio.local:80",
		"api.example.com": "api.example.com",
	} {
		if got, _ := aliasEntry(in); got != want {
			t.Errorf("aliasEntry(%q) = %q, want %q", in, got, want)
		}
	}
}

// A policy with no loopback endpoint is passed through untouched; one with
// one is copied, and the caller's policy is left as it was.
func TestWithLoopbackAliasCopies(t *testing.T) {
	pol := testPolicy()
	pol.Model.BaseURL = "https://api.example.com/v1"
	pol.Model.ProxyAllow = []string{"api.example.com:443"}
	if got := withLoopbackAlias(pol); got != pol {
		t.Error("a policy without a loopback endpoint was copied")
	}
	pol.Model.BaseURL = "http://localhost:11434/v1"
	pol.Model.ProxyAllow = []string{"localhost:11434", "api.example.com:443"}
	pol.Harness = map[string]policy.HarnessPolicy{"codex": {BaseURL: "http://127.0.0.1:1234/v1"}}
	got := withLoopbackAlias(pol)
	if got.Model.BaseURL != "http://agentry-host.internal:11434/v1" || got.Harness["codex"].BaseURL != "http://agentry-host.internal:1234/v1" {
		t.Errorf("aliased endpoints = %q, %q", got.Model.BaseURL, got.Harness["codex"].BaseURL)
	}
	if want := []string{"agentry-host.internal:11434", "api.example.com:443"}; !slices.Equal(got.Model.ProxyAllow, want) {
		t.Errorf("proxyAllow = %q, want %q", got.Model.ProxyAllow, want)
	}
	if pol.Model.BaseURL != "http://localhost:11434/v1" || pol.Model.ProxyAllow[0] != "localhost:11434" || pol.Harness["codex"].BaseURL != "http://127.0.0.1:1234/v1" {
		t.Error("the caller's policy was changed")
	}
}

// The compiled plan gives the harness and the proxy the alias, never the
// loopback name, which the harness would dial in its own container.
func TestCompileAliasesALoopbackEndpoint(t *testing.T) {
	pol := testPolicy()
	pol.Model.Provider = "custom"
	pol.Model.Name = "stand-in"
	pol.Model.APIKeyEnv = "LOCAL_KEY"
	pol.Model.BaseURL = "http://localhost:11434/v1"
	pol.Model.ProxyAllow = []string{"localhost:11434"}
	m := parseManifest(t, "schemaVersion: \"2\"\nname: pure\nnamespace: demo\nmodes: [autonomous]\nversion: 1.0.0\nsummary: \"p\"\npublish:\n  include: [agent.yaml, AGENT.md]\n")
	dir := t.TempDir()
	plan, err := Compile(Inputs{Manifest: m, Digest: "sha256:0123456789abcdef", Source: "local", WorkspaceDir: workspaceFor(t, m, nil),
		Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"agentry-host.internal:11434"}; !slices.Equal(plan.Proxy.Allow, want) || !slices.Equal(plan.Proxy.PlainHTTP, want) {
		t.Errorf("proxy allow %q, plain %q, want %q", plan.Proxy.Allow, plan.Proxy.PlainHTTP, want)
	}
	var everything strings.Builder
	for _, v := range plan.Files {
		everything.WriteString(v)
	}
	for _, e := range plan.Harness.Agent.Env {
		everything.WriteString(e.Value + "\n")
	}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(path) //nolint:gosec // the test's own temp dir
			everything.Write(b)
		}
		return nil
	})
	if s := everything.String(); !strings.Contains(s, "http://agentry-host.internal:11434/v1") || strings.Contains(s, "//localhost:11434") {
		t.Errorf("the harness's endpoint is not the alias:\n%s", s)
	}
	if pol.Model.BaseURL != "http://localhost:11434/v1" {
		t.Error("Compile changed the caller's policy")
	}
}

// A harness that runs on this computer reaches its loopback itself, so its
// plan keeps the endpoint as the policy names it.
func TestCompileKeepsLoopbackForAHarnessOnThisComputer(t *testing.T) {
	pol := testPolicy()
	pol.Model = policy.Model{Provider: "openai", Name: "local", APIKeyEnv: "LOCAL_KEY", BaseURL: "http://localhost:1234/v1", ProxyAllow: []string{"localhost:1234"}}
	m := parseManifest(t, pureTextYAML)
	plan, err := Compile(Inputs{Manifest: m, Digest: "sha256:0123456789abcdef", Source: "local", WorkspaceDir: workspaceFor(t, m, nil),
		Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test", AdapterName: adapter.CodexAdapterName}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Harness.Host == nil {
		t.Fatal("the codex adapter did not run on the host")
	}
	if want := []string{"localhost:1234"}; !slices.Equal(plan.Proxy.Allow, want) {
		t.Errorf("proxy allow = %q, want %q", plan.Proxy.Allow, want)
	}
}
