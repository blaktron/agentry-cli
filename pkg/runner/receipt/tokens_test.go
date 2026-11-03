package receipt

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestNoReceiptFieldIsNamedInCredits: the envelope is the CLI's record of a run
// on the person's own model provider, and it states the run's usage in tokens
// and dollars. No field of it, at any depth, is named for credits: those are
// what a hosted run reports, and the registry works them out from the token
// counts here. The one exception is inference.credits, the registry's own
// figure for a run paid with Agentry credits, which the CLI records as the
// registry answered and never works out. (The files a run writes beside it
// are read by the engine's TestALocalRunsOutputIsInTokens.)
func TestNoReceiptFieldIsNamedInCredits(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(path string, ty reflect.Type)
	walk = func(path string, ty reflect.Type) {
		for ty.Kind() == reflect.Pointer || ty.Kind() == reflect.Slice || ty.Kind() == reflect.Array || ty.Kind() == reflect.Map {
			ty = ty.Elem()
		}
		if ty.Kind() != reflect.Struct || seen[ty] {
			return
		}
		seen[ty] = true
		for i := range ty.NumField() {
			f := ty.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" {
				name = f.Name
			}
			if path+"."+name == "envelope.inference.credits" {
				continue
			}
			if strings.Contains(strings.ToLower(name), "credit") || strings.Contains(strings.ToLower(f.Name), "credit") {
				t.Errorf("%s.%s is named in credits", path, name)
			}
			walk(path+"."+name, f.Type)
		}
	}
	walk("envelope", reflect.TypeOf(Envelope{}))

	raw, err := fullEnvelope().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if string(env.Usage["inputTokens"]) != "100" || string(env.Usage["outputTokens"]) != "40" || string(env.Usage["costUsd"]) != "0.05" {
		t.Fatalf("the envelope's usage is %s", raw)
	}
	if strings.Contains(strings.ToLower(string(raw)), "credit") {
		t.Fatalf("the envelope is worded in credits:\n%s", raw)
	}
}

// A run's token total is the total its harness reported when it reported
// one, and the input and output counts added together otherwise.
func TestUsageTotalIsTheReportedTotalOrTheSum(t *testing.T) {
	cases := []struct {
		name  string
		usage Usage
		want  int64
	}{
		{"nothing counted", Usage{}, 0},
		{"the sum when no total was reported", Usage{InputTokens: 100, OutputTokens: 40}, 140},
		{"input alone", Usage{InputTokens: 100}, 100},
		{"output alone", Usage{OutputTokens: 40}, 40},
		{"the reported total wins over the sum", Usage{InputTokens: 100, OutputTokens: 40, TotalTokens: 500}, 500},
		{"a reported total of one", Usage{InputTokens: 100, OutputTokens: 40, TotalTokens: 1}, 1},
		{"a negative total is not a reported one", Usage{InputTokens: 100, OutputTokens: 40, TotalTokens: -3}, 140},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.usage.Total(); got != c.want {
				t.Fatalf("Total() = %d, want %d", got, c.want)
			}
		})
	}
}
