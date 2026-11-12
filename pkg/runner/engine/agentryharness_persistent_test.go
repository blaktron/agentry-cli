package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/internal/scriptedmodel"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// A persistent agent's segments on the runner's own harness, with the real
// harness as the agent container. The folder that stands for the agent's
// session volume outlives a segment, as the volume does.

// startAgentryHarnessSegment starts one segment of the persistent fixture
// on the runner's own harness, on the conversation continueID names ("" for
// none), with store as the agent's session volume.
func startAgentryHarnessSegment(t *testing.T, model *scriptedmodel.Server, g *scriptedGateway, store, continueID string) (PersistentSession, func() []session.Event, <-chan struct{}, *dockerx.Fake) {
	t.Helper()
	p, f, _ := persistentFixture(t, continueID)
	m, err := manifest.Parse([]byte(persistentManifest))
	if err != nil {
		t.Fatal(err)
	}
	ad, err := adapter.Get(adapter.AgentryHarnessAdapterName)
	if err != nil {
		t.Fatal(err)
	}
	planDir := filepath.Join(p.RunRoot, "plan", ad.Name())
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Mode: manifest.ModePersistent, Digest: "sha256:feedface", Source: "builtin", AdapterName: ad.Name(),
		WorkspaceDir: p.WorkspaceSrc, Policy: p.Policy, PolicySHA: "sha", RunnerImage: "agentry-runner:test",
	}, planDir)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	p.Plan, p.PlanDir, p.Adapter = plan, planDir, ad
	f.Foreground["-probe"] = &dockerx.FakeForeground{Handler: probeListing("fs_read")}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: agentryHarnessContainer(t, model, g, store)}
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ps, ok := s.(PersistentSession)
	if !ok {
		t.Fatal("a segment's session is not a PersistentSession")
	}
	get, done := collect(s)
	return ps, get, done, f
}

func TestAPersistentAgentOnTheRunnersOwnHarnessKeepsItsConversationAcrossSegments(t *testing.T) {
	model := scriptedmodel.New(t,
		scriptedmodel.ToolCalls(scriptedmodel.Call{ID: "c1", Name: "fs_read", Arguments: `{"path":"notes.txt"}`}).WithUsage(scriptedmodel.Usage{Input: 1000, Output: 50}),
		scriptedmodel.Text("The note says hello.").WithUsage(scriptedmodel.Usage{Input: 1100, Output: 20}),
		scriptedmodel.Text("Still hello.").WithUsage(scriptedmodel.Usage{Input: 1200, Output: 10}),
	)
	g := newScriptedGateway(t, "fs_read")
	store := t.TempDir()
	ctx := context.Background()

	first, _, done, _ := startAgentryHarnessSegment(t, model, g, store, "")
	if err := first.Send(ctx, "what does the note say?"); err != nil {
		t.Fatal(err)
	}
	id, resumed, reason := first.Conversation()
	if id == "" || resumed {
		t.Fatalf("the first segment's conversation: %q %v %q", id, resumed, reason)
	}
	env, err := first.End(ctx, false)
	<-done
	if err != nil || env.Persistent == nil || env.Persistent.Resumed || env.Harness.Mode != "persistent" || env.Harness.Adapter != adapter.AgentryHarnessAdapterName {
		t.Fatalf("the first segment's envelope: %v, %+v, %+v", err, env.Persistent, env.Harness)
	}
	spent := env.Usage.CostUSD

	// The next segment is a new container on the same store.
	second, get, done, _ := startAgentryHarnessSegment(t, model, g, store, id)
	if got, resumed, reason := second.Conversation(); got != id || !resumed || reason != "" {
		t.Fatalf("the second segment's conversation: %q %v %q", got, resumed, reason)
	}
	if err := second.Send(ctx, "and now?"); err != nil {
		t.Fatal(err)
	}
	env, err = second.End(ctx, false)
	<-done
	if err != nil || env.Persistent == nil || !env.Persistent.Resumed {
		t.Fatalf("the second segment's envelope: %v, %+v", err, env.Persistent)
	}
	// The model reads the whole conversation, and nothing of the first
	// segment is said again in the second's events.
	req := model.Requests()[2]
	if n := len(req.Messages); n != 5 || req.Messages[3].Text != "The note says hello." {
		t.Errorf("the second segment's request holds %d messages", n)
	}
	for _, ev := range get() {
		if strings.Contains(ev.Text, "The note says hello.") {
			t.Errorf("the stored conversation reached the segment's events: %+v", ev)
		}
	}
	// A segment's usage is its own: one request here, and no more cost
	// than that request's.
	if u := env.Usage; u.InputTokens != 1200 || u.OutputTokens != 10 || (spent > 0 && u.CostUSD >= spent) {
		t.Errorf("the second segment's usage: %+v (the first cost %v)", u, spent)
	}
	if got := g.calls(); len(got) != 1 {
		t.Errorf("the gateway was sent %d calls, want the first segment's one", len(got))
	}
}

func TestAPersistentSegmentOnTheRunnersOwnHarnessStartsAnewWhenTheStoreCannotLoad(t *testing.T) {
	for name, tc := range map[string]struct {
		record string
		why    string
		sameID bool
	}{
		"a record it cannot read": {"not a record\n", "could not load", false},
		"nothing stored":          {"", "holds no conversation", true},
	} {
		t.Run(name, func(t *testing.T) {
			model := scriptedmodel.New(t, scriptedmodel.Text("Hello."))
			g := newScriptedGateway(t, "fs_read")
			store := t.TempDir()
			const stored = "3f0e1f8e-2c1b-4b5f-8f0a-1c2d3e4f5a6b"
			if tc.record != "" {
				if err := os.WriteFile(filepath.Join(store, stored+".jsonl"), []byte(tc.record), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			s, get, done, _ := startAgentryHarnessSegment(t, model, g, store, stored)
			id, resumed, reason := s.Conversation()
			if resumed || !strings.Contains(reason, tc.why) || (id == stored) != tc.sameID || id == "" {
				t.Fatalf("conversation = %q %v %q", id, resumed, reason)
			}
			if err := s.Send(context.Background(), "hello"); err != nil {
				t.Fatalf("the segment's first turn: %v", err)
			}
			env, err := s.End(context.Background(), false)
			<-done
			if err != nil || env.Persistent.Resumed || !strings.Contains(env.Persistent.ResumeReason, tc.why) {
				t.Fatalf("envelope: %v, %+v", err, env.Persistent)
			}
			for _, ev := range get() {
				if ev.Kind == session.KindError {
					t.Errorf("the conversation that gave way became an error: %+v", ev)
				}
			}
		})
	}
}

func TestAPersistentSegmentOnTheRunnersOwnHarnessStartsANewConversationAndCompacts(t *testing.T) {
	const summary = "The person greeted the agent twice."
	model := scriptedmodel.New(t,
		scriptedmodel.Text("Hello one.").WithUsage(scriptedmodel.Usage{Input: 900, Output: 10}),
		scriptedmodel.Text("Hello two.").WithUsage(scriptedmodel.Usage{Input: 400, Output: 10}),
		scriptedmodel.Text(summary).WithUsage(scriptedmodel.Usage{Input: 450, Output: 30}),
		scriptedmodel.Text("Hello three."),
	)
	g := newScriptedGateway(t, "fs_read")
	store := t.TempDir()
	ctx := context.Background()
	s, get, done, f := startAgentryHarnessSegment(t, model, g, store, "")
	if err := s.Send(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	old, _, _ := s.Conversation()

	r, ok := s.(ConversationRenewer)
	if !ok {
		t.Fatal("the segment cannot start a new conversation")
	}
	id, err := r.NewConversation(ctx, "a compaction")
	if err != nil || id == "" || id == old {
		t.Fatalf("NewConversation = %q, %v", id, err)
	}
	if got, resumed, reason := s.Conversation(); got != id || resumed || reason != "a compaction" {
		t.Errorf("Conversation = %q %v %q", got, resumed, reason)
	}
	if err := s.Send(ctx, "two"); err != nil {
		t.Fatal(err)
	}
	// The new conversation carries nothing of the one it left, which stays
	// on the store.
	if req := model.Requests()[1]; len(req.Messages) != 1 || req.Messages[0].Text != "two" {
		t.Errorf("the new conversation's first request: %+v", req.Messages)
	}
	for _, c := range []string{old, id} {
		if _, err := os.Stat(filepath.Join(store, c+".jsonl")); err != nil {
			t.Errorf("the record of %s: %v", c, err)
		}
	}

	c, ok := s.(ConversationCompactor)
	if !ok || !c.CanCompact() {
		t.Fatal("the segment cannot compact its conversation")
	}
	if err := c.CompactConversation(ctx); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if err := s.Send(ctx, "three"); err != nil {
		t.Fatal(err)
	}
	if req := model.Requests()[3]; len(req.Messages) != 1 || !strings.Contains(req.Messages[0].Text, summary) || !strings.HasSuffix(req.Messages[0].Text, "three") {
		t.Errorf("the request after the summary: %+v", req.Messages)
	}
	env, err := s.End(ctx, false)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	// One container did all of it, and the envelope says what it did.
	agents := 0
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "RunForeground:") && strings.HasSuffix(op, "-agent") {
			agents++
		}
	}
	if agents != 1 {
		t.Errorf("%d agent containers started, want the one", agents)
	}
	if nc := env.Persistent.NewConversations; len(nc) != 1 || nc[0].ID != id || nc[0].Reason != "a compaction" {
		t.Errorf("the envelope's new conversations: %+v", nc)
	}
	// The size after the summary is said, for whoever sizes the
	// conversation by it.
	compacted := 0
	for _, ev := range get() {
		if ev.Kind == session.KindUsage && ev.ContextCompacted {
			compacted++
			if ev.ContextTokens <= 0 || ev.ContextLimit <= 0 {
				t.Errorf("the size after the summary: %+v", ev)
			}
		}
	}
	if compacted != 1 {
		t.Errorf("%d usage events say the conversation was compacted, want 1", compacted)
	}
}
