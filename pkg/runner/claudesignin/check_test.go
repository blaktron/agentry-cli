package claudesignin

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
)

// The save check is one message to the image's Claude Code on its volume,
// in a container given none of the person's variables, read from its JSON.
func TestTheSaveCheckSendsTheImagesClaudeCodeOneMessage(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-person-0000")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	s, f := fixture(t, &dockerx.FakeForeground{}, signedIn)
	f.Foreground["agentry-claude-check"] = &dockerx.FakeForeground{
		Stdout: `{"type":"result","subtype":"success","is_error":false,"result":"OK","modelUsage":{"claude-sonnet-5-5":{}}}`}
	if _, err := modelsignin.Open(s.Home, nil).Put(modelsignin.Claude, modelsignin.Record{ConfigDir: Volume}, modelsignin.Grant{}); err != nil {
		t.Fatal(err)
	}
	a, err := s.CheckModel(context.Background(), "sonnet")
	if err != nil || !a.OK || a.Resolved != "claude-sonnet-5-5" {
		t.Fatalf("answer %+v, %v", a, err)
	}
	var run *dockerx.RunSpec
	for i := range f.Calls {
		if f.Calls[i].Op == "RunForeground" && strings.Contains(f.Calls[i].Arg, "agentry-claude-check") {
			run = &f.Calls[i].Run
		}
	}
	if run == nil {
		t.Fatal("no check container ran")
	}
	if !slices.Contains(run.Argv, "--max-turns") || !slices.Contains(run.Argv, "sonnet") {
		t.Fatalf("argv %q", run.Argv)
	}
	if len(run.Mounts) != 1 || run.Mounts[0].Source != Volume {
		t.Fatalf("mounts %+v: want the sign-in's volume alone", run.Mounts)
	}
	for k := range run.Env {
		if strings.HasPrefix(k, "ANTHROPIC_") || strings.HasPrefix(k, "CLAUDE_CODE_") {
			t.Errorf("the check's container is given $%s", k)
		}
	}
}

func TestTheSaveCheckWithNoClaudeSignInIsNotSignedIn(t *testing.T) {
	s, f := fixture(t, &dockerx.FakeForeground{}, signedIn)
	a, err := s.CheckModel(context.Background(), "sonnet")
	if err != nil || !a.NotSignedIn {
		t.Fatalf("answer %+v, %v", a, err)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("the daemon was asked: %v", f.Calls)
	}
}

func TestTheInstalledClaudeCodesCheckHoldsNoneOfThePersonsClaudeVariables(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "https://elsewhere.example")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "token")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/person/.claude")
	env := checkEnv("/store/claude-code")
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "CLAUDE_CODE_") || kv == "CLAUDE_CONFIG_DIR=/home/person/.claude" {
			t.Errorf("the check's environment holds %s", kv)
		}
	}
	if !slices.Contains(env, "CLAUDE_CONFIG_DIR=/store/claude-code") {
		t.Error("the check does not run in Agentry's own directory")
	}
}
