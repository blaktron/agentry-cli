package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// The line a driver parses. Its shape is a contract with the hosted run
// worker, which meters a live run from it, so the format is pinned here
// rather than left to whoever edits the narration next.
func TestUsageNarrationLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		u    receipt.Usage
		want string
	}{
		{
			name: "a harness that splits input and output",
			u:    receipt.Usage{InputTokens: 6186, OutputTokens: 2153, Turns: 2},
			want: "usage: input=6186 output=2153 total=8339 turns=2",
		},
		{
			name: "a harness that reports only context tokens",
			u:    receipt.Usage{TotalTokens: 350, Turns: 1},
			want: "usage: input=0 output=0 total=350 turns=1",
		},
		{
			name: "nothing reported yet",
			u:    receipt.Usage{},
			want: "usage: input=0 output=0 total=0 turns=0",
		},
	} {
		if got := usageNarration(tc.u); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// Narration is on for the machine reader and off for the person: the run
// that carries the envelope on stdout is the one a driver is metering.
func TestUsageReporterFollowsTheOutputFormat(t *testing.T) {
	if r := usageReporter("text", &bytes.Buffer{}); r != nil {
		t.Error("a text run narrates usage; it would be noise to a reader")
	}
	if r := usageReporter("", &bytes.Buffer{}); r != nil {
		t.Error("an unset format narrates usage")
	}
	var buf bytes.Buffer
	r := usageReporter("json", &buf)
	if r == nil {
		t.Fatal("a json run does not narrate usage, so nothing can meter it live")
	}
	r(receipt.Usage{InputTokens: 12, OutputTokens: 3, Turns: 1})
	r(receipt.Usage{InputTokens: 30, OutputTokens: 9, Turns: 2})
	got := buf.String()
	want := "usage: input=12 output=3 total=15 turns=1\nusage: input=30 output=9 total=39 turns=2\n"
	if got != want {
		t.Errorf("narrated lines:\n got %q\nwant %q", got, want)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Error("a line a driver reads must end in a newline")
	}
}
