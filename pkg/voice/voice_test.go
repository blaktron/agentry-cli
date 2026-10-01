package voice

import (
	"strings"
	"testing"
)

func TestCheckHoldsTheFormat(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		ok   bool
	}{
		{"a voice", "# My voice\n\nShort sentences. No hedging.\n", true},
		{"at the cap", strings.Repeat("a", MaxBytes), true},
		{"empty", "", false},
		{"only space", " \n\t\n", false},
		{"past the cap", strings.Repeat("a", MaxBytes+1), false},
		{"not UTF-8", "caf\xe9", false},
		{"a NUL", "a\x00b", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Check([]byte(tc.text)); (err == nil) != tc.ok {
				t.Fatalf("Check = %v, want ok %v", err, tc.ok)
			}
		})
	}
}

func TestDigestIsSha256(t *testing.T) {
	if got := Digest([]byte("abc")); got != "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("Digest = %s", got)
	}
}

func TestEverySourceHasAPhrase(t *testing.T) {
	for _, s := range []Source{SourceCreate, SourceImprove, SourceAIEdit, SourceEdit, SourceDownload} {
		if !s.Valid() || s.Phrase() == "of unknown origin" {
			t.Errorf("source %q has no phrase", s)
		}
	}
	if Source("other").Valid() {
		t.Error("an unknown source is valid")
	}
}
