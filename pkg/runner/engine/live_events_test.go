package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
)

// toolRunFake is a run whose agent calls a tool, is answered with the run's
// secret in the tool's output, and then answers.
func toolRunFake(t *testing.T) (Params, *dockerx.Fake, string) {
	t.Helper()
	p, f := fixture(t)
	happyFake(f)
	const secret = "sekret-value-1234"
	f.Foreground["-agent"].Stdout = strings.Join([]string{
		`{"type":"tool_call","tool_call":{"id":"call_1","function":{"name":"fs_read","arguments":"{}"}}}`,
		`{"type":"tool_call_response","tool_call_id":"call_1","response":"token=` + secret + `","result":{"output":"token=` + secret + `"}}`,
		`{"type":"agent_choice","message":{"content":"final answer"}}`,
		`{"type":"stream_stopped","usage":{"input_tokens":100,"output_tokens":40,"cost":0.05}}`,
	}, "\n") + "\n"
	return p, f, secret
}

// A driver following the run live is handed each normalized event in
// order: the call with its tool, the result named after the call it
// answers with the run's secret masked, the answer and the usage. The
// result is no turn and no answer: the envelope is the one the run would
// have had without it.
func TestALiveRunHandsItsEventsToTheDriverMasked(t *testing.T) {
	p, f, secret := toolRunFake(t)
	var got []adapter.Event
	p.OnEvent = func(ev adapter.Event) { got = append(got, ev) }
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var kinds []adapter.EventKind
	for _, ev := range got {
		kinds = append(kinds, ev.Kind)
		if len(ev.Raw) != 0 {
			t.Errorf("%s carries its raw line", ev.Kind)
		}
		if strings.Contains(ev.Text, secret) {
			t.Errorf("%s carries the run's secret: %q", ev.Kind, ev.Text)
		}
	}
	want := []adapter.EventKind{adapter.EventToolCall, adapter.EventToolResult, adapter.EventAssistantText, adapter.EventUsage}
	if len(kinds) != len(want) {
		t.Fatalf("events %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("events %v, want %v", kinds, want)
		}
	}
	if c := got[0]; c.Tool != "fs_read" || c.CallID != "call_1" {
		t.Errorf("call = %+v", c)
	}
	if r := got[1]; r.Tool != "fs_read" || r.CallID != "call_1" || r.Failed || r.Text != "token=[redacted]" {
		t.Errorf("result = %+v", r)
	}
	if env.Usage.Turns != 1 || string(env.Result) != `"final answer"` {
		t.Errorf("envelope: turns %d, result %s", env.Usage.Turns, env.Result)
	}
}

// A receiver that fails is switched off and said once on stderr; the run
// finishes as it would have, and its receipts keep every event.
func TestALiveEventReceiverThatFailsDoesNotEndTheRun(t *testing.T) {
	p, f, _ := toolRunFake(t)
	var narration bytes.Buffer
	p.Stderr = &narration
	calls := 0
	p.OnEvent = func(adapter.Event) {
		calls++
		panic("receiver gone")
	}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 1 {
		t.Errorf("the receiver was called %d times after it failed", calls)
	}
	if env.Status != "ok" || string(env.Result) != `"final answer"` || env.Usage.Turns != 1 {
		t.Errorf("envelope: status %s, result %s, turns %d", env.Status, env.Result, env.Usage.Turns)
	}
	if n := strings.Count(narration.String(), "live events stopped"); n != 1 {
		t.Errorf("stderr said it %d times:\n%s", n, narration.String())
	}
}

// A tool's long output is masked before it is cut, so a secret that the
// cut would split leaves no prefix of itself; a secret the model streams
// over two chunks is masked in the message they join into.
func TestALiveRunMasksBeforeItCutsAndAcrossChunks(t *testing.T) {
	p, f, secret := toolRunFake(t)
	long := strings.Repeat("a", adapter.ToolExcerptMax-4) + secret + strings.Repeat("b", 100)
	half := len(secret) / 2
	f.Foreground["-agent"].Stdout = strings.Join([]string{
		`{"type":"tool_call","tool_call":{"id":"call_1","function":{"name":"fs_read","arguments":"{}"}}}`,
		`{"type":"tool_call_response","tool_call_id":"call_1","response":"x","result":{"output":"` + long + `"}}`,
		`{"type":"agent_choice","content":"the token ` + secret[:half] + `"}`,
		`{"type":"agent_choice","content":"` + secret[half:] + ` is set"}`,
		`{"type":"stream_stopped","usage":{"input_tokens":100,"output_tokens":40,"cost":0.05}}`,
	}, "\n") + "\n"
	var got []adapter.Event
	p.OnEvent = func(ev adapter.Event) { got = append(got, ev) }
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var result, text *adapter.Event
	for i := range got {
		switch got[i].Kind {
		case adapter.EventToolResult:
			result = &got[i]
		case adapter.EventAssistantText:
			if text != nil {
				t.Errorf("the message arrived in pieces: %q, then %q", text.Text, got[i].Text)
			}
			text = &got[i]
		}
	}
	if result == nil || text == nil {
		t.Fatalf("events %+v", got)
	}
	for i := 4; i <= len(secret); i++ {
		if strings.Contains(result.Text, secret[:i]) {
			t.Fatalf("the cut left %d bytes of the secret: ...%q", i, result.Text[len(result.Text)-40:])
		}
	}
	if len(result.Text) > adapter.ToolExcerptMax+32 || !strings.Contains(result.Text, "cut]") {
		t.Errorf("the result was not cut: %d bytes", len(result.Text))
	}
	if text.Text != "the token [redacted] is set" || text.Delta {
		t.Errorf("message = %+v", *text)
	}
}

// A message the harness streams last, with nothing after it, still reaches
// the driver when the stream ends.
func TestALiveRunsLastStreamedMessageArrives(t *testing.T) {
	p, f, _ := toolRunFake(t)
	f.Foreground["-agent"].Stdout = `{"type":"agent_choice","content":"final "}` + "\n" + `{"type":"agent_choice","content":"answer"}` + "\n"
	var texts []string
	p.OnEvent = func(ev adapter.Event) {
		if ev.Kind == adapter.EventAssistantText {
			texts = append(texts, ev.Text)
		}
	}
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(texts) != 1 || texts[0] != "final answer" {
		t.Errorf("messages %q", texts)
	}
}
