package main

import (
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/intake"
)

// A BriefAgent with no intake questions yields nil answers; the overrides
// must still be the profile's secrets, not a panic (agentry-cli#49).
func TestSecretOverridesNilAnswers(t *testing.T) {
	got := secretOverrides(map[string]string{"GITHUB_TOKEN": "p1"}, nil)
	if len(got) != 1 || got["GITHUB_TOKEN"] != "p1" {
		t.Fatalf("overrides: %v", got)
	}
	if got := secretOverrides(nil, nil); len(got) != 0 {
		t.Fatalf("no sources: %v", got)
	}
}

// Intake secret answers land under their SecretName and win over the
// profile when both name the same variable.
func TestSecretOverridesIntakeWins(t *testing.T) {
	ans := &intake.Answers{Secrets: map[string]string{"api-key": "i1", "other": "i2"}}
	got := secretOverrides(map[string]string{intake.SecretName("api-key"): "p1", "KEEP": "p2"}, ans)
	if got[intake.SecretName("api-key")] != "i1" || got[intake.SecretName("other")] != "i2" || got["KEEP"] != "p2" || len(got) != 3 {
		t.Fatalf("overrides: %v", got)
	}
}
