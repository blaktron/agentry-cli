package main

import (
	"fmt"
	"io"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// Live usage narration. The envelope's usage arrives only when the run is
// over, which is too late for a driver that has to hold the run to a token
// cap. These two functions are the whole of the stderr half: what the line
// says, and when there is a line at all.

// usageReporter returns the callback the engine fires on every harness usage
// report, or nil when nothing should be narrated. Narration is on exactly
// when stdout carries the machine-readable envelope: that is the invocation
// a driver makes, and a person reading a text run along would only find the
// lines in the way.
func usageReporter(outputFormat string, w io.Writer) func(receipt.Usage) {
	if outputFormat != "json" {
		return nil
	}
	return func(u receipt.Usage) {
		_, _ = fmt.Fprintln(w, usageNarration(u))
	}
}

// usageNarration is the line itself, in the narration's key=value voice.
// Its shape is a contract with whoever parses it, so it is pinned by a test
// rather than left to the next edit. `total` is the figure a cap compares
// against — the harness's own total where it reports one, input plus output
// otherwise — so a reader never has to know which kind of harness ran.
func usageNarration(u receipt.Usage) string {
	return fmt.Sprintf("usage: input=%d output=%d total=%d turns=%d",
		u.InputTokens, u.OutputTokens, u.Total(), u.Turns)
}
