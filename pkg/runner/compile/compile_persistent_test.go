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

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
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
			if line := plan.Enforcement["persistent.files"]; !strings.Contains(line, "data volume") || strings.Contains(line, "ClamAV") != checked {
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
