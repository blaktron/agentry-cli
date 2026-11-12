package persistent

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/inference"
	"github.com/blaktron/agentry-cli/pkg/runner/inference/inferencetest"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// startOnPolicy starts the Assistant on a policy file of the test's own,
// returning the file's path.
func startOnPolicy(t *testing.T, home string, st *starter) (*Supervisor, string) {
	t.Helper()
	path := filepath.Join(home, "policy.yaml")
	if err := policy.Save(path, policy.Default()); err != nil {
		t.Fatal(err)
	}
	p, sha, err := policy.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	sv, err := Start(context.Background(), Options{
		Home: home, Agent: assistantAgent(t), Client: ClientCLI, Policy: p, PolicySHA: sha, PolicyFile: &policy.File{Path: path},
		Docker: &dockerx.Fake{}, StartSegment: st.start, Stderr: &syncBuffer{},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _, _ = sv.Stop(context.Background()) })
	return sv, path
}

// editPolicy changes the policy file at path.
func editPolicy(t *testing.T, path string, change func(*policy.Policy)) {
	t.Helper()
	p, _, err := policy.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	change(p)
	if err := policy.Save(path, p); err != nil {
		t.Fatal(err)
	}
}

// TestAPolicyChangeReachesTheNextTurn: a policy file changed while the
// agent runs makes it pending, and the next turn starts a segment on the
// new policy, the model's label following.
func TestAPolicyChangeReachesTheNextTurn(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st := &starter{resumed: true}
	sv, path := startOnPolicy(t, home, st)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if sv.Status().Pending {
		t.Fatal("pending before the policy changed")
	}
	editPolicy(t, path, func(p *policy.Policy) { p.Model.Name = "a-newer-model" })
	if !sv.Status().Pending {
		t.Fatal("a changed policy is not pending")
	}
	if err := sv.Send(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	n, last := len(st.segments), st.segments[len(st.segments)-1]
	st.mu.Unlock()
	if n != 2 || last.Policy == nil || last.Policy.Model.Name != "a-newer-model" {
		t.Fatalf("segments %d; the next ran on %+v", n, last.Policy)
	}
	if status := sv.Status(); status.Pending || status.Model != last.Policy.Model.Provider+"/a-newer-model" {
		t.Errorf("after the turn: %+v", status)
	}
}

// TestAPolicyThatCannotBeReadKeepsThePolicyTheAgentHas: a policy file
// broken while the agent runs changes nothing: no restart, and the segment
// keeps the policy it has.
func TestAPolicyThatCannotBeReadKeepsThePolicyTheAgentHas(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st := &starter{resumed: true}
	sv, path := startOnPolicy(t, home, st)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("model: [this is not a policy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if sv.Status().Pending {
		t.Fatal("an unreadable policy is pending")
	}
	if err := sv.Send(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	n := len(st.segments)
	st.mu.Unlock()
	if n != 1 {
		t.Errorf("an unreadable policy started %d segments", n)
	}
	// A segment started for another reason keeps the policy, and says why.
	if _, err := sv.Change(Change{Switch: &Switch{ID: EntryShare, On: false}}); err != nil {
		t.Fatal(err)
	}
	if err := sv.Send(ctx, "third"); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	last := st.segments[len(st.segments)-1]
	st.mu.Unlock()
	if last.Policy == nil || last.Policy.Model.Name != policy.Default().Model.Name {
		t.Errorf("the segment after an unreadable policy ran on %+v", last.Policy)
	}
	if !strings.Contains(sv.opts.Stderr.(*syncBuffer).String(), "cannot be read") {
		t.Error("the log does not say the policy cannot be read")
	}
}

// TestAHarnessChangeStartsANewConversationCarryingTheLast: moving the
// policy to another harness starts the next segment on it; the stored
// conversation, the old harness's, is not offered to it.
func TestAHarnessChangeStartsANewConversationCarryingTheLast(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st := &starter{resumed: true}
	sv, path := startOnPolicy(t, home, st)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	before := sv.Status().Adapter
	// The next harness cannot continue the conversation, so its segment
	// does not report one continued.
	st.mu.Lock()
	st.resumed = false
	st.mu.Unlock()
	editPolicy(t, path, func(p *policy.Policy) {
		p.Model.Provider, p.Model.Name, p.Model.APIKeyKind = "anthropic", "claude-sonnet-5-5", policy.KeyKindClaudeSignIn
	})
	if err := sv.Send(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	n, last := len(st.segments), st.segments[len(st.segments)-1]
	st.mu.Unlock()
	if n != 2 || sv.Status().Adapter == before || sv.Status().Adapter != adapter.ClaudeImageAdapterName {
		t.Fatalf("segments %d; the adapter %q, before %q", n, sv.Status().Adapter, before)
	}
	if last.Persistent.ContinueID != "" {
		t.Errorf("the old harness's conversation %q was offered to the new one", last.Persistent.ContinueID)
	}
	st.mu.Lock()
	raw := st.last.raw
	st.mu.Unlock()
	if len(raw) != 1 || !strings.Contains(raw[0], "first") || !strings.HasSuffix(raw[0], "second") {
		t.Errorf("the new harness was not handed the last exchange: %q", raw)
	}
	if c, _ := LoadConfig(home, "assistant"); c.Conversation == nil || c.Conversation.Adapter != adapter.ClaudeImageAdapterName {
		t.Errorf("the stored conversation is the old harness's: %+v", c.Conversation)
	}
}

// TestAnAgentFollowsThePolicysContainerHarness: a running agent whose
// policy comes to choose the runner's own harness starts its next segment
// on it, handed the last exchange and not the other harness's conversation,
// and goes back the same way when the policy does.
func TestAnAgentFollowsThePolicysContainerHarness(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st := &starter{resumed: true}
	sv, path := startOnPolicy(t, home, st)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if got := sv.Status().Adapter; got != adapter.DefaultName {
		t.Fatalf("an agent under a policy that never chose runs on %q", got)
	}
	st.mu.Lock()
	st.resumed = false
	st.mu.Unlock()
	for i, name := range []string{adapter.AgentryHarnessAdapterName, adapter.DefaultName} {
		editPolicy(t, path, func(p *policy.Policy) { p.ContainerHarness = name })
		said := []string{"second", "third"}[i]
		if err := sv.Send(ctx, said); err != nil {
			t.Fatal(err)
		}
		st.mu.Lock()
		n, last, raw := len(st.segments), st.segments[len(st.segments)-1], st.last.raw
		st.mu.Unlock()
		if n != i+2 || sv.Status().Adapter != name || last.Adapter != name {
			t.Fatalf("containerHarness %s: %d segments, the agent on %q, its segment on %q", name, n, sv.Status().Adapter, last.Adapter)
		}
		if last.Persistent.ContinueID != "" {
			t.Errorf("containerHarness %s: the other harness's conversation %q was offered", name, last.Persistent.ContinueID)
		}
		if len(raw) != 1 || !strings.HasSuffix(raw[0], said) || raw[0] == said {
			t.Errorf("containerHarness %s: the segment was not handed the last exchange: %q", name, raw)
		}
		if c, _ := LoadConfig(home, "assistant"); c.Conversation == nil || c.Conversation.Adapter != name {
			t.Errorf("containerHarness %s: the stored conversation is %+v", name, c.Conversation)
		}
	}
}

// TestAModelThatComesToPayWithCreditsOpensItsSession: a running agent whose
// policy moves to Agentry credits opens a session with a client for that
// policy before it takes it, its next segment paying with it; moving back
// ends the session and hands the segment no token.
func TestAModelThatComesToPayWithCreditsOpensItsSession(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	reg := inferencetest.New(t)
	u, _ := url.Parse(reg.URL)
	path := filepath.Join(home, "policy.yaml")
	if err := policy.Save(path, policy.Default()); err != nil {
		t.Fatal(err)
	}
	p, sha, _ := policy.Load(path)
	st := &starter{resumed: true}
	var asked []string
	sv, err := Start(context.Background(), Options{
		Home: home, Agent: assistantAgent(t), Client: ClientCLI, Policy: p, PolicySHA: sha, PolicyFile: &policy.File{Path: path},
		Docker: &dockerx.Fake{}, StartSegment: st.start,
		CreditsFor: func(p *policy.Policy) *inference.Client {
			asked = append(asked, p.Model.Provider)
			return inference.New(reg.URL, func() (string, error) { return inferencetest.SignIn, nil }, nil)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = sv.Stop(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	keyed := policy.Default().Model
	editPolicy(t, path, func(p *policy.Policy) {
		p.Model = policy.Model{Provider: policy.ProviderAgentry, Name: "agentry", BaseURL: reg.URL + policy.AgentryInferencePath, ProxyAllow: []string{u.Host}}
	})
	if err := sv.Send(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	last := st.segments[len(st.segments)-1]
	st.mu.Unlock()
	if len(reg.Opened()) != 1 || last.Inference == nil || last.Inference.Token != inferencetest.TokenFor(1) || len(asked) != 1 {
		t.Fatalf("on credits: opened %v, the segment's grant %+v, clients asked for %v", reg.Opened(), last.Inference, asked)
	}
	editPolicy(t, path, func(p *policy.Policy) { p.Model = keyed })
	if err := sv.Send(ctx, "third"); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	last = st.segments[len(st.segments)-1]
	st.mu.Unlock()
	if last.Inference != nil {
		t.Fatalf("off credits, the segment was handed %+v", last.Inference)
	}
	eventually(t, "the session ended", func() bool { return len(reg.Ended()) == 1 })
}

// TestAPolicyNoSegmentCanStartOnIsSetAside: a policy whose segment cannot
// start is set aside, the agent going on with the policy it had and saying
// why, and it is not tried again until the file changes.
func TestAPolicyNoSegmentCanStartOnIsSetAside(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st := &starter{resumed: true}
	var log syncBuffer
	path := filepath.Join(home, "policy.yaml")
	if err := policy.Save(path, policy.Default()); err != nil {
		t.Fatal(err)
	}
	p, sha, _ := policy.Load(path)
	sv, err := Start(context.Background(), Options{
		Home: home, Agent: assistantAgent(t), Client: ClientCLI, Policy: p, PolicySHA: sha, PolicyFile: &policy.File{Path: path},
		Docker: &dockerx.Fake{}, StartSegment: st.start, Stderr: &log,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = sv.Stop(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	editPolicy(t, path, func(p *policy.Policy) { p.Model.Name = "a-model-no-engine-runs" })
	refuseWith(st, func(seg localrun.Segment) error {
		if seg.Policy.Model.Name == "a-model-no-engine-runs" {
			return errors.New("the engine could not start it")
		}
		return nil
	})
	if err := sv.Send(ctx, "second"); err != nil {
		t.Fatalf("the turn after a policy no segment starts on: %v", err)
	}
	st.mu.Lock()
	last := st.segments[len(st.segments)-1]
	st.mu.Unlock()
	if last.Policy.Model.Name == "a-model-no-engine-runs" || sv.Status().Pending {
		t.Fatalf("the agent went on with %q, pending %v", last.Policy.Model.Name, sv.Status().Pending)
	}
	if !strings.Contains(log.String(), "could not take it") {
		t.Errorf("the log does not say why: %s", log.String())
	}
	before := len(st.segments)
	if err := sv.Send(ctx, "third"); err != nil || len(st.segments) != before {
		t.Errorf("the set-aside policy was tried again: %d segments, %v", len(st.segments), err)
	}
}

// TestAPolicyInTheShareIsRefused: an agent is not started on a policy file
// in its own share folder, which it writes.
func TestAPolicyInTheShareIsRefused(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	a := assistantAgent(t)
	c, _ := LoadConfig(home, a.Name)
	share, err := ShareFolder(a, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(share, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(share, "policy.yaml")
	if err := policy.Save(path, policy.Default()); err != nil {
		t.Fatal(err)
	}
	_, err = Start(context.Background(), Options{Home: home, Agent: a, Client: ClientCLI, Policy: policy.Default(), PolicyFile: &policy.File{Path: path}, Docker: &dockerx.Fake{}, StartSegment: (&starter{}).start})
	if err == nil || !strings.Contains(err.Error(), "share folder") {
		t.Fatalf("a policy in the share: %v", err)
	}
}

// syncBuffer is a log a supervisor writes from its goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
