package engine

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
)

func TestRecoveryTakesOnlyASchemaValidTerminalObject(t *testing.T) {
	sch, err := compile.CompileOutputSchema([]byte(`{"type":"object","required":["answer"],"additionalProperties":false,"properties":{"answer":{"type":"string"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	const answer = `{"answer":"a fence inside: ` + "```" + `"}`
	for _, tc := range []struct {
		name, text string
		want       bool
		dropped    int
	}{
		{"clean", answer, true, 0},
		{"preamble", "Done.\n" + answer, true, 6},
		{"prose braces", "Use {name}.\n" + answer, true, 12},
		{"fenced", "Done.\n```json\n" + answer + "\n```\n", true, 14},
		{"unicode", "Écrit.\n" + answer, true, 8},
		{"invalid schema", `Done. {"answer":1}`, false, 0},
		{"truncated", `Done. {"answer":"x"`, false, 0},
		{"trailing text", answer + " Done.", false, 0},
		{"trailing markup", answer + "<tool_call>", false, 0},
		{"inline closing fence", answer + "```", false, 0},
		{"array", `[{"answer":"x"}]`, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped, err := RecoverJSONResult(tc.text, sch)
			if (err == nil) != tc.want {
				t.Fatalf("result %s, error %v", got, err)
			}
			if tc.want && (string(got) != answer || dropped != tc.dropped) {
				t.Fatalf("result %s, dropped %d", got, dropped)
			}
			if tc.want && dropped > 0 {
				if _, err := JSONResult(tc.text, sch); err == nil && !strings.HasPrefix(tc.text, "```") {
					t.Fatal("strict result unexpectedly accepted prose")
				}
			}
		})
	}
}
