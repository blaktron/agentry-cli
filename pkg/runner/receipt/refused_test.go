package receipt

import (
	"errors"
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
}
