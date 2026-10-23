package compile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"
	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/runner/browser"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// coderInputs compiles the pure-text release as the Coding agent, with its
// folders and the grant g.
func coderInputs(t *testing.T, g spec.CoderGrant) Inputs {
	t.Helper()
	in := persistentInputs(t, &spec.AgentFiles{Share: true})
	in.Manifest = parseManifest(t, coderYAML(""))
	in.Coder = &g
	return in
}

// coderYAML is the pure-text release as agentry/coder, persistent, with caps
// as its capabilities block.
func coderYAML(caps string) string {
	y := strings.Replace(strings.Replace(pureTextYAML, "modes: [autonomous]", "modes: [persistent]", 1), "name: pure-text\nnamespace: demo", "name: coder\nnamespace: agentry", 1)
	if caps != "" {
		y = strings.Replace(y, "publish:", "capabilities:\n"+caps+"publish:", 1)
	}
	return y
}

// compiledCoder compiles in and reads back its tools spec and catalogue.
func compiledCoder(t *testing.T, in Inputs) (*spec.Plan, toolsrv.Spec, map[string]map[string]any) {
	t.Helper()
	planDir := t.TempDir()
	plan, err := Compile(in, planDir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(planDir, "gateway", "toolspec.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ts toolsrv.Spec
	if err := json.Unmarshal(b, &ts); err != nil {
		t.Fatal(err)
	}
	cat, err := os.ReadFile(filepath.Join(planDir, "gateway", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Registry map[string]map[string]any `yaml:"registry"`
	}
	if err := yaml.Unmarshal(cat, &doc); err != nil {
		t.Fatal(err)
	}
	return plan, ts, doc.Registry
}

// TestTheCodingAgentsGrantReachesEveryGate: each part of the Coding agent's
// grant, on, reaches the tools spec, the catalogue, the plan's images and
// servers and the receipt's lines together, with no machine policy key; off,
// none of them.
func TestTheCodingAgentsGrantReachesEveryGate(t *testing.T) {
	in := coderInputs(t, spec.CoderGrant{Commands: true, OpenEgress: true, Browser: true, DevHost: true})
	if in.Policy.AllowWildcardEgress || in.Policy.Browser.AllowPublicWeb {
		t.Fatal("the test policy allows what the grant must give on its own")
	}
	plan, ts, reg := compiledCoder(t, in)

	// Programs: any, in the Coding agent's exec image on its pinned toolchain.
	if !ts.Exec.Any || !ts.Exec.ImagePath || ts.Exec.Home != spec.CoderHome || len(ts.Exec.Commands) != 0 || ts.Limits.ExecTimeoutSeconds != coderExecTimeoutSeconds {
		t.Errorf("tools spec exec: %+v, limits %+v", ts.Exec, ts.Limits)
	}
	if !slices.Contains(plan.Tools, toolsrv.ToolRunCommand) {
		t.Errorf("run_command is not granted: %v", plan.Tools)
	}
	base := policy.DefaultCoderImage
	if img := plan.ExecImage; img == nil || img.Coder != base || img.Tag != spec.CoderExecImageTag("agentry-runner:test", base) || reg[spec.ToolServerName]["image"] != img.Tag {
		t.Errorf("exec image %+v, tools server image %v", plan.ExecImage, reg[spec.ToolServerName]["image"])
	}
	if line := plan.Enforcement["exec"]; !strings.Contains(line, "any program") || !strings.Contains(line, base) || !strings.Contains(line, "agreed") {
		t.Errorf("exec line: %q", line)
	}

	// Open egress: the tools server unrestricted, disclosed as the agent's.
	tools := reg[spec.ToolServerName]
	if plan.Capabilities.EgressMode != "all" || !ts.Fetch.AllowAll || tools["disableNetwork"] != nil || tools["allowHosts"] != nil || !plan.Capabilities.ToolsReachPastHost() {
		t.Errorf("egress %q, fetch %+v, tools entry %v", plan.Capabilities.EgressMode, ts.Fetch, tools)
	}
	if line := plan.Enforcement["network"]; !strings.Contains(line, "the Coding agent's open egress") || strings.Contains(line, "permitted by operator policy") {
		t.Errorf("network line: %q", line)
	}

	// The browser on any public site, agreed for this agent.
	if b := plan.Capabilities.Browser; b == nil || !b.Public || !b.Agreed || plan.Browser == nil || reg[browser.ServerName] == nil {
		t.Errorf("browser cap %+v, spec %+v", plan.Capabilities.Browser, plan.Browser)
	}
	if line := plan.Enforcement["browser"]; !strings.Contains(line, "agreed to for this agent") || strings.Contains(line, "allowPublicWeb") {
		t.Errorf("browser line: %q", line)
	}

	// The dev server, through the host link.
	if !slices.Equal(plan.Capabilities.TargetHosts, []string{spec.DevHostID}) || plan.HostLink == nil || reg[hostlink.ServerName] == nil {
		t.Errorf("target hosts %v, host link %+v", plan.Capabilities.TargetHosts, plan.HostLink)
	}
	if line := plan.Enforcement["targetHosts"]; !strings.Contains(line, "dev server") || !strings.Contains(line, "pinned") {
		t.Errorf("targetHosts line: %q", line)
	}

	// Off, nothing of it.
	plan, ts, reg = compiledCoder(t, coderInputs(t, spec.CoderGrant{}))
	if ts.Exec.Any || slices.Contains(plan.Tools, toolsrv.ToolRunCommand) || plan.ExecImage != nil || plan.Capabilities.EgressMode != "none" ||
		reg[spec.ToolServerName]["disableNetwork"] != true || plan.Capabilities.Browser != nil || len(plan.Capabilities.TargetHosts) != 0 || plan.HostLink != nil {
		t.Errorf("all off: exec %+v, tools %v, egress %q, browser %+v, hosts %v", ts.Exec, plan.Tools, plan.Capabilities.EgressMode, plan.Capabilities.Browser, plan.Capabilities.TargetHosts)
	}
	if !strings.Contains(plan.Enforcement["exec"], "no exec") {
		t.Errorf("all off, exec line: %q", plan.Enforcement["exec"])
	}
}

// TestTheCodingAgentsGrantIsAPersistentBuiltinsAlone: the grant is refused
// to any source but a built-in, to a plan that is not persistent, without
// the agent's folders, beside the web tools or the review grant, and beside
// a manifest that declares egress of its own.
func TestTheCodingAgentsGrantIsAPersistentBuiltinsAlone(t *testing.T) {
	all := spec.CoderGrant{Commands: true, OpenEgress: true, Browser: true, DevHost: true}
	refused := func(name string, in Inputs, why string) {
		t.Helper()
		var refusal *PolicyRefusal
		if _, err := Compile(in, t.TempDir()); !errors.As(err, &refusal) || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: err = %v, want a policy refusal naming %q", name, err, why)
		}
	}
	for _, source := range []string{"registry", "local"} {
		in := coderInputs(t, all)
		in.Source = source
		refused("from "+source, in, PersistentOnlyBuiltin)
	}
	auto := coderInputs(t, all)
	auto.Manifest = parseManifest(t, strings.Replace(coderYAML(""), "modes: [persistent]", "modes: [autonomous, persistent]", 1))
	auto.Mode, auto.AgentFiles = manifest.ModeAutonomous, nil
	refused("on an autonomous plan", auto, "persistent built-in's alone")
	other := coderInputs(t, all)
	other.Manifest = parseManifest(t, strings.Replace(coderYAML(""), "name: coder", "name: assistant", 1))
	refused("for another built-in", other, "agentry/coder's alone")
	noFiles := coderInputs(t, all)
	noFiles.AgentFiles = nil
	refused("without its folders", noFiles, "with its folders")
	web := coderInputs(t, all)
	web.Web = true
	refused("beside the web tools", web, "beside the web tools")
	review := coderInputs(t, all)
	review.Review = true
	refused("beside the review grant", review, "beside the web tools or the review grant")
	for name, caps := range map[string]string{
		"egress":      "  network:\n    egress: [example.com]\n",
		"exec":        "  exec:\n    commands: [git]\n",
		"a browser":   "  browser:\n    scope: public\n",
		"target host": "  targetHosts: [server]\n",
	} {
		in := coderInputs(t, all)
		in.Manifest = parseManifest(t, coderYAML(caps))
		refused("beside a manifest's own "+name, in, "replace what a manifest declares")
	}
	unpinned := coderInputs(t, all)
	pol := *unpinned.Policy
	pol.Images.Coder = "ghcr.io/blaktron/agentry-coder:latest"
	unpinned.Policy = &pol
	refused("on a toolchain image not pinned by digest", unpinned, "pinned by digest")
	gvisor := coderInputs(t, all)
	pol2 := *gvisor.Policy
	pol2.Docker.Runtime, pol2.Docker.Resolvers = "runsc", nil
	gvisor.Policy = &pol2
	refused("open egress under a runtime with no resolvers", gvisor, "docker.resolvers")
}

// TestOpenEgressOutsideTheCodingAgentStaysThePolicys: without the grant, a
// manifest's "*" is still refused under a policy that does not allow it, so
// the Coding agent's grant widens nothing else.
func TestOpenEgressOutsideTheCodingAgentStaysThePolicys(t *testing.T) {
	m := parseManifest(t, strings.Replace(pureTextYAML, "publish:", "capabilities:\n  network:\n    egress: [\"*\"]\npublish:", 1))
	var refusal *PolicyRefusal
	_, err := Compile(Inputs{Manifest: m, Digest: "sha256:x", Source: "builtin", WorkspaceDir: workspaceFor(t, m, nil),
		Policy: testPolicy(), PolicySHA: "s", RunnerImage: "agentry-runner:test"}, t.TempDir())
	if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "allowWildcardEgress") {
		t.Fatalf("a built-in's own \"*\" under the default policy: %v", err)
	}
}

// TestTheCodingAgentsPortsReachTheToolsSpecAndThePlan: published ports are
// named to the tools server and in the plan's lines while its programs run;
// without programs nothing serves them, so neither does either.
func TestTheCodingAgentsPortsReachTheToolsSpecAndThePlan(t *testing.T) {
	ports := []spec.CoderPort{{Name: "web", Port: 3000}, {Name: "api", Port: 8080}}
	plan, ts, _ := compiledCoder(t, coderInputs(t, spec.CoderGrant{Commands: true, Ports: ports}))
	if !slices.Equal(ts.Exec.Ports, []toolsrv.Port{{Name: "web", Port: 3000}, {Name: "api", Port: 8080}}) {
		t.Errorf("tools spec ports: %+v", ts.Exec.Ports)
	}
	if line := plan.Enforcement["persistent.ports"]; !strings.Contains(line, "web on 3000, api on 8080") || !strings.Contains(line, "127.0.0.1") {
		t.Errorf("ports line: %q", line)
	}
	if !slices.Contains(plan.Tools, toolsrv.ToolBackgroundOutput) || !slices.Contains(plan.Tools, toolsrv.ToolStopBackground) {
		t.Errorf("the background tools are not granted: %v", plan.Tools)
	}
	plan, ts, _ = compiledCoder(t, coderInputs(t, spec.CoderGrant{Ports: ports}))
	if len(ts.Exec.Ports) != 0 || plan.Enforcement["persistent.ports"] != "" {
		t.Errorf("without programs: ports %+v, line %q", ts.Exec.Ports, plan.Enforcement["persistent.ports"])
	}
	for name, bad := range map[string][]spec.CoderPort{
		"privileged":     {{Name: "web", Port: 22}},
		"a number twice": {{Name: "web", Port: 3000}, {Name: "api", Port: 3000}},
	} {
		if _, err := Compile(coderInputs(t, spec.CoderGrant{Commands: true, Ports: bad}), t.TempDir()); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}
}

// TestTheCodersStoredSecretsAreNamesItMayHave: a stored secret's name the
// person could not store (one the runner or its programs set, or not a
// variable's), one given twice, and one the manifest declares are refused
// at compile, whatever wrote the grant.
func TestTheCodersStoredSecretsAreNamesItMayHave(t *testing.T) {
	if err := checkCoderSecrets([]string{"GITHUB_TOKEN", "NPM_TOKEN"}, nil); err != nil {
		t.Fatalf("two plain names: %v", err)
	}
	for _, tc := range []struct {
		names, declared []string
	}{
		{[]string{"PATH"}, nil},
		{[]string{"HTTPS_PROXY"}, nil},
		{[]string{"LD_PRELOAD"}, nil},
		{[]string{"AGENTRY_WEB_JEV_KEY"}, nil},
		{[]string{"lower"}, nil},
		{[]string{"GITHUB_TOKEN", "GITHUB_TOKEN"}, nil},
		{[]string{"API_KEY"}, []string{"API_KEY"}},
	} {
		var pr *PolicyRefusal
		if err := checkCoderSecrets(tc.names, tc.declared); !errors.As(err, &pr) {
			t.Errorf("%v (declared %v): %v", tc.names, tc.declared, err)
		}
	}
}

// TestTheCodersAccountsReachTheToolsSpecAndThePlan: a git host account
// reaches the tools spec (as an account, and its token's variable as a
// masked secret), the catalogue's secrets, the plan's line and the System
// Prompt, by host and name; with commands off, or out of shape, it is
// refused at compile, whatever wrote the grant.
func TestTheCodersAccountsReachTheToolsSpecAndThePlan(t *testing.T) {
	acct := spec.CoderAccount{Host: "github.com", Kind: spec.GitHostGitHub, Own: true, User: "coder-bot", Name: "Coder Bot", Email: "bot@example.com", TokenEnv: spec.AccountTokenEnv(0)}
	plan, ts, reg := compiledCoder(t, coderInputs(t, spec.CoderGrant{Commands: true, Accounts: []spec.CoderAccount{acct}}))
	if len(ts.Exec.Accounts) != 1 || ts.Exec.Accounts[0].Host != "github.com" || ts.Exec.Accounts[0].TokenEnv != "AGENTRY_ACCOUNT_TOKEN_1" || !ts.Exec.Accounts[0].Own ||
		!slices.Contains(ts.Exec.SecretEnv, "AGENTRY_ACCOUNT_TOKEN_1") {
		t.Errorf("tools spec: %+v", ts.Exec)
	}
	if b, _ := json.Marshal(reg); !strings.Contains(string(b), "agentry-tools.agentry_account_token_1") {
		t.Errorf("the catalogue has no secret for the token: %s", b)
	}
	if line := plan.Enforcement["persistent.accounts"]; !strings.Contains(line, "github.com as coder-bot (the agent's own account, github)") || !strings.Contains(line, "to its own host alone") {
		t.Errorf("accounts line: %q", line)
	}
	if !strings.Contains(accountsPrompt(plan.Capabilities.Coder.Accounts), "ask them to add your account as a collaborator") {
		t.Errorf("the prompt: %q", accountsPrompt(plan.Capabilities.Coder.Accounts))
	}
	yours := acct
	yours.Own = false
	if p := accountsPrompt([]spec.CoderAccount{yours}); !strings.Contains(p, "the person's own account, so act only as they asked") || strings.Contains(p, "collaborator") {
		t.Errorf("the prompt for the person's account: %q", p)
	}
	for name, g := range map[string]spec.CoderGrant{
		"commands off":          {Accounts: []spec.CoderAccount{acct}},
		"a host with a path":    {Commands: true, Accounts: []spec.CoderAccount{{Host: "github.com/o", Kind: spec.GitHostGitHub, User: "u", Name: "U", Email: "u@example.com", TokenEnv: spec.AccountTokenEnv(0)}}},
		"a stored secret's var": {Commands: true, Accounts: []spec.CoderAccount{{Host: "github.com", Kind: spec.GitHostGitHub, User: "u", Name: "U", Email: "u@example.com", TokenEnv: "GITHUB_TOKEN"}}},
		"two on one host":       {Commands: true, Accounts: []spec.CoderAccount{acct, {Host: "github.com", Kind: spec.GitHostGitHub, User: "v", Name: "V", Email: "v@example.com", TokenEnv: spec.AccountTokenEnv(1)}}},
		"two GitLab hosts": {Commands: true, Accounts: []spec.CoderAccount{
			{Host: "gitlab.com", Kind: spec.GitHostGitLab, User: "u", Name: "U", Email: "u@example.com", TokenEnv: spec.AccountTokenEnv(0)},
			{Host: "gitlab.example.com", Kind: spec.GitHostGitLab, User: "u", Name: "U", Email: "u@example.com", TokenEnv: spec.AccountTokenEnv(1)}}},
	} {
		var pr *PolicyRefusal
		if _, err := Compile(coderInputs(t, g), t.TempDir()); !errors.As(err, &pr) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
