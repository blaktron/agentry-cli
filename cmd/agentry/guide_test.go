package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"
)

// The release guide on a terminal run (DEV-141, agentry-cli#257): printed
// once before the first question, never under --output-format json, and
// not at all for a release without one.
func TestSayGuide(t *testing.T) {
	m := &manifest.Manifest{Guide: &manifest.Guide{
		Overview: "Reads your contract.",
		Prepare:  []string{"The contract", "Your playbook"},
		Files:    "The contract, as text.",
	}}

	var out bytes.Buffer
	opts := runOpts{outputFormat: "text", guideShown: new(bool)}
	sayGuide(&out, m, opts)
	sayGuide(&out, m, opts) // the intake after the profile: not again
	got := out.String()
	for _, want := range []string{"Reads your contract.", "Have these ready:", "  - The contract", "  - Your playbook", "Files: The contract, as text."} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Count(got, "Reads your contract.") != 1 {
		t.Errorf("the guide was printed more than once:\n%s", got)
	}

	var js bytes.Buffer
	sayGuide(&js, m, runOpts{outputFormat: "json", guideShown: new(bool)})
	if js.Len() != 0 {
		t.Errorf("--output-format json printed the guide: %q", js.String())
	}

	var none bytes.Buffer
	sayGuide(&none, &manifest.Manifest{}, runOpts{outputFormat: "text", guideShown: new(bool)})
	if none.Len() != 0 {
		t.Errorf("a release without a guide printed %q", none.String())
	}
}
