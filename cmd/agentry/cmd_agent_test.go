package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// agentHome isolates the machine state and gives the runner a home short
// enough for an agent's socket.
func agentHome(t *testing.T) string {
	t.Helper()
	isolateEnv(t)
	home, err := os.MkdirTemp("", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv(policy.HomeEnv, home)
	return home
}

func exitCode(err error) int {
	var ce *engine.ClassifiedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return -1
}

// TestAgentCommandsRefuseWhatTheCLIDoesNotRun: Main is the desktop's, an
// agent that is not enabled does not start, and an unknown name is a usage
// error naming the agents.
func TestAgentCommandsRefuseWhatTheCLIDoesNotRun(t *testing.T) {
	agentHome(t)
	for _, args := range [][]string{{"agent", "start", "main"}, {"agent", "chat", "main"}, {"agent", "enable", "main"}} {
		_, _, err := runCLI(t, nil, args...)
		if err == nil || !strings.Contains(err.Error(), "Agentry Desktop") || exitCode(err) != engine.ExitUsage {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	_, _, err := runCLI(t, nil, "agent", "start", "assistant")
	if err == nil || !strings.Contains(err.Error(), "agentry agent enable assistant") || exitCode(err) != engine.ExitUsage {
		t.Errorf("start of a disabled agent: err = %v", err)
	}
	_, _, err = runCLI(t, nil, "agent", "show", "nobody")
	if err == nil || !strings.Contains(err.Error(), "main, assistant") {
		t.Errorf("unknown agent: err = %v", err)
	}
	_, _, err = runCLI(t, nil, "agent", "chat", "assistant")
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("chat with a stopped agent: err = %v", err)
	}
}

// TestAgentListShowsEachAgentsState: enable and disable write the agent's
// configuration, and list says how each agent stands.
func TestAgentListShowsEachAgentsState(t *testing.T) {
	home := agentHome(t)
	out, _, err := runCLI(t, nil, "agent", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "main") || !strings.Contains(out, "desktop only") || !strings.Contains(out, "assistant") || !strings.Contains(out, "disabled") {
		t.Errorf("list:\n%s", out)
	}
	if _, _, err := runCLI(t, nil, "agent", "enable", "assistant"); err != nil {
		t.Fatal(err)
	}
	cfg, err := persistent.LoadConfig(home, "assistant")
	if err != nil || !cfg.Enabled {
		t.Fatalf("config after enable = %+v, %v", cfg, err)
	}
	out, _, err = runCLI(t, nil, "--json", "agent", "list")
	if err != nil {
		t.Fatal(err)
	}
	var states []agentState
	if err := json.Unmarshal([]byte(out), &states); err != nil {
		t.Fatalf("%v in %s", err, out)
	}
	if len(states) != 3 || states[1].Agent != "assistant" || states[1].State != agentStopped || !states[0].DesktopOnly ||
		states[2].Agent != "coder" || states[2].State != agentDisabled || states[2].DesktopOnly {
		t.Errorf("states = %+v", states)
	}
}

// TestAgentStartRefusesASecondOwner: while another process holds the
// agent, start is refused naming it, before anything is launched.
func TestAgentStartRefusesASecondOwner(t *testing.T) {
	home := agentHome(t)
	if _, _, err := runCLI(t, nil, "agent", "enable", "assistant"); err != nil {
		t.Fatal(err)
	}
	owner, err := persistent.Acquire(home, "assistant", persistent.ClientDesktop)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	_, _, err = runCLI(t, nil, "agent", "start", "assistant")
	if err == nil || !strings.Contains(err.Error(), "already running under Agentry Desktop") {
		t.Errorf("err = %v", err)
	}
	_, _, err = runCLI(t, nil, "agent", "disable", "assistant")
	if err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("disable of a running agent: err = %v", err)
	}
	_, _, err = runCLI(t, nil, "agent", "reset", "assistant", "--confirm", "assistant")
	if err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("reset of a running agent: err = %v", err)
	}
}

// TestAgentResetNeedsTheName: without a terminal reset needs --confirm, a
// wrong name resets nothing, and the right one removes both volumes.
func TestAgentResetNeedsTheName(t *testing.T) {
	agentHome(t)
	f := &dockerx.Fake{}
	prev := agentDocker
	agentDocker = func() dockerx.Client { return f }
	t.Cleanup(func() { agentDocker = prev })
	_, _, err := runCLI(t, strings.NewReader(""), "agent", "reset", "assistant")
	if err == nil || !strings.Contains(err.Error(), "--confirm assistant") {
		t.Errorf("reset without a terminal: err = %v", err)
	}
	_, _, err = runCLI(t, nil, "agent", "reset", "assistant", "--confirm", "assistnat")
	if err == nil || len(f.Ops()) != 0 {
		t.Errorf("a wrong name: err = %v, ops %v", err, f.Ops())
	}
	if _, _, err := runCLI(t, nil, "agent", "reset", "assistant", "--confirm", "assistant"); err != nil {
		t.Fatal(err)
	}
	ops := strings.Join(f.Ops(), " ")
	if !strings.Contains(ops, "VolumeRemove:agentry-persistent-assistant-session") || !strings.Contains(ops, "VolumeRemove:agentry-persistent-assistant-data") {
		t.Errorf("ops = %s", ops)
	}
}

// TestAgentConfigureAndGrantAskForConsent: on a stopped agent, a name is
// saved as it is, a switch off needs nothing, a switch on that the person
// has not agreed to is refused without a terminal unless --yes agrees, a
// start of an agent whose roster is not agreed to is refused the same way,
// and a share folder the guards refuse is a usage error.
func TestAgentConfigureAndGrantAskForConsent(t *testing.T) {
	home := agentHome(t)
	if _, _, err := runCLI(t, nil, "agent", "enable", "assistant"); err != nil {
		t.Fatal(err)
	}
	_, _, err := runCLI(t, nil, "agent", "start", "assistant")
	if err == nil || !strings.Contains(err.Error(), "--yes") || exitCode(err) != engine.ExitPolicy {
		t.Fatalf("a start with nothing agreed to: err = %v", err)
	}
	if _, _, err := runCLI(t, nil, "agent", "configure", "assistant", "--name", "Sandra"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, nil, "agent", "grant", "assistant", "share", "off"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := persistent.LoadConfig(home, "assistant")
	if cfg.DisplayName != "Sandra" || cfg.Switches["share"] {
		t.Fatalf("config = %+v", cfg)
	}
	_, _, err = runCLI(t, nil, "agent", "grant", "assistant", "share", "on")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("a switch on without agreement: err = %v", err)
	}
	if _, _, err := runCLI(t, nil, "agent", "grant", "assistant", "share", "on", "--yes"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = persistent.LoadConfig(home, "assistant")
	if !cfg.Switches["share"] || cfg.Consented["share"] == "" {
		t.Fatalf("config after agreeing = %+v", cfg)
	}
	out, _, err := runCLI(t, nil, "agent", "show", "assistant")
	if err != nil || !strings.Contains(out, "Sandra") || !strings.Contains(out, "share") || !strings.Contains(out, "data") {
		t.Fatalf("show: %v\n%s", err, out)
	}
	_, _, err = runCLI(t, nil, "agent", "configure", "assistant", "--share", "/etc", "--yes")
	if err == nil || exitCode(err) != engine.ExitUsage {
		t.Errorf("a system folder as the share: err = %v", err)
	}
	_, _, err = runCLI(t, nil, "agent", "grant", "assistant", "data", "off")
	if err == nil || !strings.Contains(err.Error(), "always") {
		t.Errorf("switching off the agent's own files: err = %v", err)
	}
	_, _, err = runCLI(t, nil, "agent", "configure", "assistant", "--knowledge", "nope")
	if err == nil || !strings.Contains(err.Error(), "no pack of that name") {
		t.Errorf("a pack that is not kept: err = %v", err)
	}
}

// TestAgentJSONAgreementIsAskedOfTheProgram: with --json and no --yes, a
// change that needs agreement prints what to agree to and changes nothing;
// the same command with --yes then applies it.
func TestAgentJSONAgreementIsAskedOfTheProgram(t *testing.T) {
	home := agentHome(t)
	if _, _, err := runCLI(t, nil, "agent", "enable", "assistant"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, nil, "agent", "grant", "assistant", "share", "off"); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, nil, "agent", "grant", "assistant", "share", "on", "--json")
	if err == nil || exitCode(err) != engine.ExitPolicy || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("grant --json without --yes: err = %v", err)
	}
	var asked struct {
		Agent          string `json:"agent"`
		NeedsAgreement []struct {
			ID   string `json:"id"`
			Line string `json:"line"`
		} `json:"needsAgreement"`
	}
	if err := json.Unmarshal([]byte(out), &asked); err != nil {
		t.Fatalf("output is not one JSON object: %v\n%s", err, out)
	}
	if asked.Agent != "assistant" || len(asked.NeedsAgreement) != 1 || asked.NeedsAgreement[0].ID != "share" || asked.NeedsAgreement[0].Line == "" {
		t.Errorf("asked = %+v", asked)
	}
	if cfg, _ := persistent.LoadConfig(home, "assistant"); cfg.Switches["share"] {
		t.Fatal("the switch was turned on without agreement")
	}
	if _, _, err := runCLI(t, nil, "agent", "grant", "assistant", "share", "on", "--json", "--yes"); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := persistent.LoadConfig(home, "assistant"); !cfg.Switches["share"] {
		t.Error("the switch is off after agreeing")
	}
}

// TestAgentAttachIsForAProgram: attach speaks JSON only, and needs the agent
// running.
func TestAgentAttachIsForAProgram(t *testing.T) {
	agentHome(t)
	_, _, err := runCLI(t, nil, "agent", "attach", "assistant")
	if err == nil || exitCode(err) != engine.ExitUsage || !strings.Contains(err.Error(), "--json") {
		t.Errorf("attach without --json: err = %v", err)
	}
	_, _, err = runCLI(t, nil, "agent", "attach", "assistant", "--json")
	if err == nil || exitCode(err) != engine.ExitUsage || !strings.Contains(err.Error(), "not running") {
		t.Errorf("attach to a stopped agent: err = %v", err)
	}
	_, _, err = runCLI(t, nil, "agent", "attach", "main", "--json")
	if err == nil || !strings.Contains(err.Error(), "Agentry Desktop") {
		t.Errorf("attach to Main: err = %v", err)
	}
}

// TestAgentConfigurePublishesAPort: --port name=number saves one of the
// Coding agent's ports with agreement, --no-port takes it away, and a port
// that is not name=number, or one for the Assistant, is refused.
func TestAgentConfigurePublishesAPort(t *testing.T) {
	home := agentHome(t)
	for _, bad := range []string{"web", "web=", "web=http", "=3000"} {
		if _, _, err := runCLI(t, nil, "agent", "configure", "coder", "--port", bad, "--yes"); err == nil || exitCode(err) != engine.ExitUsage {
			t.Errorf("--port %q: err = %v", bad, err)
		}
	}
	if _, _, err := runCLI(t, nil, "agent", "configure", "coder", "--port", "web=3000", "--yes"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := persistent.LoadConfig(home, "coder")
	if len(cfg.Ports) != 1 || cfg.Ports[0] != (persistent.Port{Name: "web", Port: 3000}) || cfg.Consented["port:web"] == "" {
		t.Fatalf("config = %+v", cfg)
	}
	out, _, err := runCLI(t, nil, "agent", "show", "coder")
	if err != nil || !strings.Contains(out, "port:web") || !strings.Contains(out, "http://127.0.0.1:3000") {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if _, _, err := runCLI(t, nil, "agent", "configure", "coder", "--no-port", "web"); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := persistent.LoadConfig(home, "coder"); len(cfg.Ports) != 0 {
		t.Fatalf("after --no-port: %+v", cfg)
	}
	if _, _, err := runCLI(t, nil, "agent", "configure", "assistant", "--port", "web=3000", "--yes"); err == nil || !strings.Contains(err.Error(), "Coding agent alone") {
		t.Errorf("a port for the Assistant: err = %v", err)
	}
}

// TestAgentStartPrintsEachStage: the supervisor writes each stage its start
// reaches to its log as a "stage:" line in the words a person is shown, and
// agentry agent start's follower prints each as the log grows.
func TestAgentStartPrintsEachStage(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "supervisor.log")
	logf, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logf.Close() }()
	var shown strings.Builder
	follow := &logFollower{path: logPath, w: &shown}
	report := stageLines(logf)

	report(persistent.Stage{Name: receipt.StageImages})
	follow.flush()
	if shown.String() != "stage: Getting images ready\n" {
		t.Fatalf("shown %q after the first stage", shown.String())
	}
	report(persistent.Stage{Name: receipt.StageAgent, Subject: "Claude Code"})
	report(persistent.Stage{Name: receipt.StageAgent, Detail: engine.StageDetailResume})
	follow.flush()
	want := "stage: Getting images ready\nstage: Starting Claude Code\nstage: Picking up the conversation\n"
	if shown.String() != want {
		t.Fatalf("shown %q, want %q", shown.String(), want)
	}
}

// TestAgentShowSaysWhyAServerFailed: a server whose install failed is shown
// failed, with why, in show and in show --json, and switching it on again
// clears it for its next install.
func TestAgentShowSaysWhyAServerFailed(t *testing.T) {
	home := agentHome(t)
	if _, _, err := runCLI(t, nil, "agent", "enable", "assistant"); err != nil {
		t.Fatal(err)
	}
	srv := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(srv, []byte("pypi: office-word-mcp-server\nversion: 1.1.11\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, nil, "agent", "configure", "assistant", "--add-server", srv, "--yes"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := persistent.LoadConfig(home, "assistant")
	id := "server:pypi-office-word-mcp-server"
	cfg.ServerFailures = map[string]persistent.ServerFailure{id: {Reason: "resolve pypi:office-word-mcp-server@1.1.11: not found in the configured registry"}}
	if err := persistent.SaveConfig(home, "assistant", cfg); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, nil, "agent", "show", "assistant")
	if err != nil || !strings.Contains(out, "failed") || !strings.Contains(out, "why it failed: resolve pypi:office-word-mcp-server@1.1.11: not found") {
		t.Fatalf("show: %v\n%s", err, out)
	}
	js, _, err := runCLI(t, nil, "agent", "show", "assistant", "--json")
	if err != nil || !strings.Contains(js, `"state":"failed"`) || !strings.Contains(js, `"reason":"resolve pypi:`) {
		t.Fatalf("show --json: %v\n%s", err, js)
	}
	if _, _, err := runCLI(t, nil, "agent", "grant", "assistant", id, "on"); err != nil {
		t.Fatal(err)
	}
	if cfg, _ = persistent.LoadConfig(home, "assistant"); len(cfg.ServerFailures) != 0 {
		t.Errorf("switching the failed server on kept its failure: %v", cfg.ServerFailures)
	}
}
