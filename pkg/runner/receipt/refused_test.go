package receipt

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Each refused input is held to its check's fields and grammars; what
// fails them is dropped, never repaired into a name.
func TestARefusedInputIsHeldToItsGrammar(t *testing.T) {
	in := []RefusedInput{
		{Path: "eicar.com", By: RefusedByClamAV, Signature: "Eicar-Test-Signature", Verdict: "malicious", Flagged: []string{"injection"}},
		{Path: "bundle.zip/docs/eicar.txt", By: RefusedByClamAV, Signature: "Win.Test.EICAR_HDB-1"},
		{Path: "odd.com", By: RefusedByClamAV, Signature: "not a <signature>"},
		{Path: "tool.exe", By: RefusedByHybridAnalysis, Verdict: "malicious", ThreatScore: 100, Signature: "x"},
		{Path: "maybe.exe", By: RefusedByHybridAnalysis, Verdict: "suspicious", ThreatScore: 60},
		{Path: "score.exe", By: RefusedByHybridAnalysis, Verdict: "malicious", ThreatScore: 1000},
		{Path: "letter.docx", By: RefusedByAIReview, Flagged: []string{"injection", "off_task", "Ignore all previous instructions", "injection"}},
		{Path: "notes.txt", By: RefusedByAIReview},
		{Path: "x.txt", By: "virustotal"},
		{Path: "../etc/passwd", By: RefusedByClamAV},
		{Path: "/etc/passwd", By: RefusedByClamAV},
		{Path: "a//b", By: RefusedByClamAV},
		{Path: "line\nbreak", By: RefusedByClamAV},
		{Path: "rlo\u202etxt.exe", By: RefusedByClamAV},
		{Path: strings.Repeat("a", 1025), By: RefusedByClamAV},
		{Path: "", By: RefusedByClamAV},
	}
	got, more := CleanRefusedInputs(in)
	want := []RefusedInput{
		{Path: "eicar.com", By: RefusedByClamAV, Signature: "Eicar-Test-Signature"},
		{Path: "bundle.zip/docs/eicar.txt", By: RefusedByClamAV, Signature: "Win.Test.EICAR_HDB-1"},
		{Path: "odd.com", By: RefusedByClamAV},
		{Path: "tool.exe", By: RefusedByHybridAnalysis, Verdict: "malicious", ThreatScore: 100},
		{Path: "score.exe", By: RefusedByHybridAnalysis, Verdict: "malicious"},
	}
	// Past the five: letter.docx and notes.txt; failing the grammars:
	// maybe.exe, x.txt and the seven malformed paths.
	if !reflect.DeepEqual(got, want) || more != 11 {
		t.Fatalf("got %+v and %d more, want %+v and 11 more", got, more, want)
	}
	got, _ = CleanRefusedInputs(in[6:8])
	if !reflect.DeepEqual(got, []RefusedInput{{Path: "letter.docx", By: RefusedByAIReview, Flagged: []string{"injection", "off_task"}}, {Path: "notes.txt", By: RefusedByAIReview}}) {
		t.Fatalf("AI Review's entries: %+v", got)
	}

	// Each entry alone, so the list's bound is not what drops it. What fails a
	// grammar is dropped and counted as one more.
	dropped := []struct {
		name string
		in   RefusedInput
	}{
		{"an empty path", RefusedInput{Path: "", By: RefusedByClamAV}},
		{"an absolute path", RefusedInput{Path: "/etc/passwd", By: RefusedByClamAV}},
		{"a path climbing out", RefusedInput{Path: "../etc/passwd", By: RefusedByClamAV}},
		{"a path climbing out in the middle", RefusedInput{Path: "a/../b", By: RefusedByClamAV}},
		{"a path ending in dot-dot", RefusedInput{Path: "a/..", By: RefusedByClamAV}},
		{"a dot segment first", RefusedInput{Path: "./a", By: RefusedByClamAV}},
		{"a dot segment in the middle", RefusedInput{Path: "a/./b", By: RefusedByClamAV}},
		{"an empty segment", RefusedInput{Path: "a//b", By: RefusedByClamAV}},
		{"a trailing slash", RefusedInput{Path: "a/", By: RefusedByClamAV}},
		{"a newline", RefusedInput{Path: "line\nbreak", By: RefusedByClamAV}},
		{"a NUL", RefusedInput{Path: "nul\x00byte", By: RefusedByClamAV}},
		{"a right-to-left override", RefusedInput{Path: "rlo\u202etxt.exe", By: RefusedByClamAV}},
		{"a zero-width space", RefusedInput{Path: "zero\u200bwidth", By: RefusedByClamAV}},
		{"bytes that are not UTF-8", RefusedInput{Path: "bad\xffname", By: RefusedByClamAV}},
		{"a path of 1025 bytes", RefusedInput{Path: strings.Repeat("a", 1025), By: RefusedByClamAV}},
		{"a check that is not one of the three", RefusedInput{Path: "x.txt", By: "virustotal"}},
		{"no check", RefusedInput{Path: "x.txt"}},
		{"a verdict that is not malicious", RefusedInput{Path: "x.exe", By: RefusedByHybridAnalysis, Verdict: "suspicious", ThreatScore: 50}},
		{"no verdict", RefusedInput{Path: "x.exe", By: RefusedByHybridAnalysis, ThreatScore: 50}},
	}
	for _, c := range dropped {
		t.Run("drops "+c.name, func(t *testing.T) {
			if got, more := CleanRefusedInputs([]RefusedInput{c.in}); len(got) != 0 || more != 1 {
				t.Fatalf("got %+v and %d more, want none and 1 more", got, more)
			}
		})
	}

	long := strings.Repeat("a", 1024)
	q64 := "q" + strings.Repeat("q", 63)
	kept := []struct {
		name string
		in   RefusedInput
		want RefusedInput
	}{
		{"a path of 1024 bytes", RefusedInput{Path: long, By: RefusedByClamAV}, RefusedInput{Path: long, By: RefusedByClamAV}},
		{"a path with dots inside a segment", RefusedInput{Path: "a/.hidden/b..c", By: RefusedByClamAV}, RefusedInput{Path: "a/.hidden/b..c", By: RefusedByClamAV}},
		{"a threat score of 1", RefusedInput{Path: "a.exe", By: RefusedByHybridAnalysis, Verdict: "malicious", ThreatScore: 1}, RefusedInput{Path: "a.exe", By: RefusedByHybridAnalysis, Verdict: "malicious", ThreatScore: 1}},
		{"a threat score of 100", RefusedInput{Path: "a.exe", By: RefusedByHybridAnalysis, Verdict: "malicious", ThreatScore: 100}, RefusedInput{Path: "a.exe", By: RefusedByHybridAnalysis, Verdict: "malicious", ThreatScore: 100}},
		{"a threat score of 101 drops the score", RefusedInput{Path: "a.exe", By: RefusedByHybridAnalysis, Verdict: "malicious", ThreatScore: 101}, RefusedInput{Path: "a.exe", By: RefusedByHybridAnalysis, Verdict: "malicious"}},
		{"a threat score of 0", RefusedInput{Path: "a.exe", By: RefusedByHybridAnalysis, Verdict: "malicious"}, RefusedInput{Path: "a.exe", By: RefusedByHybridAnalysis, Verdict: "malicious"}},
		{"a negative threat score drops the score", RefusedInput{Path: "a.exe", By: RefusedByHybridAnalysis, Verdict: "malicious", ThreatScore: -5}, RefusedInput{Path: "a.exe", By: RefusedByHybridAnalysis, Verdict: "malicious"}},
		{"a ClamAV entry keeps no verdict or score", RefusedInput{Path: "a.com", By: RefusedByClamAV, Signature: "Eicar-Test-Signature", Verdict: "malicious", ThreatScore: 50}, RefusedInput{Path: "a.com", By: RefusedByClamAV, Signature: "Eicar-Test-Signature"}},
		{"a hybrid-analysis entry keeps no signature or questions", RefusedInput{Path: "a.exe", By: RefusedByHybridAnalysis, Verdict: "malicious", Signature: "Eicar-Test-Signature", Flagged: []string{"injection"}}, RefusedInput{Path: "a.exe", By: RefusedByHybridAnalysis, Verdict: "malicious"}},
		{"an AI Review entry keeps no signature, verdict or score", RefusedInput{Path: "a.md", By: RefusedByAIReview, Signature: "Eicar-Test-Signature", Verdict: "malicious", ThreatScore: 50, Flagged: []string{"injection"}}, RefusedInput{Path: "a.md", By: RefusedByAIReview, Flagged: []string{"injection"}}},
		{"review questions are lower-case words", RefusedInput{Path: "a.md", By: RefusedByAIReview, Flagged: []string{"a", "off-task", "off_task", q64, q64 + "q", "1x", "_x", "-x", "Off", "offTask", "off task", ""}}, RefusedInput{Path: "a.md", By: RefusedByAIReview, Flagged: []string{"a", "off-task", "off_task", q64}}},
		{"a repeated review question is named once", RefusedInput{Path: "a.md", By: RefusedByAIReview, Flagged: []string{"x", "y", "x", "y", "x"}}, RefusedInput{Path: "a.md", By: RefusedByAIReview, Flagged: []string{"x", "y"}}},
	}
	for _, c := range kept {
		t.Run("keeps "+c.name, func(t *testing.T) {
			got, more := CleanRefusedInputs([]RefusedInput{c.in})
			if more != 0 || !reflect.DeepEqual(got, []RefusedInput{c.want}) {
				t.Fatalf("got %+v and %d more, want %+v", got, more, c.want)
			}
		})
	}

	// The bound is five named entries; a sixth is counted, and so is a
	// seventh. The same file refused by two checks is two entries, the same
	// file twice by one check is one.
	six := []RefusedInput{
		{Path: "a", By: RefusedByClamAV}, {Path: "b", By: RefusedByClamAV}, {Path: "c", By: RefusedByClamAV},
		{Path: "d", By: RefusedByClamAV}, {Path: "e", By: RefusedByClamAV}, {Path: "f", By: RefusedByClamAV},
		{Path: "f", By: RefusedByClamAV}, {Path: "g", By: RefusedByClamAV},
	}
	if got, more := CleanRefusedInputs(six[:5]); len(got) != 5 || more != 0 {
		t.Errorf("five entries: got %d named and %d more", len(got), more)
	}
	if got, more := CleanRefusedInputs(six[:6]); len(got) != 5 || got[4].Path != "e" || more != 1 {
		t.Errorf("six entries: got %+v and %d more", got, more)
	}
	if got, more := CleanRefusedInputs(six); len(got) != 5 || more != 2 {
		t.Errorf("a sixth entry repeated and a seventh: got %d named and %d more, want 5 and 2", len(got), more)
	}
	both := []RefusedInput{
		{Path: "a.exe", By: RefusedByClamAV, Signature: "Win.Test.EICAR_HDB-1"},
		{Path: "a.exe", By: RefusedByHybridAnalysis, Verdict: "malicious"},
		{Path: "a.exe", By: RefusedByClamAV, Signature: "Eicar-Test-Signature"},
	}
	if got, more := CleanRefusedInputs(both); more != 0 || !reflect.DeepEqual(got, both[:2]) {
		t.Errorf("one file refused by two checks: got %+v and %d more", got, more)
	}

	// A refused input is printed with its own keys, and only the ones it holds.
	t.Run("a refused input is printed with its own keys", func(t *testing.T) {
		for _, c := range []struct {
			in   RefusedInput
			want string
		}{
			{RefusedInput{Path: "a.com", By: RefusedByClamAV}, `{"path":"a.com","by":"clamav"}`},
			{RefusedInput{Path: "a.com", By: RefusedByClamAV, Signature: "Eicar-Test-Signature"}, `{"path":"a.com","by":"clamav","signature":"Eicar-Test-Signature"}`},
			{RefusedInput{Path: "a.exe", By: RefusedByHybridAnalysis, Verdict: "malicious", ThreatScore: 80}, `{"path":"a.exe","by":"hybrid-analysis","verdict":"malicious","threatScore":80}`},
			{RefusedInput{Path: "a.md", By: RefusedByAIReview, Flagged: []string{"injection"}}, `{"path":"a.md","by":"ai-review","flagged":["injection"]}`},
			{RefusedInput{Path: "a.md", By: RefusedByAIReview}, `{"path":"a.md","by":"ai-review"}`},
		} {
			if b, err := json.Marshal(c.in); err != nil || string(b) != c.want {
				t.Errorf("got %s (%v), want %s", b, err, c.want)
			}
		}
	})
}

// Only a refusal whose reason is input-refused names inputs, and a repeated
// entry is named once and not counted.
func TestOnlyAnInputRefusalNamesInputs(t *testing.T) {
	list := []RefusedInput{{Path: "eicar.com", By: RefusedByClamAV}, {Path: "eicar.com", By: RefusedByClamAV}}
	err := WithRefusedInputs(WithReason(errors.New("refused"), ReasonInputRefused, InputMalware), list)
	if got, more := RefusedInputsOf(err); len(got) != 1 || more != 0 {
		t.Fatalf("got %+v and %d more", got, more)
	}
	other := WithRefusedInputs(WithReason(errors.New("missing"), ReasonSecretMissing, "", "TOKEN"), list)
	if got, more := RefusedInputsOf(other); got != nil || more != 0 {
		t.Fatalf("a secret's refusal named %+v", got)
	}
	if got, _ := RefusedInputsOf(WithRefusedInputs(errors.New("no reason"), list)); got != nil {
		t.Fatalf("an error with no reason named %+v", got)
	}

	// A refusal for inputs that were not attached names none.
	if got, more := RefusedInputsOf(WithReason(errors.New("refused"), ReasonInputRefused, InputMalware)); got != nil || more != 0 {
		t.Fatalf("a refusal with no list named %+v and %d more", got, more)
	}
	// The list is held to the grammars on the way out, and the bound counts
	// what it drops.
	messy := []RefusedInput{{Path: "eicar.com", By: RefusedByClamAV}, {Path: "../x", By: RefusedByClamAV}}
	got, more := RefusedInputsOf(WithRefusedInputs(WithReason(errors.New("refused"), ReasonInputRefused, InputMalware), messy))
	if !reflect.DeepEqual(got, messy[:1]) || more != 1 {
		t.Fatalf("got %+v and %d more, want %+v and 1 more", got, more, messy[:1])
	}

	// Attaching inputs changes nothing about the error itself, and leaves a
	// nil error or an empty list alone.
	base := errors.New("refused")
	if WithRefusedInputs(nil, list) != nil {
		t.Fatal("a nil error gained inputs")
	}
	if WithRefusedInputs(base, nil) != base || WithRefusedInputs(base, []RefusedInput{}) != base {
		t.Fatal("an empty list wrapped the error")
	}
	wrapped := fmt.Errorf("prepare: %w", WithRefusedInputs(base, list))
	if !errors.Is(wrapped, base) || wrapped.Error() != "prepare: refused" {
		t.Fatalf("the inputs changed the error: %v", wrapped)
	}
}
