// TTYPrompter tests over in-memory streams: prompt text and answers ride
// Err/In so the flows stay pipeline-clean (§R8), one buffered reader must
// survive across questions, and AskSecret must WARN when echo cannot be
// disabled rather than silently exposing input. The disableEcho happy path
// needs a real controlling terminal, so tests adapt to whichever world
// they run in instead of flaking in one of them.
package setup

import (
	"strings"
	"testing"
)

func TestTTYPrompterAsk(t *testing.T) {
	t.Run("answer wins over default", func(t *testing.T) {
		var errOut strings.Builder
		p := &TTYPrompter{In: strings.NewReader("  custom  \n"), Err: &errOut}
		got, err := p.Ask("Model", "claude-sonnet-4-5")
		if err != nil || got != "custom" {
			t.Fatalf("Ask = (%q, %v)", got, err)
		}
		if !strings.Contains(errOut.String(), "Model [claude-sonnet-4-5]: ") {
			t.Errorf("prompt text = %q, must show the default", errOut.String())
		}
	})
	t.Run("empty answer keeps the default", func(t *testing.T) {
		p := &TTYPrompter{In: strings.NewReader("\n"), Err: &strings.Builder{}}
		got, err := p.Ask("Model", "the-default")
		if err != nil || got != "the-default" {
			t.Fatalf("Ask = (%q, %v)", got, err)
		}
	})
	t.Run("no default shows a bare prompt", func(t *testing.T) {
		var errOut strings.Builder
		p := &TTYPrompter{In: strings.NewReader("x\n"), Err: &errOut}
		if _, err := p.Ask("Endpoint", ""); err != nil {
			t.Fatal(err)
		}
		if got := errOut.String(); got != "Endpoint: " {
			t.Errorf("prompt text = %q", got)
		}
	})
	t.Run("answer without trailing newline still counts", func(t *testing.T) {
		p := &TTYPrompter{In: strings.NewReader("eof-answer"), Err: &strings.Builder{}}
		got, err := p.Ask("Q", "")
		if err != nil || got != "eof-answer" {
			t.Fatalf("Ask at EOF = (%q, %v)", got, err)
		}
	})
	t.Run("empty stream is an error", func(t *testing.T) {
		p := &TTYPrompter{In: strings.NewReader(""), Err: &strings.Builder{}}
		if _, err := p.Ask("Q", "def"); err == nil || !strings.Contains(err.Error(), "read answer") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestTTYPrompterConfirm(t *testing.T) {
	tests := []struct {
		name  string
		input string
		def   bool
		want  bool
		hint  string
	}{
		{"y", "y\n", false, true, "y/N"},
		{"yes", "yes\n", false, true, "y/N"},
		{"uppercase Y", "Y\n", false, true, "y/N"},
		{"n", "n\n", true, false, "Y/n"},
		{"no", "NO\n", true, false, "Y/n"},
		{"empty takes default true", "\n", true, true, "Y/n"},
		{"empty takes default false", "\n", false, false, "y/N"},
		{"garbage takes default", "wat\n", true, true, "Y/n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var errOut strings.Builder
			p := &TTYPrompter{In: strings.NewReader(tc.input), Err: &errOut}
			got, err := p.Confirm("Keep this profile?", tc.def)
			if err != nil || got != tc.want {
				t.Fatalf("Confirm = (%v, %v), want %v", got, err, tc.want)
			}
			if !strings.Contains(errOut.String(), "["+tc.hint+"]") {
				t.Errorf("prompt %q must carry the %s hint", errOut.String(), tc.hint)
			}
		})
	}

	t.Run("read failure returns the default and the error", func(t *testing.T) {
		p := &TTYPrompter{In: strings.NewReader(""), Err: &strings.Builder{}}
		got, err := p.Confirm("Q?", true)
		if err == nil || got != true {
			t.Fatalf("Confirm on EOF = (%v, %v)", got, err)
		}
	})
}

func TestTTYPrompterAskSecret(t *testing.T) {
	// Whether echo can be disabled depends on the environment (a real
	// /dev/tty under `go test` in a terminal, none under CI). Probe once,
	// restoring immediately, and assert the branch this environment takes.
	restore, canDisable := disableEcho()
	if restore != nil {
		restore()
	}

	var errOut strings.Builder
	p := &TTYPrompter{In: strings.NewReader("  hunter2  \n"), Err: &errOut}
	got, err := p.AskSecret("Value for API_TOKEN")
	if err != nil || got != "hunter2" {
		t.Fatalf("AskSecret = (%q, %v)", got, err)
	}
	if !strings.Contains(errOut.String(), "Value for API_TOKEN: ") {
		t.Errorf("prompt text = %q", errOut.String())
	}
	if canDisable {
		if strings.Contains(errOut.String(), "input will be visible") {
			t.Error("warned about visible input although echo was disabled")
		}
	} else {
		// The honest-fallback rule: no tty means the operator is TOLD the
		// value will echo, before they type it.
		if !strings.Contains(errOut.String(), "(warning: input will be visible) ") {
			t.Errorf("no-tty AskSecret must warn; Err got %q", errOut.String())
		}
	}

	t.Run("empty stream is an error", func(t *testing.T) {
		p := &TTYPrompter{In: strings.NewReader(""), Err: &strings.Builder{}}
		if _, err := p.AskSecret("S"); err == nil || !strings.Contains(err.Error(), "read secret") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("empty answer means leave unset", func(t *testing.T) {
		p := &TTYPrompter{In: strings.NewReader("\n"), Err: &strings.Builder{}}
		got, err := p.AskSecret("S")
		if err != nil || got != "" {
			t.Fatalf("AskSecret = (%q, %v), want empty and no error", got, err)
		}
	})
}

func TestTTYPrompterSay(t *testing.T) {
	var errOut strings.Builder
	p := &TTYPrompter{In: strings.NewReader(""), Err: &errOut}
	p.Say("profile will be kept")
	if got := errOut.String(); got != "profile will be kept\n" {
		t.Errorf("Say wrote %q", got)
	}
}

// TestTTYPrompterReaderSurvivesAcrossQuestions pins the buf() contract:
// one bufio.Reader wraps In once, so answers queued in a single stream
// feed consecutive questions instead of being swallowed by re-wrapping.
func TestTTYPrompterReaderSurvivesAcrossQuestions(t *testing.T) {
	var errOut strings.Builder
	p := &TTYPrompter{In: strings.NewReader("first\nsecret-two\ny\n"), Err: &errOut}
	if got, err := p.Ask("Q1", ""); err != nil || got != "first" {
		t.Fatalf("Q1 = (%q, %v)", got, err)
	}
	if got, err := p.AskSecret("Q2"); err != nil || got != "secret-two" {
		t.Fatalf("Q2 = (%q, %v)", got, err)
	}
	if got, err := p.Confirm("Q3", false); err != nil || !got {
		t.Fatalf("Q3 = (%v, %v)", got, err)
	}
}
