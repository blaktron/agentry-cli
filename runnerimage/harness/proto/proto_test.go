package proto

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/schema.json from the Go types")

func valid() Config {
	return Config{
		Version:    Version,
		Model:      Model{API: APIAnthropic, Name: "claude-x", KeyEnv: "ANTHROPIC_API_KEY", MaxOutputTokens: 8192, ContextWindow: 200000},
		System:     "You are an agent. Say <done> & stop.",
		Gateway:    Gateway{URL: "http://gateway:8811/mcp", TokenEnv: "MCP_GATEWAY_TOKEN"},
		SessionDir: "/session",
	}
}

func TestAConfigurationGoesThroughUnchanged(t *testing.T) {
	want := valid()
	a, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := want.Encode()
	if !bytes.Equal(a, b) {
		t.Error("the same configuration rendered to different bytes")
	}
	if !strings.Contains(string(a), "Say <done> & stop.") {
		t.Errorf("the system prompt was escaped:\n%s", a)
	}
	got, err := DecodeConfig(bytes.NewReader(a))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decoded %+v\nwant    %+v", got, want)
	}
}

func TestAChatCompletionsConfiguration(t *testing.T) {
	temp, topK := 0.6, int64(20)
	want := valid()
	want.Model = Model{
		API: APIChat, Name: "qwen3.6", BaseURL: "http://agentry-host.internal:1234/v1", MaxOutputTokens: 4096,
		Sampling:        &Sampling{Temperature: &temp, TopK: &topK},
		ExtraBody:       json.RawMessage(`{"chat_template_kwargs":{"enable_thinking":true}}`),
		ReplayReasoning: true,
	}
	raw, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("a keyless Chat Completions model was refused: %v", err)
	}
	if got.Model.KeyEnv != "" || *got.Model.Sampling.Temperature != 0.6 || *got.Model.Sampling.TopK != 20 || !got.Model.ReplayReasoning {
		t.Errorf("model: %+v", got.Model)
	}
	var extra map[string]any
	if json.Unmarshal(got.Model.ExtraBody, &extra) != nil || extra["chat_template_kwargs"] == nil {
		t.Errorf("extra body: %s", got.Model.ExtraBody)
	}
}

func TestAConfigurationForALongRunGoesThroughUnchanged(t *testing.T) {
	want := valid()
	want.Results = &Results{MaxBytes: 48 << 10}
	want.Scrub = &Scrub{Unchanged: []string{"read_input", "search_inputs"}}
	want.Loop = &Loop{Repeats: 5, CycleRepeats: 3, MaxPeriod: 4}
	want.LengthRecoveries = 2
	want.ColdClear = &ColdClear{AfterSeconds: 300, KeepTurns: 2, KeepBrowser: 2, BrowserTools: []string{"browser_snapshot"}}
	want.Compaction = &Compaction{Threshold: 0.8, Checkpoint: "Keep the task.", Notes: "checklist.md in your scratch folder"}
	want.Retry = &Retry{Attempts: 3, BaseSeconds: 2, MaxSeconds: 60, IdleSeconds: 300}
	raw, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decoded %+v\nwant    %+v", got, want)
	}
	// A block left out is a thing the harness does not do, never a default.
	bare, _ := valid().Encode()
	if got, _ := DecodeConfig(bytes.NewReader(bare)); got.Results != nil || got.Scrub != nil || got.Loop != nil || got.ColdClear != nil || got.Compaction != nil || got.Retry != nil || got.LengthRecoveries != 0 {
		t.Errorf("a configuration with no block decoded with one: %+v", got)
	}
}

func TestDecodeConfigRefuses(t *testing.T) {
	edit := func(f func(m map[string]any)) string {
		raw, _ := valid().Encode()
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		f(m)
		out, _ := json.Marshal(m)
		return string(out)
	}
	cases := map[string]string{
		"an unknown field":         edit(func(m map[string]any) { m["toolsets"] = []string{"shell"} }),
		"an unknown field inside":  edit(func(m map[string]any) { m["model"].(map[string]any)["apiKey"] = "a value" }),
		"a second document":        edit(func(map[string]any) {}) + edit(func(map[string]any) {}),
		"an older version":         edit(func(m map[string]any) { m["version"] = 0 }),
		"a newer version":          edit(func(m map[string]any) { m["version"] = Version + 1 }),
		"an API it does not speak": edit(func(m map[string]any) { m["model"].(map[string]any)["api"] = "other" }),
		"no model name":            edit(func(m map[string]any) { m["model"].(map[string]any)["name"] = "" }),
		"no key variable":          edit(func(m map[string]any) { m["model"].(map[string]any)["keyEnv"] = "" }),
		"a negative price": edit(func(m map[string]any) {
			m["model"].(map[string]any)["prices"] = map[string]any{"input": 2, "cacheRead": -0.1, "cacheWrite": 2.5, "output": 10}
		}),
		"sampling for the Anthropic API": edit(func(m map[string]any) {
			m["model"].(map[string]any)["sampling"] = map[string]any{"temperature": 0.7}
		}),
		"Chat Completions with no address": edit(func(m map[string]any) { m["model"].(map[string]any)["api"] = APIChat }),
		"an extra body that is not an object": edit(func(m map[string]any) {
			mm := m["model"].(map[string]any)
			mm["api"], mm["baseUrl"], mm["extraBody"] = APIChat, "http://host/v1", []int{1}
		}),
		"no output limit":           edit(func(m map[string]any) { m["model"].(map[string]any)["maxOutputTokens"] = 0 }),
		"no gateway address":        edit(func(m map[string]any) { m["gateway"].(map[string]any)["url"] = "" }),
		"no gateway token variable": edit(func(m map[string]any) { m["gateway"].(map[string]any)["tokenEnv"] = "" }),
		"no session folder":         edit(func(m map[string]any) { m["sessionDir"] = "" }),
		"not JSON":                  "version: 1",
		"a result limit with no room for its notice": edit(func(m map[string]any) { m["results"] = map[string]any{"maxBytes": 100} }),
		"an unknown field in a block":                edit(func(m map[string]any) { m["scrub"] = map[string]any{"off": true} }),
		"a streak of one call":                       edit(func(m map[string]any) { m["loop"] = map[string]any{"repeats": 1} }),
		"sequences with no length":                   edit(func(m map[string]any) { m["loop"] = map[string]any{"repeats": 5, "cycleRepeats": 3} }),
		"negative recoveries":                        edit(func(m map[string]any) { m["lengthRecoveries"] = -1 }),
		"a clear with no lifetime": edit(func(m map[string]any) {
			m["coldClear"] = map[string]any{"afterSeconds": 0, "keepTurns": 2, "keepBrowser": 2}
		}),
		"a clear that keeps a negative count": edit(func(m map[string]any) {
			m["coldClear"] = map[string]any{"afterSeconds": 300, "keepTurns": -1, "keepBrowser": 2}
		}),
		"a summary at the whole window":  edit(func(m map[string]any) { m["compaction"] = map[string]any{"threshold": 1} }),
		"a summary at none of it":        edit(func(m map[string]any) { m["compaction"] = map[string]any{"threshold": 0} }),
		"notes of two lines":             edit(func(m map[string]any) { m["compaction"] = map[string]any{"threshold": 0.8, "notes": "a\nb"} }),
		"a longest wait below the first": edit(func(m map[string]any) { m["retry"] = map[string]any{"attempts": 3, "baseSeconds": 10, "maxSeconds": 5} }),
		"negative retries":               edit(func(m map[string]any) { m["retry"] = map[string]any{"attempts": -1, "baseSeconds": 1, "maxSeconds": 5} }),
	}
	for name, raw := range cases {
		if _, err := DecodeConfig(strings.NewReader(raw)); err == nil {
			t.Errorf("%s was accepted", name)
		} else if !strings.HasPrefix(err.Error(), "harness configuration: ") {
			t.Errorf("%s: the error does not say what it is about: %v", name, err)
		}
	}
}

// TestTheContractIsWhatTheSchemaSays holds the configuration and the event
// to a committed description of their fields, so a change to either is a
// change someone made on purpose: run the test with -update and read the
// difference.
func TestTheContractIsWhatTheSchemaSays(t *testing.T) {
	doc := map[string]any{
		"version": Version,
		"config":  describe(reflect.TypeOf(Config{})),
		"event":   describe(reflect.TypeOf(Event{})),
		"command": describe(reflect.TypeOf(Command{})),
		"values": map[string][]string{
			"command.type":                   {CmdPrompt, CmdAccept, CmdContinue, CmdOpen, CmdSteer, CmdInterrupt, CmdPermit, CmdDeny, CmdCompact},
			"event.type":                     {TypeReady, TypeText, TypeReasoning, TypeToolCall, TypeToolResult, TypeUsage, TypeWarning, TypeError, TypeTurnEnding, TypeTurnEnd, TypePermission, TypeSteered},
			"event.reason":                   {EndAnswered, EndAnswerCut, EndError, EndMaxSteps, EndInterrupted},
			"event.code":                     {WarnLoopNudge, WarnLengthRecovery, WarnColdClear, WarnCompaction, WarnCompactionFailed, WarnRetry, WarnCommandRefused},
			"warning.reason.length_recovery": {CutAnswer, CutReasoning, CutToolCall},
			"warning.reason.compaction":      {CompactionThreshold, CompactionOverflow, CompactionRequest},
			"event.class":                    {ClassOverflow, ClassCapacity, ClassRateLimit, ClassCredits, ClassAuth, ClassRequest, ClassTransport, ClassServer, ClassGateway, ClassHarness},
			"model.api":                      {APIAnthropic, APIChat, APIResponses, APIGemini},
		},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		t.Fatal(err)
	}
	const path = "testdata/schema.json"
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("the contract's types no longer match %s.\nIf the change is meant, move Version when a field changed meaning or went away, then run: go test ./runnerimage/harness/proto -update\n\ngot:\n%s", path, buf.Bytes())
	}
}

// describe lists a struct's JSON fields with their types.
func describe(t reflect.Type) any {
	switch t.Kind() {
	case reflect.Pointer:
		return describe(t.Elem())
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return "json"
		}
		return []any{describe(t.Elem())}
	case reflect.Struct:
		fields := map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				panic("field " + t.Name() + "." + f.Name + " has no JSON name")
			}
			d := describe(f.Type)
			if opts == "omitempty" {
				if s, ok := d.(string); ok {
					d = s + ", optional"
				}
			}
			fields[name] = d
		}
		return fields
	default:
		return t.Kind().String()
	}
}
