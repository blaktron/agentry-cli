package examiner

import (
	"slices"
	"testing"

	"github.com/blaktron/agentry/pkg/gatecases"
	"github.com/blaktron/agentry/pkg/judge"
)

// TestTheGateRestatementMatchesTheCoresTable: every case of the table the
// core publishes, and runs against its own gate, reaches the same outcome,
// queue and branch here. A core pin that changes the gate fails this test
// until Gate follows it.
func TestTheGateRestatementMatchesTheCoresTable(t *testing.T) {
	table, err := gatecases.Load()
	if err != nil {
		t.Fatal(err)
	}
	branches := map[string]bool{}
	for _, c := range table.Cases {
		got := Gate(GateInput{
			Findings: c.Findings, Policy: table.PolicyOf(c), WildcardEgress: c.WildcardEgress,
			JudgeForcesReview: c.JudgeForcesReview, DeclaredCapabilities: c.DeclaredCapabilities,
			Public: c.Public, NamespaceIsOrg: c.NamespaceIsOrg,
		})
		if got.Want != c.Want || got.Branch != c.Branch {
			t.Errorf("%s: %+v by %s, the core's gate reaches %+v by %s", c.Name, got.Want, got.Branch, c.Want, c.Branch)
		}
		branches[c.Branch] = true
	}
	// Every branch Gate names is one the table reaches, so no rule here is
	// one the core's gate does not have.
	for _, b := range []string{branchCritical, branchWildcardEgressReject, branchWildcardEgressReview, branchForcedReview,
		branchCouldNotScan, branchReviewAlways, branchReviewCapability, branchWithinThreshold, branchOverThreshold} {
		if !branches[b] {
			t.Errorf("the branch %s is reached by no case of the core's table", b)
		}
	}
}

// TestAIReviewFindingRatesAsS5Does: score 2 medium and 3 high, with an
// attempted injection, an undeclared capability and deceiving the user high
// from 2 (deception from ruleset 0.4.0), exfiltration and destructive
// actions medium at any score (ruleset 0.4.0), and nothing under 2.
func TestAIReviewFindingRatesAsS5Does(t *testing.T) {
	for _, tc := range []struct {
		id    string
		score int
		want  string
	}{
		{judge.RubricIDExfiltration, 1, ""},
		{judge.RubricIDExfiltration, 2, "medium"},
		{judge.RubricIDExfiltration, 3, "medium"},
		{judge.RubricIDDeception, 2, "high"},
		{judge.RubricIDDeception, 3, "high"},
		{judge.RubricIDObfuscation, 2, "medium"},
		{judge.RubricIDObfuscation, 3, "high"},
		{judge.RubricIDDestructive, 2, "medium"},
		{judge.RubricIDDestructive, 3, "medium"},
		{judge.RubricIDInjection, 2, "high"},
		{judge.RubricIDUndeclaredCapability, 2, "high"},
		{judge.RubricIDInjection, 1, ""},
	} {
		f, ok := AIReviewFinding(tc.id, tc.score)
		if ok != (tc.want != "") || f.Severity != tc.want {
			t.Errorf("%s at %d: %+v %v, want %q", tc.id, tc.score, f, ok, tc.want)
			continue
		}
		if ok && (f.Scanner != "S5/"+tc.id || f.Confidence != "medium") {
			t.Errorf("%s at %d: %+v", tc.id, tc.score, f)
		}
	}
}

// TestTheGateInputsAreTheRegistrysDerivations: the two gate inputs the
// expected outcome reads from a manifest come from the core's own
// derivations, so every derivation case the core publishes gives the
// Examiner the tokens and the flag the registry's gate is given.
func TestTheGateInputsAreTheRegistrysDerivations(t *testing.T) {
	table, err := gatecases.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(table.Derivations) == 0 {
		t.Fatal("the core publishes no derivation cases")
	}
	for _, d := range table.Derivations {
		p := &Prepared{ManifestJSON: d.Manifest, Policy: PolicyUsed{Policy: table.Baseline}}
		got := gateInputOf(p)
		if !slices.Equal(got.DeclaredCapabilities, d.Want.DeclaredCapabilities) || got.WildcardEgress != d.Want.UnrestrictedNetwork {
			t.Errorf("%s: tokens %v flag %v, the registry's are %v %v", d.Name, got.DeclaredCapabilities, got.WildcardEgress, d.Want.DeclaredCapabilities, d.Want.UnrestrictedNetwork)
		}
	}
}
