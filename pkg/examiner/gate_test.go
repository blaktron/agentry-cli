package examiner

import (
	"testing"

	"github.com/blaktron/agentry/pkg/gatecases"
	"github.com/blaktron/agentry/pkg/judge"
)

// TestTheGateRestatementMatchesTheCoresTable (plan
// desktop-review-key-and-run-cards.md D31; agentry#776): every case of the
// table the core publishes, and runs against its own gate, reaches the same
// outcome, queue and branch here. A core pin that changes the gate fails
// this test until Gate follows it.
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
// from 2 (DEV-186; deception since ruleset 0.4.0), exfiltration and
// destructive actions medium at any score (ruleset 0.4.0, agentry#893), and
// nothing under 2.
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

// TestDeclaredCapabilitiesReadTheCanonicalJSON: the tokens the gate's
// requireHumanReview reads, from the form the core reads them in.
func TestDeclaredCapabilitiesReadTheCanonicalJSON(t *testing.T) {
	for _, tc := range []struct {
		json     string
		want     string
		wildcard bool
	}{
		{`{"capabilities":{"network":{"egress":"none"},"filesystem":{"read":"none","write":"none"},"exec":"none"}}`, "", false},
		{`{"capabilities":{"network":{"egress":["api.example.com"]},"filesystem":{"read":{"paths":["/a"]},"write":"none"},"exec":{"commands":["git"]},"secrets":["K"]}}`, "network,fs-read,exec,env", false},
		{`{"capabilities":{"network":{"egress":["*"]},"filesystem":{"write":{"paths":["/o"]}}}}`, "network,fs-write", true},
		{`not json`, "", false},
	} {
		got := DeclaredCapabilities([]byte(tc.json))
		s := ""
		for i, c := range got {
			if i > 0 {
				s += ","
			}
			s += c
		}
		if s != tc.want || WildcardEgress([]byte(tc.json)) != tc.wildcard {
			t.Errorf("%s: %q wildcard %v", tc.json, s, WildcardEgress([]byte(tc.json)))
		}
	}
}
