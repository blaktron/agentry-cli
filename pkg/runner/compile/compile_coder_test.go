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
