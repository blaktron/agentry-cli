package compile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry/pkg/manifest"
)

// compileRender compiles webTextAgent from source with the web tools (if
// web) and a rendered web_read asked for.
func compileRender(t *testing.T, source string, web bool) (*spec.Plan, string, error) {
	t.Helper()
	m, err := manifest.Parse([]byte(webTextAgent))
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "AGENT.md"), []byte("Build.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "plan")
	plan, err := Compile(Inputs{Manifest: m, Digest: "sha256:x", Source: source, Web: web, WebSearchedOnly: web, WebRender: true, WorkspaceDir: ws,
		Policy: policy.Default(), PolicySHA: "sha", RunnerImage: "agentry-runner:test"}, dir)
	return plan, dir, err
}

// TestARenderedReadIsABuiltinsWebGrant: a built-in with the web tools gets
// the renderer: the plan names its image (the browser image) and its line,
// the tools server its hand-off folder read-write and web_read's render
// argument, and the gateway may bind that one folder writable. Without the
// web tools, or for a release, it is refused; and no browser server is
// compiled, so the model is offered no browser tool.
func TestARenderedReadIsABuiltinsWebGrant(t *testing.T) {
	plan, dir, err := compileRender(t, "builtin", true)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Capabilities.WebRender || plan.Render == nil || plan.Render.Image != policy.Default().Images.BrowserImage() {
		t.Fatalf("render %v %+v", plan.Capabilities.WebRender, plan.Render)
	}
	if plan.Browser != nil || plan.Capabilities.Browser != nil {
		t.Fatal("a rendered read compiled a browser server")
	}
	for _, tool := range plan.Tools {
		if strings.HasPrefix(tool, "browser_") {
			t.Fatalf("the model is offered %s", tool)
		}
	}
	if !strings.Contains(plan.Enforcement["web.render"], "the model never drives the browser") {
		t.Fatalf("enforcement %q", plan.Enforcement["web.render"])
	}
	raw, err := os.ReadFile(filepath.Join(dir, "gateway", "toolspec.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ts toolsrv.Spec
	if err := json.Unmarshal(raw, &ts); err != nil {
		t.Fatal(err)
	}
	if ts.Web.Render != spec.RenderMount || !ts.Web.SearchedOnly {
		t.Fatalf("tools spec web %+v", ts.Web)
	}
	catalog, err := os.ReadFile(filepath.Join(dir, "gateway", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(catalog), spec.PHCfg+"/"+spec.RenderDir+":"+spec.RenderMount+":rw") {
		t.Fatal("the tools server is not given the render folder")
	}
	var writable string
	for _, e := range plan.Gateway.Env {
		if e.Name == "MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS" {
			writable = e.Value
		}
	}
	if !strings.Contains(writable, spec.PHCfg+"/"+spec.RenderDir) || strings.Contains(writable, spec.PHCfg+":") {
		t.Fatalf("writable binds %q", writable)
	}

	var refusal *PolicyRefusal
	if _, _, err := compileRender(t, "builtin", false); !errors.As(err, &refusal) {
		t.Fatalf("a render without the web tools: %v", err)
	}
	for _, source := range []string{"registry", "local"} {
		if _, _, err := compileRender(t, source, true); !errors.As(err, &refusal) {
			t.Fatalf("a %s release with a rendered read: %v", source, err)
		}
	}
}

// TestAPlanWithoutTheRendererHasNone: the web tools alone render nothing.
func TestAPlanWithoutTheRendererHasNone(t *testing.T) {
	plan, err := compileWebGrant(t, true, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Capabilities.WebRender || plan.Render != nil || plan.Enforcement["web.render"] != "" {
		t.Fatalf("render %v %+v %q", plan.Capabilities.WebRender, plan.Render, plan.Enforcement["web.render"])
	}
}
