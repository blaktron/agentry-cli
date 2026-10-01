package compile

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry/pkg/manifest"
)

// compileReview compiles webTextAgent's contract (no egress, exec, write,
// secret or server) with the review grant asked for, and the web tools if
// web.
func compileReview(t *testing.T, yaml, source string, review, web bool) (*spec.Plan, string, error) {
	t.Helper()
	m, err := manifest.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "AGENT.md"), []byte("Review.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.AllowWildcardEgress = true
	pol.Exec.Packages = map[string]string{"git": "git"}
	dir := filepath.Join(t.TempDir(), "plan")
	plan, err := Compile(Inputs{Manifest: m, Digest: "sha256:x", Source: source, Review: review, Web: web, WorkspaceDir: ws,
		Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test"}, dir)
	return plan, dir, err
}

// TestCompileRefusesTheReviewGrantBeyondABuiltin (agentry-cli#503): the
// review grant is a built-in's alone, never beside the web tools, and never
// to a built-in whose contract would run beside the person's key.
func TestCompileRefusesTheReviewGrantBeyondABuiltin(t *testing.T) {
	for _, source := range []string{"registry", "local", ""} {
		_, _, err := compileReview(t, webTextAgent, source, true, false)
		var refusal *PolicyRefusal
		if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "built-in's alone") {
			t.Fatalf("source %q: %v", source, err)
		}
	}
	_, _, err := compileReview(t, webTextAgent, "builtin", true, true)
	var refusal *PolicyRefusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "beside the web tools") {
		t.Fatalf("beside the web tools: %v", err)
	}
	for name, widen := range map[string]string{
		"egress":  "  network:\n    egress: [api.example.com]\n",
		"any":     "  network:\n    egress: \"*\"\n",
		"exec":    "  exec:\n    commands: [git]\n",
		"write":   "  filesystem:\n    write: workspace\n",
		"secrets": "  secrets: [API_KEY]\n",
		"servers": "  mcpServers:\n    - npm: \"@acme/mcp\"\n      version: 1.0.0\n",
	} {
		y := strings.Replace(webTextAgent, "capabilities:\n  network:\n    egress: none\n  exec: none\n", "capabilities:\n"+widen, 1)
		_, _, err := compileReview(t, y, "builtin", true, false)
		var refusal *PolicyRefusal
		if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "declares no egress") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// TestTheReviewGrantCompilesItsContainer: the grant gives the tools
// container TypeSafe's API as its one host, names the review key as the
// tools server's secret, grants ai_review, and says so in the plan's
// enforcement; without it the same package's container has no network.
func TestTheReviewGrantCompilesItsContainer(t *testing.T) {
	plan, dir, err := compileReview(t, webTextAgent, "builtin", true, false)
	if err != nil {
		t.Fatal(err)
	}
	catalog, _ := os.ReadFile(filepath.Join(dir, "gateway", "catalog.yaml"))
	if !strings.Contains(string(catalog), "allowHosts:\n") || !strings.Contains(string(catalog), toolsrv.ReviewHost+":443") ||
		strings.Count(string(catalog), ":443") != 1 || strings.Contains(string(catalog), "disableNetwork") {
		t.Fatalf("the review's container:\n%s", catalog)
	}
	if !strings.Contains(string(catalog), "env: "+toolsrv.EnvReviewJevKey) || !strings.Contains(string(catalog), SecretKey(toolsrv.EnvReviewJevKey)) ||
		strings.Contains(string(catalog), "AGENTRY_WEB") {
		t.Fatalf("the review's key:\n%s", catalog)
	}
	if !plan.Capabilities.ReviewTools || plan.Capabilities.WebTools || !slices.Contains(plan.Tools, toolsrv.ToolAIReview) ||
		slices.Contains(plan.Tools, toolsrv.ToolFetch) || slices.Contains(plan.Tools, toolsrv.ToolWebRead) {
		t.Fatalf("caps %+v tools %v", plan.Capabilities, plan.Tools)
	}
	if !strings.Contains(plan.Enforcement["network"], toolsrv.ReviewHost+":443 alone") || !strings.Contains(plan.Enforcement["review"], "never the package's text") {
		t.Fatalf("%v", plan.Enforcement)
	}
	if len(plan.Secrets) != 0 {
		t.Fatalf("the review key is not a declared secret: %v", plan.Secrets)
	}
	toolspec, _ := os.ReadFile(filepath.Join(dir, "gateway", "toolspec.json"))
	if !strings.Contains(string(toolspec), `"review": {`) {
		t.Fatalf("toolspec:\n%s", toolspec)
	}

	plain, dir, err := compileReview(t, webTextAgent, "builtin", false, false)
	if err != nil {
		t.Fatal(err)
	}
	catalog, _ = os.ReadFile(filepath.Join(dir, "gateway", "catalog.yaml"))
	if !strings.Contains(string(catalog), "disableNetwork: true") || strings.Contains(string(catalog), "AGENTRY_REVIEW") ||
		plain.Capabilities.ReviewTools || slices.Contains(plain.Tools, toolsrv.ToolAIReview) || plain.Enforcement["review"] != "" {
		t.Fatalf("without the grant:\n%s\n%+v", catalog, plain.Capabilities)
	}
}
