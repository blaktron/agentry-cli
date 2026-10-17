package adapter

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A tool's output is carried whole up to the limit, and cut past it at a
// character boundary with a mark that says so.
func TestToolExcerpt(t *testing.T) {
	if got := ToolExcerpt("  short output\n"); got != "short output" {
		t.Errorf("short = %q", got)
	}
	exact := strings.Repeat("a", ToolExcerptMax)
	if got := ToolExcerpt(exact); got != exact {
		t.Errorf("an output of exactly the limit was cut")
	}
	// A three-byte character straddling the limit is dropped whole.
	long := strings.Repeat("a", ToolExcerptMax-1) + "€" + strings.Repeat("b", 100)
	got := ToolExcerpt(long)
	if !utf8.ValidString(got) {
		t.Fatalf("cut inside a character: %q", got[len(got)-20:])
	}
	if !strings.HasSuffix(got, toolExcerptCut) {
		t.Errorf("a cut excerpt is not marked: %q", got[len(got)-20:])
	}
	if body := strings.TrimSuffix(got, toolExcerptCut); body != strings.Repeat("a", ToolExcerptMax-1) {
		t.Errorf("cut body is %d bytes, want %d", len(body), ToolExcerptMax-1)
	}
}
