package compile

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// wordSpec is a package server with no network, as the agent's store
// resolves it.
func wordSpec() spec.MCPServerSpec {
	return spec.MCPServerSpec{
		Name: "mcp-pypi-office-word-mcp-server", Source: "pypi",
		Package: "office-word-mcp-server", Version: "1.1.11", Integrity: "sha256:aaa",
		Image:       "python:3.13-slim",
		Command:     []string{"/opt/agentry-mcp/pypi-office-word-mcp-server/bin/word_mcp_server"},
		StoreSubdir: "pypi-office-word-mcp-server",
	}
}

// shareVolumes are a catalogue entry's binds of the share or its mirror.
func shareVolumes(e map[string]any) []string {
	var out []string
	for _, v := range strs(e["volumes"]) {
		if strings.HasPrefix(v, spec.PHShare+":") || strings.HasPrefix(v, spec.PHShareMirror+":") {
			out = append(out, v)
		}
	}
	return out
}

// TestNoServerBindsTheShareWritable: a server that sees the share binds the
// mirror or the folder read-only at the share's path; the share stays the
// one writable folder beside the output folder, for the tools server alone,
// and the mirror is a bind root no container may write.
func TestNoServerBindsTheShareWritable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    func(t *testing.T) Inputs
		want  string
		root  string
		other string
	}{
		{"the Assistant: the mirror", func(t *testing.T) Inputs {
			return persistentInputs(t, &spec.AgentFiles{Share: true, CheckShare: true})
		}, spec.PHShareMirror + ":" + spec.AgentRoot + "/" + spec.AgentShareDir + ":ro", spec.PHShareMirror, ""},
		{"the Coding agent: the folder", func(t *testing.T) Inputs {
			return coderInputs(t, spec.CoderGrant{})
		}, spec.PHShare + ":" + spec.AgentRoot + "/" + spec.AgentShareDir + ":ro", spec.PHShare, spec.PHShareMirror},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in(t)
			in.MCPServers = []spec.MCPServerSpec{wordSpec()}
			in.ShareServers = []string{"pypi:office-word-mcp-server"}
			plan, _, reg := compiledCoder(t, in)
			if got := shareVolumes(reg["mcp-pypi-office-word-mcp-server"]); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("the server's share binds %v, want %s", got, tc.want)
			}
			for name, e := range reg {
				if name == spec.ToolServerName {
					continue // the share's one writer
				}
				for _, v := range shareVolumes(e) {
					if !strings.HasSuffix(v, ":ro") {
						t.Errorf("%s binds %s writable", name, v)
					}
				}
			}
			// The share is written by the tools server alone; the server
			// saves into the share outbox, which no other container binds.
			outbox := spec.PHShareOut + ":" + spec.AgentRoot + "/" + spec.AgentShareOutDir
			for name, e := range reg {
				if v := strs(e["volumes"]); slices.Contains(v, outbox) != (name == "mcp-pypi-office-word-mcp-server") {
					t.Errorf("%s binds %v", name, v)
				}
			}
			writable := strings.Split(gatewayEnv(plan, "MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS"), ":")
			if !slices.Equal(writable, []string{spec.PHOut, spec.PHShare, spec.PHShareOut}) {
				t.Errorf("the writable list is %v, want the output folder, the share and the share outbox alone", writable)
			}
			roots := gatewayEnv(plan, "MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS")
			if !strings.Contains(roots, tc.root) || tc.other != "" && strings.Contains(roots, tc.other) {
				t.Errorf("the bind roots %s, want %s and not %q", roots, tc.root, tc.other)
			}
			if plan.MCPServers[0].Share == "" || !strings.Contains(plan.Enforcement["persistent.serverShare"], "mcp-pypi-office-word-mcp-server") {
				t.Errorf("the plan's server %+v, receipt %q", plan.MCPServers[0], plan.Enforcement["persistent.serverShare"])
			}
		})
	}
	// A server not named sees nothing.
	in := persistentInputs(t, &spec.AgentFiles{Share: true, CheckShare: true})
	in.MCPServers = []spec.MCPServerSpec{wordSpec()}
	plan, _, reg := compiledCoder(t, in)
	if got := shareVolumes(reg["mcp-pypi-office-word-mcp-server"]); len(got) != 0 || strings.Contains(gatewayEnv(plan, "MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS"), spec.PHShareMirror) ||
		strings.Contains(gatewayEnv(plan, "MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS"), spec.PHShareOut) || strings.Contains(gatewayEnv(plan, "MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS"), spec.PHShareOut) {
		t.Fatalf("a server the person did not let see the share binds %v", got)
	}
	if _, ok := plan.Enforcement["persistent.serverShare"]; ok {
		t.Error("the receipt names a server that sees the share")
	}
}

// TestLiveShareReadOnlyForTheCoderAlone: the share folder itself reaches a
// server only beside the Coding agent; where the share is checked a server
// sees the mirror, never the folder; with the share off, or outside a
// persistent built-in, no server is given it.
func TestLiveShareReadOnlyForTheCoderAlone(t *testing.T) {
	in := persistentInputs(t, &spec.AgentFiles{Share: true, CheckShare: true})
	in.MCPServers, in.ShareServers = []spec.MCPServerSpec{wordSpec()}, []string{"pypi:office-word-mcp-server"}
	_, _, reg := compiledCoder(t, in)
	for _, v := range shareVolumes(reg["mcp-pypi-office-word-mcp-server"]) {
		if strings.HasPrefix(v, spec.PHShare+":") {
			t.Fatalf("a server of a checked share binds the share folder itself: %s", v)
		}
	}
	for name, files := range map[string]*spec.AgentFiles{
		"an unchecked share beside no Coding agent": {Share: true},
		"no share":   {},
		"no folders": nil,
	} {
		in := persistentInputs(t, files)
		in.MCPServers, in.ShareServers = []spec.MCPServerSpec{wordSpec()}, []string{"pypi:office-word-mcp-server"}
		var refusal *PolicyRefusal
		if _, err := Compile(in, t.TempDir()); !errors.As(err, &refusal) {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
	rel := persistentInputs(t, &spec.AgentFiles{Share: true, CheckShare: true})
	rel.Source = "registry"
	rel.AgentFiles = nil
	rel.MCPServers, rel.ShareServers = []spec.MCPServerSpec{wordSpec()}, []string{"pypi:office-word-mcp-server"}
	if _, err := Compile(rel, t.TempDir()); err == nil {
		t.Error("a release was given a share for its server")
	}
}

// TestCompileRefusesTheShareToAServerWithNetwork: a server that reaches a
// host, a remote server and one that signs in by OAuth are refused the
// share; so is a server the plan does not declare.
func TestCompileRefusesTheShareToAServerWithNetwork(t *testing.T) {
	egress := wordSpec()
	egress.Egress = []string{"api.example.com"}
	remote := spec.MCPServerSpec{Name: "mcp-url-mcp-example-com-mcp", Source: spec.SourceURL, Package: "https://mcp.example.com/mcp", Auth: "none"}
	oauth := workspaceMCPSpec()
	oauth.Egress = nil
	for name, tc := range map[string]struct {
		srv spec.MCPServerSpec
		id  string
	}{
		"egress": {egress, "pypi:office-word-mcp-server"},
		"remote": {remote, remote.SourceID()},
		"oauth":  {oauth, "pypi:workspace-mcp"},
	} {
		in := persistentInputs(t, &spec.AgentFiles{Share: true, CheckShare: true})
		in.MCPServers, in.ShareServers = []spec.MCPServerSpec{tc.srv}, []string{tc.id}
		var refusal *PolicyRefusal
		if _, err := Compile(in, t.TempDir()); !errors.As(err, &refusal) {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
	in := persistentInputs(t, &spec.AgentFiles{Share: true, CheckShare: true})
	in.MCPServers, in.ShareServers = []spec.MCPServerSpec{wordSpec()}, []string{"npm:other"}
	if _, err := Compile(in, t.TempDir()); err == nil {
		t.Error("the share was given to a server the plan does not declare")
	}
}

// TestThePromptSaysWhereAServerSeesTheShare: with a server that sees the
// share, the System Prompt names it and the path to pass to its tools.
func TestThePromptSaysWhereAServerSeesTheShare(t *testing.T) {
	in := persistentInputs(t, &spec.AgentFiles{Share: true, CheckShare: true})
	in.MCPServers, in.ShareServers = []spec.MCPServerSpec{wordSpec()}, []string{"pypi:office-word-mcp-server"}
	dir := t.TempDir()
	plan, err := Compile(in, dir)
	if err != nil {
		t.Fatal(err)
	}
	instr := renderedInstruction(t, dir, plan.Harness.Adapter)
	if !strings.Contains(instr, "mcp-pypi-office-word-mcp-server see the person's share folder, read only") || !strings.Contains(instr, spec.AgentRoot+"/"+spec.AgentShareDir+"/<file>") {
		t.Fatalf("the System Prompt does not say where the server sees the share:\n%s", instr)
	}
	if !strings.Contains(instr, spec.AgentRoot+"/"+spec.AgentShareOutDir+"/<name>") || !strings.Contains(instr, `"<name> (2)"`) || !strings.Contains(instr, "never over a file") {
		t.Fatalf("the System Prompt does not say where the server saves into the share:\n%s", instr)
	}
	in.ShareServers = nil
	dir = t.TempDir()
	if plan, err = Compile(in, dir); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(renderedInstruction(t, dir, plan.Harness.Adapter), "Your share for MCP servers") {
		t.Error("the System Prompt names a share for servers no server sees")
	}
}
