package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/modelfacts"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/harness/proto"
)

func agentryHarnessInput() Input {
	return Input{
		Mode:         manifest.ModeAutonomous,
		Instruction:  "You are an agent.",
		Model:        policy.Model{Provider: policy.ProviderAnthropic, Name: "claude-x", APIKeyEnv: "MY_ANTHROPIC_KEY", ContextWindow: 200000},
		HarnessImage: "agentry-runner:1.2.3",
		GatewayURL:   spec.GatewayURL(),
	}
}

func TestAgentryHarnessCompilesAConfigurationItsHarnessReads(t *testing.T) {
	r, err := (&agentryHarnessAdapter{}).Compile(agentryHarnessInput())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := proto.DecodeConfig(bytes.NewReader(r.Files["harness.json"]))
	if err != nil {
		t.Fatalf("the harness would refuse what the adapter wrote: %v", err)
	}
	want := proto.Config{
		Version:    proto.Version,
		Model:      proto.Model{API: proto.APIAnthropic, Name: "claude-x", KeyEnv: "AGENTRY_MODEL_KEY", MaxOutputTokens: 32000, ContextWindow: 200000},
		System:     "You are an agent.",
		Gateway:    proto.Gateway{URL: spec.GatewayURL(), TokenEnv: spec.GatewayTokenEnv},
		SessionDir: spec.SessionMount,
		// The page a 200,000-token window is given, the tools server's own.
		Results:          &proto.Results{MaxBytes: 48 << 10},
		Scrub:            &proto.Scrub{Unchanged: []string{"read_input", "search_inputs"}},
		Loop:             &proto.Loop{Repeats: 5, CycleRepeats: 3, MaxPeriod: 4},
		LengthRecoveries: 2,
		ColdClear:        &proto.ColdClear{AfterSeconds: 300, KeepTurns: 2, KeepBrowser: 2},
		Compaction:       &proto.Compaction{Threshold: 0.8, Checkpoint: checkpointText, Notes: spec.ChecklistForHarness},
		Retry:            &proto.Retry{Attempts: 3, BaseSeconds: 2, MaxSeconds: 60, IdleSeconds: 300},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("configuration %+v\nwant          %+v", cfg, want)
	}
	if len(r.Files) != 1 {
		t.Errorf("plan files: %d, want the configuration alone", len(r.Files))
	}

	a := r.Agent
	if a.Image != "agentry-runner:1.2.3" || a.User != "65534:65534" || !a.ReadOnlyRoot || !a.StdinTask {
		t.Errorf("agent container: %+v", a)
	}
	if got := strings.Join(a.Argv, " "); got != "harness run --config /plan/harness.json --conversation "+spec.PHConversation {
		t.Errorf("argv: %s", got)
	}
	var key spec.EnvVar
	for _, v := range a.Env {
		if v.Name == "AGENTRY_MODEL_KEY" {
			key = v
		}
	}
	if key.FromHostEnv != "MY_ANTHROPIC_KEY" || key.Value != "" || key.ExpectedKind == nil || key.ExpectedKind.Provider != policy.ProviderAnthropic {
		t.Errorf("the key's variable: %+v", key)
	}
	writable := 0
	for _, m := range a.Mounts {
		if !m.ReadOnly {
			writable++
			if m.Target != spec.SessionMount {
				t.Errorf("a writable mount at %s", m.Target)
			}
		}
	}
	if writable != 1 {
		t.Errorf("%d writable mounts, want the conversation's record alone", writable)
	}
	if r.Resume == nil || r.Resume.StoreOwner != a.User {
		t.Errorf("the record is not given to the user the harness runs as: %+v", r.Resume)
	}
	if r.Session != nil || r.ResultKind != "text" || r.Model != "anthropic/claude-x" {
		t.Errorf("result: session %v, kind %q, model %q", r.Session, r.ResultKind, r.Model)
	}
}

func TestAgentryHarnessTakesThePolicysOutputLimitAndASchema(t *testing.T) {
	in := agentryHarnessInput()
	in.Model.MaxOutputTokens = 4096
	in.Model.BaseURL = "https://gateway.example/anthropic/"
	in.SchemaJSON = []byte(`{"type":"object"}`)
	r, err := (&agentryHarnessAdapter{}).Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := proto.DecodeConfig(bytes.NewReader(r.Files["harness.json"]))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.MaxOutputTokens != 4096 || cfg.Model.BaseURL != "https://gateway.example/anthropic" {
		t.Errorf("model: %+v", cfg.Model)
	}
	if r.ResultKind != "json" || !strings.Contains(cfg.System, `{"type":"object"}`) {
		t.Errorf("a schema: kind %q, system %q", r.ResultKind, cfg.System)
	}
}

func TestAgentryHarnessRefusesWhatItCannotRun(t *testing.T) {
	cases := map[string]func(*Input){
		"interactive": func(in *Input) { in.Mode = manifest.ModeInteractive },
		"a server of the person's own with no address": func(in *Input) { in.Model.Provider = "custom" },
		"the registry's models with no address":        func(in *Input) { in.Model.Provider = policy.ProviderAgentry },
		"a subscription token":                         func(in *Input) { in.Model.APIKeyKind = policy.KeyKindSubscription },
		"a provider it does not reach":                 func(in *Input) { in.Model.Provider = "amazon-bedrock" },
	}
	for name, edit := range cases {
		in := agentryHarnessInput()
		edit(&in)
		var refusal *Refusal
		if _, err := (&agentryHarnessAdapter{}).Compile(in); !errors.As(err, &refusal) {
			t.Errorf("%s: not refused: %v", name, err)
		}
	}
	in := agentryHarnessInput()
	in.Mode = "sideways"
	var refusal *Refusal
	if _, err := (&agentryHarnessAdapter{}).Compile(in); err == nil || errors.As(err, &refusal) {
		t.Errorf("an unknown mode: %v", err)
	}
}

func TestAgentryHarnessReachesAServerOfThePersonsOwn(t *testing.T) {
	in := agentryHarnessInput()
	in.Model = policy.Model{Provider: "custom", Name: "Qwen3.6-35B-A3B", BaseURL: "http://agentry-host.internal:1234/v1/", APIKeyEnv: "LOCAL_KEY", ContextWindow: 65536}
	r, err := (&agentryHarnessAdapter{}).Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := proto.DecodeConfig(bytes.NewReader(r.Files["harness.json"]))
	if err != nil {
		t.Fatalf("the harness would refuse what the adapter wrote: %v", err)
	}
	m := cfg.Model
	if m.API != proto.APIChat || m.BaseURL != "http://agentry-host.internal:1234/v1" || m.KeyEnv != "AGENTRY_MODEL_KEY" || m.ContextWindow != 65536 {
		t.Errorf("model: %+v", m)
	}
	// The family's profile, the one every container harness is given.
	sampling, opts := modelSettings(in.Model)
	if m.Sampling == nil || m.Sampling.Temperature == nil || *m.Sampling.Temperature != *sampling.Temperature || int64(m.MaxOutputTokens) != sampling.MaxTokens {
		t.Errorf("sampling %+v and limit %d, want the profile's %+v", m.Sampling, m.MaxOutputTokens, sampling)
	}
	if opts.ReplayReasoning != nil && m.ReplayReasoning != *opts.ReplayReasoning {
		t.Errorf("replayReasoning %v, want the profile's %v", m.ReplayReasoning, *opts.ReplayReasoning)
	}
	if opts.ExtraBody != nil && !strings.Contains(string(m.ExtraBody), "chat_template_kwargs") {
		t.Errorf("extra body %s, want the profile's template arguments", m.ExtraBody)
	}
	var key spec.EnvVar
	for _, v := range r.Agent.Env {
		if v.Name == "AGENTRY_MODEL_KEY" {
			key = v
		}
	}
	if key.FromHostEnv != "LOCAL_KEY" || key.ExpectedKind != nil {
		t.Errorf("the key's variable: %+v", key)
	}

	// The person's own keys win over the profile's.
	temp := 0.11
	in.Model.Sampling = &policy.Sampling{Temperature: &temp}
	in.Model.MaxOutputTokens = 1234
	r, _ = (&agentryHarnessAdapter{}).Compile(in)
	cfg, _ = proto.DecodeConfig(bytes.NewReader(r.Files["harness.json"]))
	if *cfg.Model.Sampling.Temperature != 0.11 || cfg.Model.MaxOutputTokens != 1234 {
		t.Errorf("the policy's own sampling and limit: %+v", cfg.Model)
	}
}

func TestAgentryHarnessRoutersAndCredits(t *testing.T) {
	for provider, want := range map[string]string{
		policy.ProviderOpenRouter:  "https://openrouter.ai/api/v1",
		policy.ProviderHuggingFace: "https://router.huggingface.co/v1",
		policy.ProviderPoe:         "https://api.poe.com/v1",
		policy.ProviderVultr:       "https://api.vultrinference.com/v1",
	} {
		in := agentryHarnessInput()
		in.Model = policy.Model{Provider: provider, Name: "some/model", APIKeyEnv: "ROUTER_KEY"}
		r, err := (&agentryHarnessAdapter{}).Compile(in)
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		cfg, err := proto.DecodeConfig(bytes.NewReader(r.Files["harness.json"]))
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		if cfg.Model.API != proto.APIChat || cfg.Model.BaseURL != want || cfg.Model.Sampling != nil || cfg.Model.MaxOutputTokens != 0 {
			t.Errorf("%s: %+v", provider, cfg.Model)
		}
		if r.Model != provider+"/some/model" {
			t.Errorf("%s: the receipt's model is %q", provider, r.Model)
		}
	}

	// The registry's own models: its address from the policy, the run's
	// credits session as the key, never a variable of the person's.
	in := agentryHarnessInput()
	in.Model = policy.Model{Provider: policy.ProviderAgentry, Name: "a-model", BaseURL: "https://registry.example/api/v1/x/inference/v1", APIKeyEnv: "IGNORED"}
	r, err := (&agentryHarnessAdapter{}).Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range r.Agent.Env {
		if v.FromHostEnv != "" || v.FromKeyring != "" {
			t.Errorf("a credit-paid run is given a variable of the person's: %+v", v)
		}
		if v.Name == "AGENTRY_MODEL_KEY" && !v.FromInference {
			t.Errorf("the key is not the run's credits session: %+v", v)
		}
	}
}

func TestAgentryHarnessReachesOpenAIsOwnAPI(t *testing.T) {
	in := agentryHarnessInput()
	in.Model = policy.Model{Provider: policy.ProviderOpenAI, Name: "gpt-x", APIKeyEnv: "MY_OPENAI_KEY", MaxOutputTokens: 9000}
	r, err := (&agentryHarnessAdapter{}).Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := proto.DecodeConfig(bytes.NewReader(r.Files["harness.json"]))
	if err != nil {
		t.Fatalf("the harness would refuse what the adapter wrote: %v", err)
	}
	if m := cfg.Model; m.API != proto.APIResponses || m.BaseURL != "" || m.KeyEnv != "AGENTRY_MODEL_KEY" || m.MaxOutputTokens != 9000 || m.Sampling != nil {
		t.Errorf("model: %+v", m)
	}
	found := false
	for _, v := range r.Agent.Env {
		if v.Name == "AGENTRY_MODEL_KEY" && v.FromHostEnv == "MY_OPENAI_KEY" {
			found = true
		}
	}
	if !found {
		t.Errorf("the key's variable: %+v", r.Agent.Env)
	}
}

func TestAgentryHarnessReachesGemini(t *testing.T) {
	in := agentryHarnessInput()
	in.Model = policy.Model{Provider: "google", Name: "gemini-x", APIKeyEnv: "GEMINI_API_KEY"}
	r, err := (&agentryHarnessAdapter{}).Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := proto.DecodeConfig(bytes.NewReader(r.Files["harness.json"]))
	if err != nil {
		t.Fatalf("the harness would refuse what the adapter wrote: %v", err)
	}
	if m := cfg.Model; m.API != proto.APIGemini || m.BaseURL != "" || m.KeyEnv != "AGENTRY_MODEL_KEY" || m.Sampling != nil {
		t.Errorf("model: %+v", m)
	}
	if r.Model != "google/gemini-x" {
		t.Errorf("the receipt's model is %q", r.Model)
	}
}

func TestAgentryHarnessFillsWhatTheCatalogueKnows(t *testing.T) {
	const id = "claude-sonnet-5-5"
	facts, ok := modelfacts.Lookup(policy.ProviderAnthropic, id)
	if !ok || !facts.Priced || facts.ContextWindow <= 0 || facts.MaxOutput <= 0 {
		t.Fatalf("the catalogue no longer holds %s with a window, an output limit and prices: %+v", id, facts)
	}
	compile := func(m policy.Model) proto.Model {
		t.Helper()
		in := agentryHarnessInput()
		in.Model = m
		r, err := (&agentryHarnessAdapter{}).Compile(in)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := proto.DecodeConfig(bytes.NewReader(r.Files["harness.json"]))
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Model
	}

	m := compile(policy.Model{Provider: policy.ProviderAnthropic, Name: id, APIKeyEnv: "K"})
	if int64(m.ContextWindow) != facts.ContextWindow || int64(m.MaxOutputTokens) != facts.MaxOutput {
		t.Errorf("window %d and output limit %d, want the catalogue's %d and %d", m.ContextWindow, m.MaxOutputTokens, facts.ContextWindow, facts.MaxOutput)
	}
	if m.Prices == nil || *m.Prices != (proto.Prices{Input: facts.Input, CacheRead: facts.CacheRead, CacheWrite: facts.CacheWrite, Output: facts.Output}) {
		t.Errorf("prices %+v, want the catalogue's %+v", m.Prices, facts)
	}

	// What the policy says wins over the catalogue.
	m = compile(policy.Model{Provider: policy.ProviderAnthropic, Name: id, APIKeyEnv: "K", ContextWindow: 200000, MaxOutputTokens: 4096})
	if m.ContextWindow != 200000 || m.MaxOutputTokens != 4096 || m.Prices == nil {
		t.Errorf("the policy's own window and limit: %+v", m)
	}

	// A model the catalogue does not hold has no facts, and none are made
	// up; neither has a server of the person's own, whatever its model is
	// called, nor the registry's endpoint.
	for name, pm := range map[string]policy.Model{
		"unknown":        {Provider: policy.ProviderAnthropic, Name: "claude-not-yet", APIKeyEnv: "K"},
		"own server":     {Provider: "custom", Name: id, BaseURL: "http://agentry-host.internal:1234/v1", APIKeyEnv: "K"},
		"openai at base": {Provider: policy.ProviderOpenAI, Name: "gpt-4o-mini", BaseURL: "http://agentry-host.internal:1234/v1", APIKeyEnv: "K"},
		"registry":       {Provider: policy.ProviderAgentry, Name: id, BaseURL: "https://registry.example/api/v1/x/inference/v1"},
	} {
		if m := compile(pm); m.Prices != nil || m.ContextWindow != 0 {
			t.Errorf("%s: facts were filled in: %+v", name, m)
		}
	}
}

func TestAgentryHarnessSendsNoKeyToAServerThatTakesNone(t *testing.T) {
	in := agentryHarnessInput()
	in.Model = policy.Model{Provider: "custom", Name: "local", BaseURL: "http://agentry-host.internal:11434/v1"}
	if !policy.TakesNoKey(in.Harness, in.Model) {
		t.Skip("the policy treats this model as taking a key")
	}
	r, err := (&agentryHarnessAdapter{}).Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := proto.DecodeConfig(bytes.NewReader(r.Files["harness.json"]))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.KeyEnv != "" {
		t.Errorf("keyEnv %q, want none", cfg.Model.KeyEnv)
	}
	for _, v := range r.Agent.Env {
		if v.Name == "AGENTRY_MODEL_KEY" {
			t.Errorf("a key variable for a server that takes none: %+v", v)
		}
	}
}

func TestAgentryHarnessEvents(t *testing.T) {
	a := &agentryHarnessAdapter{}
	one := func(line string) Event {
		t.Helper()
		evs := a.Events([]byte(line))
		if len(evs) != 1 {
			t.Fatalf("%s: %d events, want 1", line, len(evs))
		}
		if string(evs[0].Raw) != line {
			t.Errorf("the raw line was not kept: %s", evs[0].Raw)
		}
		return evs[0]
	}

	if ev := one(`{"v":1,"seq":2,"type":"text","text":"Hel"}`); ev.Kind != EventAssistantText || ev.Text != "Hel" || !ev.Delta {
		t.Errorf("text: %+v", ev)
	}
	if ev := one(`{"v":1,"seq":3,"type":"reasoning","text":"private"}`); ev.Kind != EventReasoning || ev.Text != "" {
		t.Errorf("reasoning carries no text into the run's events: %+v", ev)
	}
	if ev := one(`{"v":1,"seq":4,"type":"tool_call","callId":"c1","tool":"fs_read","arguments":{"path":"a"}}`); ev.Kind != EventToolCall || ev.Tool != "fs_read" || ev.Text != "fs_read" || ev.CallID != "c1" {
		t.Errorf("tool call: %+v", ev)
	}
	if ev := one(`{"v":1,"seq":5,"type":"tool_result","callId":"c1","tool":"fs_read","text":"no such file","failed":true}`); ev.Kind != EventToolResult || !ev.Failed || ev.Text != "no such file" || ev.CallID != "c1" || ev.Redacted {
		t.Errorf("tool result: %+v", ev)
	}
	if ev := one(`{"v":1,"seq":6,"type":"tool_result","callId":"c2","tool":"read_input","unchanged":true}`); ev.Kind != EventToolResult || !ev.Redacted || ev.Text != "" {
		t.Errorf("a result passed on unchanged: %+v", ev)
	}
	ev := one(`{"v":1,"seq":7,"type":"usage","usage":{"input":10,"cacheRead":900,"cacheWrite":40,"output":30,"contextTokens":950,"contextLimit":200000,"costUsd":0.0123}}`)
	if ev.Kind != EventUsage || ev.InputTokens != 950 || ev.OutputTokens != 30 || ev.ContextTokens != 950 || ev.ContextLimit != 200000 || ev.CostUSD != 0.0123 {
		t.Errorf("usage: %+v", ev)
	}
	if ev := one(`{"v":1,"seq":8,"type":"turn_end","reason":"answer_cut"}`); ev.Kind != EventAnswerCut {
		t.Errorf("a cut answer: %+v", ev)
	}
	if ev := one(`{"v":1,"seq":8,"type":"turn_end","reason":"max_steps"}`); ev.Kind != EventError {
		t.Errorf("the step limit: %+v", ev)
	}
	for _, line := range []string{
		`{"v":1,"seq":1,"type":"ready","tools":["fs_read"]}`,
		`{"v":1,"seq":9,"type":"turn_end","reason":"answered","text":"Hello"}`,
		`{"v":1,"seq":9,"type":"warning","code":"something"}`,
		`{"v":1,"seq":9,"type":"usage"}`,
		`{"v":2,"seq":9,"type":"a type of a later version"}`,
		`{"seq":9}`,
		`not json`,
	} {
		if ev := one(line); ev.Kind != EventOther {
			t.Errorf("%s: %+v, want other", line, ev)
		}
	}
	if evs := a.Events([]byte("  \n")); len(evs) != 0 {
		t.Errorf("a blank line: %+v", evs)
	}
}

// The engine reads an error's words to tell a server at capacity (wait and
// start again) and a passing failure (carry on from the record) from a
// failure that ends the run.
func TestAgentryHarnessErrorsSayWhatTheEngineActsOn(t *testing.T) {
	a := &agentryHarnessAdapter{}
	text := func(line string) (Event, string) {
		evs := a.Events([]byte(line))
		return evs[0], evs[0].Text
	}

	ev, s := text(`{"v":1,"seq":1,"type":"error","class":"capacity","status":529,"text":"Overloaded"}`)
	if ev.Kind != EventError || !AtCapacity(s) || !strings.Contains(s, "Overloaded") {
		t.Errorf("capacity: %+v", ev)
	}
	for _, line := range []string{
		`{"v":1,"seq":1,"type":"error","class":"rate_limit","status":429,"text":"slow down","retryAfterSeconds":20}`,
		`{"v":1,"seq":1,"type":"error","class":"server","status":500,"text":"Internal server error"}`,
		`{"v":1,"seq":1,"type":"error","class":"transport","text":"the reply's stream ended before the reply did"}`,
	} {
		ev, s := text(line)
		if _, ok := TransientModelFailure(s); ev.Kind != EventError || !ok {
			t.Errorf("%s\nis not read as a passing failure: %q", line, s)
		}
	}
	for _, line := range []string{
		`{"v":1,"seq":1,"type":"error","class":"auth","status":401,"text":"invalid x-api-key"}`,
		`{"v":1,"seq":1,"type":"error","class":"request","status":400,"text":"messages: field required"}`,
		// The harness has already tried a summary of the conversation, so
		// starting again on it would only be refused again.
		`{"v":1,"seq":1,"type":"error","class":"overflow","status":400,"text":"prompt is too long: 250001 tokens > 200000 maximum"}`,
		`{"v":1,"seq":1,"type":"error","class":"overflow","status":400,"text":"This model's maximum context length is 32768 tokens."}`,
		`{"v":1,"seq":1,"type":"error","class":"gateway","text":"tool gateway answered HTTP 401: no bearer"}`,
	} {
		ev, s := text(line)
		if _, ok := TransientModelFailure(s); ev.Kind != EventError || ok || AtCapacity(s) {
			t.Errorf("%s\nis read as a failure to wait out: %q", line, s)
		}
	}
	ev, s = text(`{"v":1,"seq":1,"type":"error","class":"credits","status":400,"text":"Your credit balance is too low to access the Anthropic API."}`)
	if ev.Kind != EventOutOfCredits || !strings.Contains(s, "credit balance is too low") {
		t.Errorf("no credits: %+v", ev)
	}
}

// What the harness does on a long run reaches the engine as the events it
// counts and acts on.
func TestAgentryHarnessLongRunEvents(t *testing.T) {
	a := &agentryHarnessAdapter{}
	one := func(line string) Event {
		t.Helper()
		evs := a.Events([]byte(line))
		if len(evs) != 1 || string(evs[0].Raw) != line {
			t.Fatalf("%s: %+v", line, evs)
		}
		return evs[0]
	}
	if ev := one(`{"v":2,"seq":5,"type":"warning","code":"loop_nudge","tool":"fs_read","count":5,"text":"5 identical rounds of calls (fs_read) in a row; the model was told to reach its goal another way."}`); ev.Kind != EventLoopNudge || !strings.Contains(ev.Text, "fs_read") {
		t.Errorf("a nudge: %+v", ev)
	}
	// What was cut is the word the receipt counts by.
	for reason, want := range map[string]string{"answer": "answer", "reasoning": "reasoning", "tool call": "tool call"} {
		if ev := one(`{"v":2,"seq":5,"type":"warning","code":"length_recovery","reason":"` + reason + `","count":1,"text":"A reply cut off…"}`); ev.Kind != EventLengthRecovery || ev.Text != want {
			t.Errorf("a recovery from a cut %s: %+v", reason, ev)
		}
	}
	if ev := one(`{"v":2,"seq":5,"type":"warning","code":"cold_clear","count":7,"reason":"cold cache","text":"7 old tool results…"}`); ev.Kind != EventResultsCleared || ev.Text != "cold cache" {
		t.Errorf("a clear: %+v", ev)
	}
	for _, reason := range []string{"threshold", "overflow"} {
		if ev := one(`{"v":2,"seq":5,"type":"warning","code":"compaction","reason":"` + reason + `","text":"The conversation was replaced…"}`); ev.Kind != EventCompaction || ev.Text != reason {
			t.Errorf("a compaction for %s: %+v", reason, ev)
		}
	}
	if ev := one(`{"v":2,"seq":5,"type":"warning","code":"retry","class":"capacity","status":529,"count":1,"text":"The model request failed…"}`); ev.Kind != EventModelRetry {
		t.Errorf("a retry: %+v", ev)
	}
	// A summary that could not be made changed nothing the engine counts.
	if ev := one(`{"v":2,"seq":5,"type":"warning","code":"compaction_failed","reason":"threshold","text":"…"}`); ev.Kind != EventOther {
		t.Errorf("a failed compaction: %+v", ev)
	}
	// The conversation's size after a summary, with no request's tokens.
	ev := one(`{"v":2,"seq":6,"type":"usage","usage":{"input":0,"cacheRead":0,"cacheWrite":0,"output":0,"contextTokens":812,"contextLimit":32768,"compacted":true,"costUsd":0.04}}`)
	if ev.Kind != EventUsage || !ev.ContextCompacted || ev.ContextTokens != 812 || ev.ContextLimit != 32768 || ev.InputTokens != 0 || ev.OutputTokens != 0 || ev.CostUSD != 0.04 {
		t.Errorf("usage after a compaction: %+v", ev)
	}
	if ev := one(`{"v":2,"seq":7,"type":"turn_ending"}`); ev.Kind != EventTurnEnding {
		t.Errorf("a turn about to end: %+v", ev)
	}
}

// The engine writes the harness's stdin through the adapter, and the harness
// reads what it wrote as the commands they are.
func TestAgentryHarnessFramesTheRunnersCommands(t *testing.T) {
	var a Adapter = &agentryHarnessAdapter{}
	g, ok := a.(Gated)
	if !ok {
		t.Fatal("the adapter's harness asks before a turn ends, and the adapter does not say so")
	}
	read := func(line []byte) proto.Command {
		t.Helper()
		if !bytes.HasSuffix(line, []byte("\n")) || bytes.Count(line, []byte("\n")) != 1 {
			t.Fatalf("not one line: %q", line)
		}
		var c proto.Command
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		return c
	}
	task := "Read <a.txt> & say\nwhat it holds.\u2028"
	if c := read(g.PromptCommand([]byte(task))); c.Type != proto.CmdPrompt || c.Text != task {
		t.Errorf("the prompt: %+v", c)
	}
	if c := read(g.EndingAnswer("Your checklist has open steps:\n- one (pending)")); c.Type != proto.CmdContinue || c.Text != "Your checklist has open steps:\n- one (pending)" {
		t.Errorf("an answer to go on: %+v", c)
	}
	if c := read(g.EndingAnswer("")); c.Type != proto.CmdAccept || c.Text != "" {
		t.Errorf("an answer to end: %+v", c)
	}
}

// The long-run settings follow the model's server and the plan.
func TestAgentryHarnessLongRunSettings(t *testing.T) {
	config := func(in Input) proto.Config {
		t.Helper()
		r, err := (&agentryHarnessAdapter{}).Compile(in)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := proto.DecodeConfig(bytes.NewReader(r.Files["harness.json"]))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	// A server of the person's own with a small window: a smaller page, an
	// earlier summary, a longer wait for a reply, and a cache taken to live
	// as long as any server's.
	own := agentryHarnessInput()
	own.Model = policy.Model{Provider: "custom", Name: "qwen3.6", BaseURL: "http://agentry-host.internal:1234/v1", ContextWindow: 32768}
	cfg := config(own)
	if cfg.Results.MaxBytes != int(PageBudget(AgentryHarnessAdapterName, own.Model).Bytes) || cfg.Results.MaxBytes >= 48<<10 {
		t.Errorf("the result limit: %d", cfg.Results.MaxBytes)
	}
	if cfg.Compaction.Threshold != 0.7 || cfg.Retry.IdleSeconds != 1800 || cfg.ColdClear.AfterSeconds != 3600 {
		t.Errorf("compaction %+v, retry %+v, clear %+v", cfg.Compaction, cfg.Retry, cfg.ColdClear)
	}
	// The plan's own tools: the ones whose results are the host's own
	// output, and the browser's.
	plan := agentryHarnessInput()
	plan.UnchangedTools = []string{"host_run"}
	plan.BrowserTools = []string{"browser_navigate", "browser_snapshot"}
	cfg = config(plan)
	if got := strings.Join(cfg.Scrub.Unchanged, ","); got != "read_input,search_inputs,host_run" {
		t.Errorf("tools passed on unchanged: %s", got)
	}
	if got := strings.Join(cfg.ColdClear.BrowserTools, ","); got != "browser_navigate,browser_snapshot" {
		t.Errorf("browser tools: %s", got)
	}
	if strings.Join(InputTools, ",") != "read_input,search_inputs" {
		t.Errorf("the shared list of input tools was changed: %v", InputTools)
	}
}

// TestAgentryHarnessProviderGoldens holds the configuration the adapter
// renders for each provider it reaches to a committed file, and each file
// to what the harness accepts. The models are ones the catalogue does not
// hold, so a refresh of the catalogue changes none of them.
func TestAgentryHarnessProviderGoldens(t *testing.T) {
	cases := map[string]policy.Model{
		"anthropic":   {Provider: policy.ProviderAnthropic, Name: "fixture-model", APIKeyEnv: "FIXTURE_API_KEY"},
		"openai":      {Provider: policy.ProviderOpenAI, Name: "fixture-model", APIKeyEnv: "FIXTURE_API_KEY"},
		"google":      {Provider: "google", Name: "fixture-model", APIKeyEnv: "FIXTURE_API_KEY"},
		"custom":      {Provider: "custom", Name: "Qwen3.6-35B-A3B", BaseURL: "http://agentry-host.internal:1234/v1", APIKeyEnv: "FIXTURE_API_KEY", ContextWindow: 65536},
		"openrouter":  {Provider: policy.ProviderOpenRouter, Name: "fixture/model", APIKeyEnv: "OPENROUTER_API_KEY"},
		"huggingface": {Provider: policy.ProviderHuggingFace, Name: "fixture/model", APIKeyEnv: "HF_TOKEN"},
		"poe":         {Provider: policy.ProviderPoe, Name: "fixture-model", APIKeyEnv: "POE_API_KEY"},
		"vultr":       {Provider: policy.ProviderVultr, Name: "fixture-model", APIKeyEnv: "VULTR_INFERENCE_API_KEY"},
		"agentry":     {Provider: policy.ProviderAgentry, Name: "fixture-model", BaseURL: "https://registry.example/api/v1/x/inference/v1"},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			in := agentryHarnessInput()
			in.Model = m
			r, err := (&agentryHarnessAdapter{}).Compile(in)
			if err != nil {
				t.Fatal(err)
			}
			got := r.Files["harness.json"]
			if _, err := proto.DecodeConfig(bytes.NewReader(got)); err != nil {
				t.Fatalf("the harness would refuse this configuration: %v", err)
			}
			path := filepath.Join("testdata", "agentry-harness", "providers", name+".json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // a test fixture
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path) //nolint:gosec // testdata
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from what the adapter renders (run with -update and read the diff):\n%s", path, got)
			}
		})
	}
}
