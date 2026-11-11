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
		"an unknown field":          edit(func(m map[string]any) { m["toolsets"] = []string{"shell"} }),
		"an unknown field inside":   edit(func(m map[string]any) { m["model"].(map[string]any)["apiKey"] = "a value" }),
		"a second document":         edit(func(map[string]any) {}) + edit(func(map[string]any) {}),
		"an older version":          edit(func(m map[string]any) { m["version"] = 0 }),
		"a newer version":           edit(func(m map[string]any) { m["version"] = Version + 1 }),
		"an API it does not speak":  edit(func(m map[string]any) { m["model"].(map[string]any)["api"] = "other" }),
		"no model name":             edit(func(m map[string]any) { m["model"].(map[string]any)["name"] = "" }),
		"no key variable":           edit(func(m map[string]any) { m["model"].(map[string]any)["keyEnv"] = "" }),
		"no output limit":           edit(func(m map[string]any) { m["model"].(map[string]any)["maxOutputTokens"] = 0 }),
		"no gateway address":        edit(func(m map[string]any) { m["gateway"].(map[string]any)["url"] = "" }),
		"no gateway token variable": edit(func(m map[string]any) { m["gateway"].(map[string]any)["tokenEnv"] = "" }),
		"no session folder":         edit(func(m map[string]any) { m["sessionDir"] = "" }),
		"not JSON":                  "version: 1",
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
		"values": map[string][]string{
			"event.type":   {TypeReady, TypeText, TypeReasoning, TypeToolCall, TypeToolResult, TypeUsage, TypeWarning, TypeError, TypeTurnEnd},
			"event.reason": {EndAnswered, EndAnswerCut, EndError, EndMaxSteps},
			"event.class":  {ClassOverflow, ClassCapacity, ClassRateLimit, ClassCredits, ClassAuth, ClassRequest, ClassTransport, ClassServer, ClassGateway, ClassHarness},
			"model.api":    {APIAnthropic},
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
