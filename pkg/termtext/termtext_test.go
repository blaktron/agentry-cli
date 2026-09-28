package termtext

import (
	"strings"
	"testing"
)

// TestSafeReplacesHiddenCharacters: a control character but a newline or a
// tab, a C1 control or a format character (a bidi override, a zero-width
// space) is shown as "?", so what a terminal shows is what the text says.
func TestSafeReplacesHiddenCharacters(t *testing.T) {
	if got, want := Safe("a\x1bb\u0085c\u202Ed\u200Be\x7f\n\tf"), "a?b?c?d?e?\n\tf"; got != want {
		t.Fatalf("Safe = %q, want %q", got, want)
	}
}

// TestQuoteSetsEveryLineBehindTheGutter: every line, a blank one included,
// starts with the gutter, so none can start where the CLI's own lines do.
func TestQuoteSetsEveryLineBehindTheGutter(t *testing.T) {
	got := Quote("\none\n\n  - two\x1b[31m\n")
	if want := "  | one\n  |\n  |   - two?[31m"; got != want {
		t.Fatalf("Quote = %q, want %q", got, want)
	}
	for _, l := range strings.Split(got, "\n") {
		if !strings.HasPrefix(l, strings.TrimRight(Gutter, " ")) || !strings.HasPrefix(l, "  |") {
			t.Errorf("line %q is not behind the gutter", l)
		}
	}
}
