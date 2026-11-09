package persistent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/session/protocol"
)

// labPolicy writes a policy with a second saved model, Qwen on an endpoint
// of the person's own that takes no key, beside the default, and returns
// its path and Qwen.
func labPolicy(t *testing.T, home string) (string, policy.SavedModel) {
	t.Helper()
	path := filepath.Join(home, "policy.yaml")
	p := policy.Default()
	if err := p.AddProvider("custom-lab", policy.Provider{Name: "Lab", BaseURL: "http://192.0.2.10:8080/v1", ProxyAllow: []string{"192.0.2.10:8080"}}); err != nil {
		t.Fatal(err)
	}
	qwen, err := p.AddModel(policy.SavedModel{Name: "Qwen", Provider: "custom-lab", Model: "qwen3", ContextWindow: 32768})
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.Save(path, p); err != nil {
		t.Fatal(err)
	}
	return path, qwen
}

// usable is a ModelUsable that refuses every model while refusing is set.
type usable struct {
	mu       sync.Mutex
	refusing error
	asked    []string
}

func (u *usable) check(rm localrun.RunModel) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.asked = append(u.asked, rm.Saved.Name)
	return u.refusing
}

func (u *usable) refuse(err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.refusing = err
}

// startOnModels starts the Assistant on the policy file at path.
func startOnModels(t *testing.T, home, path string, st *starter, u *usable) (*Supervisor, error) {
	t.Helper()
	p, sha, err := policy.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Home: home, Agent: assistantAgent(t), Client: ClientCLI, Policy: p, PolicySHA: sha, PolicyFile: &policy.File{Path: path},
		Docker: &dockerx.Fake{}, StartSegment: st.start, Stderr: &syncBuffer{},
	}
	if u != nil {
		opts.ModelUsable = u.check
	}
	sv, err := Start(context.Background(), opts)
	if err == nil {
		t.Cleanup(func() { _, _ = sv.Stop(context.Background()) })
	}
	return sv, err
}

// lastSegment is the newest segment st started, and how many it started.
func lastSegment(st *starter) (localrun.Segment, int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.segments[len(st.segments)-1], len(st.segments)
}

// segmentModel is the saved model a segment was compiled on.
func segmentModel(t *testing.T, seg localrun.Segment) policy.SavedModel {
	t.Helper()
	if seg.Policy == nil {
		t.Fatal("the segment was given no policy")
	}
	sm, ok := seg.Policy.RunSavedModel()
	if !ok {
		t.Fatalf("the segment's policy is not a saved model's view: %+v", seg.Policy.Model)
	}
	return sm
}

// An agent names the saved model it runs on: each segment is compiled from
// the policy as that model reads, and the agent names none follows the
// default. A running agent moved to another takes it at its next turn, with
// no stop and no new agreement, and is told.
func TestAnAgentRunsOnTheSavedModelItNamesAndTakesAChangeAtItsNextTurn(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	path, qwen := labPolicy(t, home)
	if _, _, err := Configure(context.Background(), home, assistantAgent(t), ClientCLI, Change{Model: &qwen.ID}); err != nil {
		t.Fatalf("naming a saved model on a stopped agent: %v", err)
	}
	if c, _ := LoadConfig(home, "assistant"); c.Model != qwen.ID {
		t.Fatalf("the config names %q", c.Model)
	}
	st := &starter{resumed: true}
	sv, err := startOnModels(t, home, path, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	seg, _ := lastSegment(st)
	if sm := segmentModel(t, seg); sm.ID != qwen.ID || seg.Policy.Model.Name != "qwen3" || seg.Policy.Model.KeyProvider() != "custom-lab" {
		t.Fatalf("the first segment ran on %+v (%+v)", sm, seg.Policy.Model)
	}
	if s := sv.Status(); s.SavedModel == nil || s.SavedModel.ID != qwen.ID || s.SavedModel.Name != "Qwen" {
		t.Fatalf("the status names %+v", s.SavedModel)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events, detach := sv.Subscribe()
	defer detach()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	def := ""
	if _, err := sv.Change(Change{Model: &def}); err != nil {
		t.Fatalf("moving a running agent to the default asked for more: %v", err)
	}
	if !sv.Status().Pending {
		t.Fatal("a changed model is not pending")
	}
	if err := sv.Send(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	seg, n := lastSegment(st)
	full, _, _ := policy.Load(path)
	if sm := segmentModel(t, seg); n != 2 || sm.ID != full.DefaultModel().ID {
		t.Fatalf("segments %d; the next ran on %+v", n, sm)
	}
	if seg.Policy.Model.Provider != "anthropic" {
		t.Fatalf("the default's segment ran on %+v", seg.Policy.Model)
	}
	select {
	case <-sv.Done():
		t.Fatal("the agent stopped for a change of model")
	default:
	}
	if !told(events, "The agent's model changed") {
		t.Error("the chat was not told the model changed")
	}

	if _, err := sv.Change(Change{Model: &qwen.ID}); err != nil {
		t.Fatal(err)
	}
	if err := sv.Send(ctx, "third"); err != nil {
		t.Fatal(err)
	}
	if seg, n = lastSegment(st); n != 3 || segmentModel(t, seg).ID != qwen.ID {
		t.Fatalf("segments %d; moved back, the next ran on %+v", n, seg.Policy.Model)
	}
}

// A turn on a saved model the computer does not have fails with one error
// naming the model and the fix, and never runs on the default or on the
// model the agent had; the next turn tries again.
func TestATurnOnAMissingSavedModelFailsAndTheNextTriesAgain(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	path, qwen := labPolicy(t, home)
	st := &starter{resumed: true}
	sv, err := startOnModels(t, home, path, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := sv.Change(Change{Model: &qwen.ID}); err != nil {
		t.Fatal(err)
	}
	editPolicy(t, path, func(p *policy.Policy) {
		if _, err := p.RemoveModel(qwen.ID, ""); err != nil {
			t.Fatal(err)
		}
	})
	for _, turn := range []string{"second", "third"} {
		err := sv.Send(ctx, turn)
		var me *ModelError
		if !errors.As(err, &me) || classOf(err) != engine.ExitPolicy {
			t.Fatalf("the %s turn on a missing model: %v", turn, err)
		}
		for _, want := range []string{qwen.ID, "agentry agent configure assistant --model", "never runs on the default"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q lacks %q", err, want)
			}
		}
		if _, n := lastSegment(st); n != 1 {
			t.Fatalf("a turn on a missing model started a segment (%d)", n)
		}
		st.mu.Lock()
		sent := st.last.sentTurns()
		st.mu.Unlock()
		if len(sent) != 1 {
			t.Fatalf("the segment of the model the agent had was sent %q", sent)
		}
	}
	select {
	case <-sv.Done():
		t.Fatal("a turn on a missing model stopped the agent")
	default:
	}
	if !sv.Status().Pending {
		t.Error("the change is not pending after its turn failed: the next turn would not try again")
	}
	// Moved to a model it has, the next turn runs there.
	def := ""
	if _, err := sv.Change(Change{Model: &def}); err != nil {
		t.Fatal(err)
	}
	if err := sv.Send(ctx, "fourth"); err != nil {
		t.Fatal(err)
	}
	if seg, n := lastSegment(st); n != 2 || seg.Policy.Model.Provider != "anthropic" {
		t.Fatalf("segments %d, on %+v", n, seg.Policy.Model)
	}
}

// A turn on a saved model whose credential cannot be used here fails by the
// check's own words and the next turn tries again; once it can be used,
// the turn runs on it.
func TestATurnOnAnUnusableSavedModelFailsUntilItCanBeUsed(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	path, qwen := labPolicy(t, home)
	st := &starter{resumed: true}
	u := &usable{}
	sv, err := startOnModels(t, home, path, st, u)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := sv.Change(Change{Model: &qwen.ID}); err != nil {
		t.Fatal(err)
	}
	u.refuse(errors.New(`the saved model "Qwen" (provider custom-lab) cannot be used: its key is not set`))
	for range 2 {
		err := sv.Send(ctx, "hello")
		if err == nil || !strings.Contains(err.Error(), `the saved model "Qwen" (provider custom-lab) cannot be used`) || classOf(err) != engine.ExitPolicy {
			t.Fatalf("a turn on an unusable model: %v", err)
		}
		if _, n := lastSegment(st); n != 1 {
			t.Fatalf("a turn on an unusable model started a segment (%d)", n)
		}
	}
	u.refuse(nil)
	if err := sv.Send(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if seg, n := lastSegment(st); n != 2 || segmentModel(t, seg).ID != qwen.ID {
		t.Fatalf("segments %d, on %+v", n, seg.Policy.Model)
	}
}

// An agent naming a saved model the computer does not have does not start,
// and the refusal names the model.
func TestAnAgentOnAMissingSavedModelDoesNotStart(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	path, _ := labPolicy(t, home)
	gone := "0badc0de"
	if _, _, err := Configure(context.Background(), home, assistantAgent(t), ClientCLI, Change{Model: &gone}); err != nil {
		t.Fatal(err)
	}
	st := &starter{resumed: true}
	_, err := startOnModels(t, home, path, st, nil)
	var me *ModelError
	if !errors.As(err, &me) || !strings.Contains(err.Error(), gone) {
		t.Fatalf("a start on a missing model: %v", err)
	}
	st.mu.Lock()
	n := len(st.segments)
	st.mu.Unlock()
	if n != 0 {
		t.Fatalf("a start on a missing model started %d segments", n)
	}
}

// A running agent is refused a saved model its policy does not have, and a
// model reference that is not a saved model's id.
func TestARunningAgentIsRefusedASavedModelItsPolicyDoesNotHave(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	path, _ := labPolicy(t, home)
	st := &starter{resumed: true}
	sv, err := startOnModels(t, home, path, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"0badc0de", "Not An Id"} {
		if _, err := sv.Change(Change{Model: &ref}); err == nil {
			t.Errorf("a running agent took the model %q", ref)
		}
	}
	if c, _ := LoadConfig(home, "assistant"); c.Model != "" {
		t.Errorf("the config names %q", c.Model)
	}
}

// A configuration naming a saved model is written at a version an older
// build refuses, rather than drop the model and run on the default; one
// that names none keeps the version it had.
func TestAConfigNamingASavedModelIsRefusedByAnOlderBuild(t *testing.T) {
	home := shortHome(t)
	version := func() string {
		b, err := os.ReadFile(filepath.Join(Dir(home, "assistant"), configFile))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "version: "); ok {
				return v
			}
		}
		return ""
	}
	if err := SaveConfig(home, "assistant", Config{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if v := version(); v != "2" {
		t.Fatalf("a config naming no model is written at version %s", v)
	}
	if err := SaveConfig(home, "assistant", Config{Enabled: true, Model: "0badc0de"}); err != nil {
		t.Fatal(err)
	}
	if v := version(); v != strconv.Itoa(savedModelVersion) || savedModelVersion <= messengerKindsVersion || ConfigVersion != savedModelVersion {
		t.Fatalf("a config naming a model is written at version %s (saved models %d, newest %d)", v, savedModelVersion, ConfigVersion)
	}
	if c, err := LoadConfig(home, "assistant"); err != nil || c.Model != "0badc0de" {
		t.Fatalf("read back %q, %v", c.Model, err)
	}
}

// classOf is err's exit class, 0 when it has none.
func classOf(err error) int {
	var ce *engine.ClassifiedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return 0
}

// A running agent moved to a saved model whose credential takes another
// harness starts its next segment on that harness, in a new conversation
// that carries the last exchange.
func TestAModelOnAnotherHarnessCarriesTheLastExchange(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	path, _ := labPolicy(t, home)
	var plan policy.SavedModel
	editPolicy(t, path, func(p *policy.Policy) {
		if err := p.AddProvider("openai", policy.Provider{APIKeyKind: policy.KeyKindChatGPTPlan, ProxyAllow: policy.SignInProxyAllow(policy.KeyKindChatGPTPlan)}); err != nil {
			t.Fatal(err)
		}
		var err error
		if plan, err = p.AddModel(policy.SavedModel{Name: "Plan", Provider: "openai", Model: "gpt-5.5"}); err != nil {
			t.Fatal(err)
		}
	})
	st := &starter{resumed: true}
	sv, err := startOnModels(t, home, path, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	before := sv.Status().Adapter
	st.mu.Lock()
	st.resumed = false
	st.mu.Unlock()
	if _, err := sv.Change(Change{Model: &plan.ID}); err != nil {
		t.Fatal(err)
	}
	if err := sv.Send(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	seg, n := lastSegment(st)
	if n != 2 || seg.Adapter == before || seg.Adapter != sv.Status().Adapter || segmentModel(t, seg).ID != plan.ID {
		t.Fatalf("segments %d; the next ran on %q (before %q)", n, seg.Adapter, before)
	}
	if seg.Persistent.ContinueID != "" {
		t.Errorf("the old harness's conversation %q was offered to the new one", seg.Persistent.ContinueID)
	}
	st.mu.Lock()
	raw := st.last.raw
	st.mu.Unlock()
	if len(raw) != 1 || !strings.Contains(raw[0], "first") || !strings.HasSuffix(raw[0], "second") {
		t.Errorf("the new harness was not handed the last exchange: %q", raw)
	}
}

// A running agent moved to a saved model no segment can start on is not put
// back on the model it was moved from: the turn fails, saying why.
func TestAnAgentMovedToAModelNoSegmentStartsOnIsNotPutBack(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	path, qwen := labPolicy(t, home)
	st := &starter{resumed: true}
	sv, err := startOnModels(t, home, path, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	refuseWith(st, func(seg localrun.Segment) error {
		if seg.Policy.Model.Name == "qwen3" {
			return errors.New("the engine could not start it")
		}
		return nil
	})
	if _, err := sv.Change(Change{Model: &qwen.ID}); err != nil {
		t.Fatal(err)
	}
	if err := sv.Send(ctx, "second"); err == nil || !strings.Contains(err.Error(), "the engine could not start it") {
		t.Fatalf("the turn on a model no segment starts on: %v", err)
	}
	if _, n := lastSegment(st); n != 1 {
		t.Fatalf("the agent was put back on the model it was moved from (%d segments)", n)
	}
}

// A chat attached through the owner socket is told when its turn fails for
// the agent's saved model: the send is refused with that one error, of the
// class a program driving the session protocol is refused with, and the
// chat's turn ends on it rather than waiting for an answer that never comes.
func TestAChatIsToldWhenItsTurnFailsForTheSavedModel(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	path, qwen := labPolicy(t, home)
	st := &starter{resumed: true}
	sv, err := startOnModels(t, home, path, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sv.Change(Change{Model: &qwen.ID}); err != nil {
		t.Fatal(err)
	}
	editPolicy(t, path, func(p *policy.Policy) {
		if _, err := p.RemoveModel(qwen.ID, ""); err != nil {
			t.Fatal(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	chat, err := Attach(ctx, home, "assistant", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = chat.End(context.Background(), false) }()
	err = chat.Send(ctx, "hello")
	if err == nil || !strings.Contains(err.Error(), qwen.ID) || !strings.Contains(err.Error(), "agentry agent configure assistant --model") {
		t.Fatalf("the chat's send on a missing model: %v", err)
	}
	var refused *protocol.RefusedError
	if !errors.As(err, &refused) || refused.Class != RefusedModel {
		t.Errorf("the send's error is not the model refusal a program is given: %#v", err)
	}
	if _, n := lastSegment(st); n != 1 {
		t.Errorf("a refused send started a segment (%d)", n)
	}
}
