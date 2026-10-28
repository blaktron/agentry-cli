package compile

// The browser: a release that declares capabilities.browser compiles a
// browser server, a catalog entry of its own behind one relay to the run's
// browse proxy, whose run-time values are placeholders, and nothing else of
// the plan moves for it.

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/runner/browser"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

const browserManifest = `schemaVersion: "2"
name: site-tester
namespace: acme
modes: [autonomous]
version: 1.0.0
summary: "browser compile fixture"
intake:
  - id: site
    prompt: "Which site should I test?"
    type: url
    required: true
  - id: notes
    prompt: "A file to read"
    type: url
capabilities:
  browser:
    sites: [site]
    hosts: [fonts.googleapis.com, "*.Cdnhost.net"]
entrypoints:
  instruction: SKILL.md
publish:
  include: [agent.yaml, SKILL.md]
`

// withBrowserSecrets is browserManifest whose browser may type two
// declared secrets.
func withBrowserSecrets(doc string) string {
	doc = strings.Replace(doc, "    hosts: [fonts.googleapis.com, \"*.Cdnhost.net\"]\n",
		"    hosts: [fonts.googleapis.com, \"*.Cdnhost.net\"]\n    secrets: [SITE_USER, SITE_PASSWORD]\n  secrets: [SITE_USER, SITE_PASSWORD]\n", 1)
	return doc
}

func gatewayTools(t *testing.T, planDir string) map[string][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(planDir, "gateway", "tools.yaml")) //nolint:gosec // test plan dir
	if err != nil {
		t.Fatal(err)
	}
	var out map[string][]string
	if err := yaml.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCompileRendersTheBrowserServer(t *testing.T) {
	planDir := t.TempDir()
	plan, err := Compile(hostInputs(t, browserManifest), planDir)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Browser == nil || plan.Browser.Image != policy.DefaultBrowserImage {
		t.Fatalf("plan.Browser = %+v", plan.Browser)
	}
	if c := plan.Capabilities.Browser; c == nil || !slices.Equal(c.Sites, []string{"site"}) ||
		!slices.Equal(c.Hosts, []string{"fonts.googleapis.com", "*.cdnhost.net"}) || c.Public || len(c.Secrets) != 0 {
		t.Fatalf("capabilities.browser = %+v", c)
	}

	e := catalogServer(t, planDir, browser.ServerName)
	if e["type"] != "server" || e["image"] != policy.DefaultBrowserImage || e["user"] != spec.PHBrowserUser || e["longLived"] != true {
		t.Fatalf("the browser's entry: %v", e)
	}
	if got := strs(e["command"]); !slices.Equal(got, browser.Args(spec.PHBrowseProxy, false)) || slices.Contains(got, "--secrets") || slices.Contains(got, "--no-sandbox") {
		t.Fatalf("command = %v", got)
	}
	// One relay, to the run's browse proxy, and no other network (the half
	// of the network rule that is the plan's).
	if got := strs(e["allowHosts"]); !slices.Equal(got, []string{spec.PHBrowseProxy + ":3128/tcp"}) || e["disableNetwork"] != nil {
		t.Fatalf("allowHosts = %v", got)
	}
	// The output folder is its one writable bind, and no secret reaches it
	// through the gateway.
	wantVols := []string{
		spec.PHCfg + "/gateway/browser.json:" + browser.ConfigMount + ":ro",
		spec.PHCfg + "/browserout:" + browser.OutMount + ":rw",
	}
	if got := strs(e["volumes"]); !slices.Equal(got, wantVols) {
		t.Fatalf("volumes = %v, want %v", got, wantVols)
	}
	if e["secrets"] != nil || e["env"] != nil {
		t.Fatalf("the browser takes no gateway secret or env: %v", e)
	}
	if b, err := os.ReadFile(filepath.Join(planDir, "gateway", "browser.json")); err != nil || !strings.Contains(string(b), `"chromiumSandbox":true`) { //nolint:gosec // test plan dir
		t.Fatalf("browser.json %s, %v", b, err)
	}

	// The filter is an allowlist that never passes a tool running code in
	// the server or uploading a host file.
	tools := gatewayTools(t, planDir)[browser.ServerName]
	if !slices.Equal(tools, browser.Tools(false)) || !slices.Contains(tools, browser.ToolEvaluate) {
		t.Fatalf("tools.yaml browser = %v", tools)
	}
	for _, never := range []string{"browser_run_code_unsafe", "browser_file_upload", "browser_install", "browser_route", "browser_cookie_list"} {
		if slices.Contains(tools, never) {
			t.Fatalf("tools.yaml passes %s", never)
		}
	}

	// The tools server reads the browser's folder, read-only, and gains
	// no network.
	ts := catalogServer(t, planDir, spec.ToolServerName)
	if !slices.Contains(strs(ts["volumes"]), spec.PHCfg+"/browserout:/out/reports/browser:ro") || ts["disableNetwork"] != true {
		t.Fatalf("the tools server: %v", ts)
	}
	for _, env := range plan.Gateway.Env {
		if env.Name != "MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS" {
			continue
		}
		roots := strings.Split(env.Value, ":")
		if !slices.Contains(roots, spec.PHCfg+"/browserout") || slices.Contains(roots, spec.PHCfg+"/browser") {
			t.Fatalf("writable binds %q: the output folder, never the config and secrets folder", env.Value)
		}
	}
	if !slices.Contains(plan.Gateway.Servers, browser.ServerName) || !slices.Contains(plan.Gateway.Args, "--block-network") || !slices.Contains(plan.Gateway.Args, "--block-secrets") {
		t.Fatalf("gateway: %v %v", plan.Gateway.Servers, plan.Gateway.Args)
	}

	agent, _ := os.ReadFile(filepath.Join(planDir, "harness", "agent.yaml")) //nolint:gosec // test plan dir
	for _, want := range []string{"**Browser.**", "fonts.googleapis.com", `"site"`, "browser_network_requests", "/out/reports/browser/<name>", "never instructions"} {
		if !strings.Contains(string(agent), want) {
			t.Errorf("the System Prompt lacks %q", want)
		}
	}
	// The site is opened, never downloaded; the other url question still is.
	if strings.Contains(string(agent), `person named for "site" was downloaded`) || !strings.Contains(string(agent), `"notes"`) {
		t.Errorf("the System Prompt says the site is downloaded, or forgets the file question")
	}
	if line := plan.Enforcement["browser"]; !strings.Contains(line, "*.cdnhost.net") || !strings.Contains(line, "browse proxy") || strings.Contains(line, "--secrets") {
		t.Errorf("enforcement line %q", line)
	}
}

// TestABrowserThatTypesSecretsLosesEvaluate: the secrets reach the browser
// server alone, as a read-only file, and browser_evaluate is gone.
func TestABrowserThatTypesSecretsLosesEvaluate(t *testing.T) {
	planDir := t.TempDir()
	plan, err := Compile(hostInputs(t, withBrowserSecrets(browserManifest)), planDir)
	if err != nil {
		t.Fatal(err)
	}
	e := catalogServer(t, planDir, browser.ServerName)
	if got := strs(e["command"]); !slices.Equal(got, browser.Args(spec.PHBrowseProxy, true)) {
		t.Fatalf("command = %v", got)
	}
	if vols := strs(e["volumes"]); !slices.Contains(vols, spec.PHCfg+"/browser/secrets.env:"+browser.SecretsMount+":ro") {
		t.Fatalf("volumes = %v", vols)
	}
	tools := gatewayTools(t, planDir)[browser.ServerName]
	if slices.Contains(tools, browser.ToolEvaluate) || len(tools) != len(browser.Tools(false))-1 {
		t.Fatalf("tools.yaml browser = %v", tools)
	}
	agent, _ := os.ReadFile(filepath.Join(planDir, "harness", "agent.yaml")) //nolint:gosec // test plan dir
	if !strings.Contains(string(agent), "SITE_USER, SITE_PASSWORD") || !strings.Contains(string(agent), "which you never see") {
		t.Errorf("the System Prompt does not say how to type a secret")
	}
	if line := plan.Enforcement["browser"]; !strings.Contains(line, "SITE_USER, SITE_PASSWORD") || !strings.Contains(line, browser.ToolEvaluate+" is not granted") {
		t.Errorf("enforcement line %q", line)
	}
}

// TestPublicWebNeedsThePolicy: scope public is refused unless the
// operator's policy allows it.
func TestPublicWebNeedsThePolicy(t *testing.T) {
	doc := strings.Replace(browserManifest, "    sites: [site]\n", "    sites: [site]\n    scope: public\n", 1)
	var refusal *PolicyRefusal
	if _, err := Compile(hostInputs(t, doc), t.TempDir()); !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "browser.allowPublicWeb") {
		t.Fatalf("a public-web browser compiled without the policy: %v", err)
	}
	in := hostInputs(t, doc)
	in.Policy.Browser.AllowPublicWeb = true
	planDir := t.TempDir()
	plan, err := Compile(in, planDir)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Capabilities.Browser.Public || !strings.Contains(plan.Enforcement["browser"], "any public site") {
		t.Fatalf("plan: %+v %q", plan.Capabilities.Browser, plan.Enforcement["browser"])
	}
}

// TestAReleaseWithoutABrowserHasNoBrowserServer: no line, no browser
// server, no browse proxy, no browser tool.
func TestAReleaseWithoutABrowserHasNoBrowserServer(t *testing.T) {
	planDir := t.TempDir()
	plan, err := Compile(mcpFixtureInputs(t, nil, policy.Default()), planDir)
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := os.ReadFile(filepath.Join(planDir, "gateway", "catalog.yaml")) //nolint:gosec // test plan dir
	agent, _ := os.ReadFile(filepath.Join(planDir, "harness", "agent.yaml")) //nolint:gosec // test plan dir
	if plan.Browser != nil || plan.Capabilities.Browser != nil || strings.Contains(string(cat), "browser") ||
		strings.Contains(string(agent), "browser_") || plan.Enforcement["browser"] != "" {
		t.Fatal("a release with no browser compiled a browser server")
	}
	if _, err := os.Stat(filepath.Join(planDir, "gateway", "browser.json")); err == nil {
		t.Fatal("browser.json was written")
	}
}

func TestABuiltinIsNeverGrantedABrowser(t *testing.T) {
	in := hostInputs(t, browserManifest)
	in.Source = "builtin"
	var refusal *PolicyRefusal
	if _, err := Compile(in, t.TempDir()); !errors.As(err, &refusal) {
		t.Fatalf("a built-in with a browser compiled: %v", err)
	}
}

// TestTheColdCacheClearNamesTheBrowsersTools: a plan with a browser gives the
// harness's cold-cache clear the tools the gateway passes for it, so only the
// newest of their results stay whole; without browser_evaluate when the line
// types secrets.
func TestTheColdCacheClearNamesTheBrowsersTools(t *testing.T) {
	for _, c := range []struct {
		name     string
		manifest string
		secrets  bool
	}{
		{"browser", browserManifest, false},
		{"browser typing secrets", withBrowserSecrets(browserManifest), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			planDir := t.TempDir()
			if _, err := Compile(hostInputs(t, c.manifest), planDir); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(planDir, "harness", "agent.yaml")) //nolint:gosec // test plan dir
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Agents map[string]struct {
					ColdCacheClear struct {
						BrowserTools []string `yaml:"browser_tools"`
					} `yaml:"cold_cache_clear"`
				} `yaml:"agents"`
			}
			if err := yaml.Unmarshal(b, &doc); err != nil {
				t.Fatal(err)
			}
			got := doc.Agents["root"].ColdCacheClear.BrowserTools
			if want := browser.Tools(c.secrets); !slices.Equal(got, want) {
				t.Errorf("cold_cache_clear.browser_tools = %v, want %v", got, want)
			}
			if want := gatewayTools(t, planDir)[browser.ServerName]; !slices.Equal(got, want) {
				t.Errorf("cold_cache_clear.browser_tools = %v, the gateway passes %v", got, want)
			}
		})
	}
}
