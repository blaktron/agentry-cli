package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// goneConversation is a stored conversation the fake Claude Code cannot find.
const goneConversation = "0e9d0000-0000-4000-8000-00000000dead"

// playPersistentClaude is Claude Code's stream-json with session
// persistence on: a --resume of a conversation it cannot find ends the
// process at once with its error result, before reading anything, as
// Claude Code 2.1.296 does; otherwise the initialize control request is
// answered and each user line gets an answer and a result.
func playPersistentClaude(r dockerx.RunSpec, in io.Reader, out io.Writer) int {
	if i := slices.Index(r.Argv, "--resume"); i >= 0 && i+1 < len(r.Argv) && r.Argv[i+1] == goneConversation {
		sayLine(out, map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true,
			"session_id": goneConversation, "errors": []string{"No conversation found with session ID: " + goneConversation}})
		return 1
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		var m struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Type {
		case "control_request":
			sayLine(out, map[string]any{"type": "control_response", "response": map[string]any{
				"subtype": "success", "request_id": m.RequestID, "response": map[string]any{"commands": []any{}}}})
		case "user":
			sayLine(out, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "noted"}}}})
			sayLine(out, map[string]any{"type": "result", "subtype": "success", "result": "noted", "stop_reason": "end_turn",
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 5}})
		}
	}
	return 0
}

// persistentClaudeFixture is a persistent built-in's segment on claude-image,
// continuing continueID ("" for none).
func persistentClaudeFixture(t *testing.T, continueID string) (Params, *dockerx.Fake) {
	t.Helper()
	m, err := manifest.Parse([]byte(persistentManifest))
	if err != nil {
		t.Fatal(err)
	}
	installDir := t.TempDir()
	ws := filepath.Join(installDir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "AGENT.md"), []byte("Keep talking.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.Images.Runner = "agentry-runner:test"
	pol.Model = policy.Model{Provider: policy.ProviderAnthropic, Name: "claude-sonnet-4-6", APIKeyKind: policy.KeyKindClaudeSignIn,
		ProxyAllow: policy.SignInProxyAllow(policy.KeyKindClaudeSignIn)}
	planDir := filepath.Join(installDir, "plan", "persistent")
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Mode: manifest.ModePersistent, Digest: "sha256:feedface", Source: "builtin",
		AdapterName:  adapter.ClaudeImageAdapterName,
		WorkspaceDir: ws, Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test",
	}, planDir)
	if err != nil {
		t.Fatal(err)
	}
	ad, err := adapter.Get(adapter.ClaudeImageAdapterName)
	if err != nil {
		t.Fatal(err)
	}
	f := &dockerx.Fake{}
	happyFake(f)
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: func(_ context.Context, r dockerx.RunSpec, stdin io.Reader, stdout, _ io.Writer) (int, error) {
		return playPersistentClaude(r, stdin, stdout), nil
	}}
	return Params{
		Plan: plan, PlanDir: planDir, WorkspaceSrc: ws, RunRoot: installDir, OutDir: t.TempDir(),
		Adapter: ad, Policy: pol, Attestation: receipt.Attestation{State: "builtin"},
		Stderr: os.Stderr, RunID: "5e9e0002", RunnerHome: installDir,
		ModelSignIn: &ModelSignInGrant{Kind: policy.KeyKindClaudeSignIn, ConfigDir: "agentry-claude-signin"},
		Persistent: &PersistentSegment{
			Agent: "keeper", Segment: 2, ContinueID: continueID,
			SessionVolume: "agentry-persistent-keeper-session", DataVolume: "agentry-persistent-keeper-data",
		},
	}, f
}

// agentCalls are the agent containers a session started, in order.
func agentCalls(f *dockerx.Fake) []dockerx.RunSpec {
	var out []dockerx.RunSpec
	for _, c := range f.Calls {
		if strings.HasSuffix(c.Run.Name, "-agent") && c.Op != "RemoveForce" {
			out = append(out, c.Run)
		}
	}
	return out
}

// conversationFlag is the agent's conversation flag and the id after it.
func conversationFlag(t *testing.T, r dockerx.RunSpec) (string, string) {
	t.Helper()
	n := len(r.Argv)
	if n < 2 || (r.Argv[n-2] != "--session-id" && r.Argv[n-2] != "--resume") {
		t.Fatalf("the agent's argv ends %q, not a conversation flag", r.Argv)
	}
	return r.Argv[n-2], r.Argv[n-1]
}

// TestAPersistentClaudeSegmentNamesItsConversation: a segment on claude-image
// with no stored conversation opens one under an id the engine chooses, with
// Claude Code's session persistence on and its projects folder on the
// agent's session volume, over the sign-in's volume; the next segment
// continues it by that id.
func TestAPersistentClaudeSegmentNamesItsConversation(t *testing.T) {
	p, f := persistentClaudeFixture(t, "")
	env, _, ps := runSegment(t, p, f)
	calls := agentCalls(f)
	if len(calls) != 1 {
		t.Fatalf("%d agent containers, want 1", len(calls))
	}
	flag, id := conversationFlag(t, calls[0])
	if flag != "--session-id" || uuid.Validate(id) != nil {
		t.Fatalf("the first segment's conversation flag %s %q", flag, id)
	}
	if slices.Contains(calls[0].Argv, "--no-session-persistence") {
		t.Fatal("a persistent segment's Claude Code keeps no transcript")
	}
	want := []dockerx.Mount{
		{Source: "agentry-claude-signin", Target: adapter.ClaudeImageConfigDir},
		{Source: "agentry-persistent-keeper-session", Target: adapter.ClaudeImageConfigDir + "/projects"},
	}
	for _, w := range want {
		if !slices.Contains(calls[0].Mounts, w) {
			t.Errorf("the agent's mounts %+v lack %+v", calls[0].Mounts, w)
		}
	}
	// The sign-in's volume and the agent's conversation reach the agent
	// container alone; the store's one-shot gives the conversation's volume
	// to the harness's user and mounts nothing else.
	for _, c := range f.Calls {
		if strings.HasSuffix(c.Run.Name, "-agent") {
			continue
		}
		for _, m := range c.Run.Mounts {
			oneShot := strings.HasSuffix(c.Run.Name, "-session-init") && m.Source == "agentry-persistent-keeper-session" && len(c.Run.Mounts) == 1
			if (m.Source == "agentry-claude-signin" || m.Source == "agentry-persistent-keeper-session" || strings.HasPrefix(m.Target, adapter.ClaudeImageConfigDir)) && !oneShot {
				t.Errorf("%s mounts %+v", c.Run.Name, m)
			}
		}
	}
	got, resumed, reason := ps.Conversation()
	if got != id || resumed || reason != noStoredConversation {
		t.Errorf("conversation = %q %v %q, want %q", got, resumed, reason, id)
	}
	if env.Persistent == nil || env.Persistent.Resumed {
		t.Errorf("envelope persistent = %+v", env.Persistent)
	}

	p2, f2 := persistentClaudeFixture(t, id)
	env2, _, ps2 := runSegment(t, p2, f2)
	calls = agentCalls(f2)
	if len(calls) != 1 {
		t.Fatalf("%d agent containers, want 1", len(calls))
	}
	if flag, got := conversationFlag(t, calls[0]); flag != "--resume" || got != id {
		t.Fatalf("the next segment's conversation flag %s %q, want --resume %q", flag, got, id)
	}
	got, resumed, reason = ps2.Conversation()
	if got != id || !resumed || reason != "" {
		t.Errorf("conversation = %q %v %q", got, resumed, reason)
	}
	if env2.Persistent == nil || !env2.Persistent.Resumed {
		t.Errorf("envelope persistent = %+v", env2.Persistent)
	}
}

// TestAPersistentClaudeSegmentStartsAnewWhenItsConversationIsGone: Claude
// Code ending at its start because it cannot find the stored conversation
// does not end the segment; the engine starts it again on a new
// conversation, and the envelope says why in Claude Code's words.
func TestAPersistentClaudeSegmentStartsAnewWhenItsConversationIsGone(t *testing.T) {
	p, f := persistentClaudeFixture(t, goneConversation)
	env, evs, ps := runSegment(t, p, f)
	calls := agentCalls(f)
	if len(calls) != 2 {
		t.Fatalf("%d agent containers, want the refused one and its replacement", len(calls))
	}
	if flag, id := conversationFlag(t, calls[0]); flag != "--resume" || id != goneConversation {
		t.Fatalf("first start %s %q", flag, id)
	}
	flag, id := conversationFlag(t, calls[1])
	if flag != "--session-id" || id == goneConversation || uuid.Validate(id) != nil {
		t.Fatalf("second start %s %q", flag, id)
	}
	got, resumed, reason := ps.Conversation()
	if got != id || resumed || !strings.Contains(reason, "No conversation found") {
		t.Errorf("conversation = %q %v %q", got, resumed, reason)
	}
	if env.Persistent == nil || env.Persistent.Resumed || !strings.Contains(env.Persistent.ResumeReason, "No conversation found") {
		t.Errorf("envelope persistent = %+v", env.Persistent)
	}
	answers := 0
	for _, ev := range evs {
		if ev.Kind == "assistant_turn" {
			answers++
		}
	}
	if answers != 3 {
		t.Errorf("%d answers on the new conversation, want 3: %+v", answers, evs)
	}
	if env.Status != "ok" && env.Status != "stopped" {
		t.Errorf("the segment ended %q", env.Status)
	}
}

// TestAPersistentClaudeSegmentTakesOnlyAConversationIDItsHarnessNames: a
// stored id that is not a UUID never reaches Claude Code's command line,
// where it could be read as a flag; the segment opens a new conversation
// and says why.
func TestAPersistentClaudeSegmentTakesOnlyAConversationIDItsHarnessNames(t *testing.T) {
	p, f := persistentClaudeFixture(t, "--permission-mode=bypassPermissions")
	env, _, ps := runSegment(t, p, f)
	calls := agentCalls(f)
	if len(calls) != 1 {
		t.Fatalf("%d agent containers, want 1", len(calls))
	}
	for _, a := range calls[0].Argv {
		if strings.Contains(a, "bypassPermissions") {
			t.Fatalf("the stored id reached the argv: %q", calls[0].Argv)
		}
	}
	flag, id := conversationFlag(t, calls[0])
	if flag != "--session-id" || uuid.Validate(id) != nil {
		t.Fatalf("conversation flag %s %q", flag, id)
	}
	if got, resumed, reason := ps.Conversation(); got != id || resumed || !strings.Contains(reason, "not one this harness names") {
		t.Errorf("conversation = %q %v %q", got, resumed, reason)
	}
	if env.Persistent == nil || env.Persistent.Resumed {
		t.Errorf("envelope persistent = %+v", env.Persistent)
	}
}
