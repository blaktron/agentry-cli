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

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// persistentInputs compiles the pure-text release as a persistent built-in
// with the agent's folders.
func persistentInputs(t *testing.T, files *spec.AgentFiles) Inputs {
	t.Helper()
	m := parseManifest(t, strings.Replace(pureTextYAML, "modes: [autonomous]", "modes: [persistent]", 1))
	return Inputs{
		Manifest: m, Mode: manifest.ModePersistent, Digest: "sha256:x", Source: "builtin", WorkspaceDir: workspaceFor(t, m, nil),
		Policy: testPolicy(), PolicySHA: "s", RunnerImage: "agentry-runner:test", AgentFiles: files,
	}
}

func gatewayEnv(p *spec.Plan, name string) string {
	for _, e := range p.Gateway.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// TestAgentFilesRootTheFilesystemTools: a persistent built-in's folders
// root fs_list, fs_read, fs_write, grep and glob at the agent's root, with
// its data volume always and the share folder only when it has it on; the
// share is a bind root and the gateway's one writable folder beside the
// output folder, and its verdicts are mounted only when it is checked.
func TestAgentFilesRootTheFilesystemTools(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files spec.AgentFiles
	}{
		{"data alone", spec.AgentFiles{}},
		{"the share, read as it is", spec.AgentFiles{Share: true}},
		{"the share, checked", spec.AgentFiles{Share: true, CheckShare: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			planDir := t.TempDir()
			plan, err := Compile(persistentInputs(t, &tc.files), planDir)
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
			folders := []string{"data"}
			if tc.files.Share {
				folders = append(folders, "share")
			}
			if ts.WorkspaceDir != spec.AgentRoot || !slices.Equal(ts.FSRead.Paths, folders) || !slices.Equal(ts.FSWrite.Paths, folders) || !ts.FSRead.Enabled || !ts.FSWrite.Enabled {
				t.Fatalf("tools spec: workspace %q, read %+v, write %+v", ts.WorkspaceDir, ts.FSRead, ts.FSWrite)
			}
			granted := ts.GrantedTools()
			for _, tool := range []string{toolsrv.ToolFSRead, toolsrv.ToolFSList, toolsrv.ToolFSWrite, toolsrv.ToolGrep, toolsrv.ToolGlob} {
				if !slices.Contains(granted, tool) || !slices.Contains(plan.Tools, tool) {
					t.Errorf("%s is not granted (tools spec %v)", tool, granted)
				}
			}
			catalog, err := os.ReadFile(filepath.Join(planDir, "gateway", "catalog.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			cat := string(catalog)
			if !strings.Contains(cat, spec.PHData+":/agent/data:rw") {
				t.Error("the data volume is not mounted read-write at /agent/data")
			}
			roots := strings.Split(gatewayEnv(plan, "MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS"), ":")
			writable := strings.Split(gatewayEnv(plan, "MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS"), ":")
			hasShare := strings.Contains(cat, spec.PHShare+":/agent/share:rw")
			if hasShare != tc.files.Share || slices.Contains(roots, spec.PHShare) != tc.files.Share || slices.Contains(writable, spec.PHShare) != tc.files.Share {
				t.Errorf("share mounted %v, a bind root %v, writable %v; want %v", hasShare, slices.Contains(roots, spec.PHShare), slices.Contains(writable, spec.PHShare), tc.files.Share)
			}
			if slices.Contains(writable, spec.PHWorkspace) || strings.Contains(cat, spec.PHWorkspace+":") {
				t.Error("a persistent agent's tools server is given the workspace")
			}
			checked := tc.files.Share && tc.files.CheckShare
			if (ts.ShareChecks != nil) != checked || strings.Contains(cat, spec.ShareChecksMount) != checked {
				t.Errorf("share checks %+v, verdicts mounted %v; want %v", ts.ShareChecks, strings.Contains(cat, spec.ShareChecksMount), checked)
			}
			if line := plan.Enforcement["persistent.files"]; !strings.Contains(line, "data volume") || strings.Contains(line, "ClamAV") != checked || strings.Contains(line, "read unchecked") != checked {
				t.Errorf("the receipt's line: %q", line)
			}
		})
	}
}

// TestAgentFilesRefusedOutsideAPersistentBuiltin: the folders are granted
// to a persistent plan of a built-in that declares no filesystem access of
// its own, and refused to anything else.
func TestAgentFilesRefusedOutsideAPersistentBuiltin(t *testing.T) {
	refused := func(name string, in Inputs) {
		t.Helper()
		var refusal *PolicyRefusal
		if _, err := Compile(in, t.TempDir()); !errors.As(err, &refusal) {
			t.Errorf("%s: err = %v, want a policy refusal", name, err)
		}
	}
	auto := persistentInputs(t, &spec.AgentFiles{Share: true})
	auto.Manifest = parseManifest(t, strings.Replace(pureTextYAML, "modes: [autonomous]", "modes: [autonomous, persistent]", 1))
	auto.Mode = manifest.ModeAutonomous
	refused("an autonomous plan", auto)
	reading := persistentInputs(t, &spec.AgentFiles{Share: true})
	m := parseManifest(t, strings.Replace(pureTextYAML, "modes: [autonomous]", "modes: [persistent]", 1)+"capabilities:\n  filesystem:\n    read: workspace\n")
	reading.Manifest, reading.WorkspaceDir = m, workspaceFor(t, m, nil)
	refused("a built-in that reads its workspace", reading)
}

// TestTheAppLinkIsAServerWithNoNetworkAndOneBind: a persistent built-in's
// app link is a catalogue entry of the runner image's applink role for its
// tool set, with no network, the supervisor's socket as its one bind (a bind
// root and a writable path of its own, exact), its set alone in the
// allowlist, and a receipt line.
func TestTheAppLinkIsAServerWithNoNetworkAndOneBind(t *testing.T) {
	in := persistentInputs(t, &spec.AgentFiles{Share: true, CheckShare: true})
	in.AppLink = applink.SetAssistant
	planDir := t.TempDir()
	plan, err := Compile(in, planDir)
	if err != nil {
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
	e := doc.Registry[applink.ServerName]
	if e == nil || e["image"] != "agentry-runner:test" || e["disableNetwork"] != true || e["user"] != spec.PHAppLinkUser || e["allowHosts"] != nil {
		t.Fatalf("the app link's entry: %v", e)
	}
	if vols, _ := e["volumes"].([]any); len(vols) != 1 || vols[0] != spec.PHAppLinkMount {
		t.Errorf("the app link's volumes: %v", e["volumes"])
	}
	if cmd, _ := e["command"].([]any); len(cmd) != 3 || cmd[0] != "applink" || cmd[2] != applink.SetAssistant {
		t.Errorf("the app link's command: %v", e["command"])
	}
	roots := strings.Split(gatewayEnv(plan, "MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS"), ":")
	writable := strings.Split(gatewayEnv(plan, "MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS"), ":")
	if !slices.Contains(roots, spec.PHAppLink) || !slices.Contains(writable, spec.PHAppLink) || !slices.Contains(plan.Gateway.Servers, applink.ServerName) {
		t.Errorf("roots %v, writable %v, servers %v", roots, writable, plan.Gateway.Servers)
	}
	allow, _ := os.ReadFile(filepath.Join(planDir, "gateway", "tools.yaml"))
	var tools map[string][]string
	if err := yaml.Unmarshal(allow, &tools); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tools[applink.ServerName], []string{applink.SelfConfigure}) {
		t.Errorf("the app link's allowlist: %v", tools[applink.ServerName])
	}
	if !strings.Contains(plan.Enforcement["persistent.appLink"], "no network") {
		t.Errorf("the receipt's line: %q", plan.Enforcement["persistent.appLink"])
	}
	if _, err := Compile(func() Inputs { i := persistentInputs(t, nil); i.AppLink = "nobody"; return i }(), t.TempDir()); err == nil {
		t.Error("an unknown tool set was compiled")
	}
}

// TestPersistentGrantsRefusedOutsideBuiltins: the folders and the app link
// are refused to a release from any source but a built-in, to a plan that is
// not persistent, and beside the web tools or the review grant.
func TestPersistentGrantsRefusedOutsideBuiltins(t *testing.T) {
	refused := func(name string, in Inputs) {
		t.Helper()
		var refusal *PolicyRefusal
		if _, err := Compile(in, t.TempDir()); !errors.As(err, &refusal) {
			t.Errorf("%s: err = %v, want a policy refusal", name, err)
		}
	}
	for _, source := range []string{"registry", "local"} {
		in := persistentInputs(t, &spec.AgentFiles{Share: true})
		in.Source = source
		refused("the folders from "+source, in)
		in = persistentInputs(t, nil)
		in.Source, in.AppLink = source, applink.SetMain
		refused("the app link from "+source, in)
	}
	auto := persistentInputs(t, nil)
	auto.Manifest = parseManifest(t, strings.Replace(pureTextYAML, "modes: [autonomous]", "modes: [autonomous, persistent]", 1))
	auto.Mode, auto.AppLink = manifest.ModeAutonomous, applink.SetMain
	refused("the app link on an autonomous plan", auto)
	web := persistentInputs(t, &spec.AgentFiles{})
	web.Web = true
	refused("the folders beside the web tools", web)
	review := persistentInputs(t, nil)
	review.AppLink, review.Review = applink.SetAssistant, true
	refused("the app link beside the review grant", review)
}
