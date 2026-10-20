package persistent

import (
	"context"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// TestAConversationIsNeverOfferedToAnotherHarness: a conversation stored by
// one harness is continued only by that harness. When the machine's model
// setup moves the agent to another (here a Claude subscription, which runs
// on claude-image), the next segment is given no conversation to continue,
// says why, and carries the last exchange; the one after continues the new
// harness's own conversation.
func TestAConversationIsNeverOfferedToAnotherHarness(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st := &starter{}
	sv := startAssistant(t, home, st)
	if err := sv.Send(context.Background(), "my cat is called Biscuit"); err != nil {
		t.Fatal(err)
	}
	if _, err := sv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := LoadConfig(home, "assistant"); cfg.Conversation == nil || cfg.Conversation.Adapter != adapter.DefaultName {
		t.Fatalf("the stored conversation %+v", cfg.Conversation)
	}

	claude := policy.Default()
	claude.Model = policy.Model{Provider: policy.ProviderAnthropic, Name: "claude-sonnet-4-6", APIKeyKind: policy.KeyKindClaudeSignIn,
		ProxyAllow: policy.SignInProxyAllow(policy.KeyKindClaudeSignIn)}
	startOn := func(pol *policy.Policy) *Supervisor {
		t.Helper()
		sv, err := Start(context.Background(), Options{
			Home: home, Agent: assistantAgent(t), Client: ClientCLI, Policy: pol,
			Docker: &dockerx.Fake{}, StartSegment: st.start,
		})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		return sv
	}
	sv2 := startOn(claude)
	if got := st.segments[1].Persistent.ContinueID; got != "" {
		t.Errorf("claude-image was offered %s's conversation %q", adapter.DefaultName, got)
	}
	if !strings.Contains(st.last.reason, "stored by the docker-agent harness") || !strings.Contains(st.last.reason, "last exchange") {
		t.Errorf("reason = %q", st.last.reason)
	}
	if _, err := sv2.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := LoadConfig(home, "assistant"); cfg.Conversation == nil || cfg.Conversation.Adapter != adapter.ClaudeImageAdapterName {
		t.Fatalf("the stored conversation %+v", cfg.Conversation)
	}

	sv3 := startOn(claude)
	defer func() { _, _ = sv3.Stop(context.Background()) }()
	if got := st.segments[2].Persistent.ContinueID; got != "conv-1" {
		t.Errorf("claude-image's own conversation was not continued: %q", got)
	}
}
