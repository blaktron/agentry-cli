package engine

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
)

// recordedUsage runs a harness's recorded stream (the conformance
// recordings, real runs) through the real adapter and the
// run's collector, and returns the collector together with the stream's
// lines of the given type, decoded.
func recordedUsage(t *testing.T, adapterName, lineType string) (*eventCollector, []map[string]any) {
	t.Helper()
	ad, err := adapter.Get(adapterName)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "adapter", "testdata", "conformance", adapterName, "events.ndjson")
	b, err := os.ReadFile(path) //nolint:gosec // a fixture of this repository
	if err != nil {
		t.Fatal(err)
	}
	c := &eventCollector{adapter: ad, events: io.Discard}
	c.consume(strings.NewReader(string(b)))
	var lines []map[string]any
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil && m["type"] == lineType {
			lines = append(lines, m)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("%s: the recording has no %q line", adapterName, lineType)
	}
	return c, lines
}

func num(m map[string]any, path ...string) int64 {
	var v any = m
	for _, k := range path {
		obj, ok := v.(map[string]any)
		if !ok {
			return 0
		}
		v = obj[k]
	}
	f, _ := v.(float64)
	return int64(f)
}

// docker-agent's token_usage.usage.input_tokens is the input of THAT model
// request — fresh plus cached — not a running total across requests, so
// the receipt sums it. The recording proves it event by event:
// usage.input_tokens = last_message.input_tokens +
// last_message.cached_input_tokens and usage.output_tokens =
// last_message.output_tokens. A running total could not satisfy that past
// the first event (the second would be the sum of the first two). The
// values only look monotonic because the conversation's context grows.
// Cost is the one field docker-agent reports as a running total, which is
// why the collector keeps its high-water mark instead.
func TestDockerAgentRecordedUsageIsPerRequestAndSummed(t *testing.T) {
	c, lines := recordedUsage(t, adapter.DefaultName, "token_usage")
	var in, out int64
	for i, l := range lines {
		ui, uo := num(l, "usage", "input_tokens"), num(l, "usage", "output_tokens")
		lmIn, lmCached, lmOut := num(l, "usage", "last_message", "input_tokens"), num(l, "usage", "last_message", "cached_input_tokens"), num(l, "usage", "last_message", "output_tokens")
		if ui != lmIn+lmCached || uo != lmOut {
			t.Errorf("token_usage %d: usage in/out %d/%d, last_message in+cached/out %d/%d: the per-request identity does not hold, so the summing below is wrong", i+1, ui, uo, lmIn+lmCached, lmOut)
		}
		in += ui
		out += uo
	}
	if len(lines) < 2 {
		t.Fatalf("the recording has %d token_usage events; summing needs at least two to be pinned", len(lines))
	}
	got := c.usage()
	if got.InputTokens != in || got.OutputTokens != out {
		t.Errorf("receipt usage in/out = %d/%d, want the per-request sums %d/%d", got.InputTokens, got.OutputTokens, in, out)
	}
	if last := num(lines[len(lines)-1], "usage", "input_tokens"); got.InputTokens == last {
		t.Errorf("receipt input = the last event's %d: a high-water mark, not the sum", last)
	}
}

// Claude Code (autonomous, `claude -p --output-format stream-json`) reports
// usage once, on its single result message, for the whole run; codex
// (`codex exec --json`) once, on its single turn.completed. With one usage
// event per run the collector's sum is the harness's own figure, so the
// per-turn-or-cumulative question does not arise for either autonomous
// path. The recordings pin that there is exactly one.
func TestClaudeAndCodexRecordedUsageIsTheirOneFigure(t *testing.T) {
	for _, tc := range []struct{ adapter, lineType string }{
		{adapter.ClaudeAdapterName, "result"},
		{adapter.CodexAdapterName, "turn.completed"},
	} {
		t.Run(tc.adapter, func(t *testing.T) {
			c, lines := recordedUsage(t, tc.adapter, tc.lineType)
			if len(lines) != 1 {
				t.Fatalf("%d %s lines in the recording, want the one per autonomous run", len(lines), tc.lineType)
			}
			wantIn, wantOut := num(lines[0], "usage", "input_tokens"), num(lines[0], "usage", "output_tokens")
			if got := c.usage(); got.InputTokens != wantIn || got.OutputTokens != wantOut {
				t.Errorf("receipt usage in/out = %d/%d, want the harness's %d/%d", got.InputTokens, got.OutputTokens, wantIn, wantOut)
			}
		})
	}
}
