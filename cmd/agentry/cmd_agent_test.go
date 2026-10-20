package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
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
	if len(states) != 2 || states[1].Agent != "assistant" || states[1].State != agentStopped || !states[0].DesktopOnly {
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
