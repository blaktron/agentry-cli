package examiner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/blaktron/agentry/pkg/gatecases"
	"github.com/blaktron/agentry/pkg/judge"
	"github.com/blaktron/agentry/pkg/manifest"
)

// The registry's gate, restated (plan desktop-review-key-and-run-cards.md
// §6.2, D24, D31). The gate's code is internal to the core, so the
// Examiner's expected outcome restates its rule here, and
// TestTheGateRestatementMatchesTheCoresTable runs every case of the table the
// core publishes and tests its own gate against (pkg/gatecases, agentry#776):
// a core pin bump that changes the gate fails that test until this file
// follows. Nothing else here may decide an outcome.

// GateInput is what the gate weighs, in the table's terms.
type GateInput struct {
	Findings             []gatecases.Finding
	Policy               gatecases.Policy
	WildcardEgress       bool
	JudgeForcesReview    bool
	DeclaredCapabilities []string
	Public               bool
	NamespaceIsOrg       bool
}

// Decision is the gate's answer: the outcome, its queue when it is
// needs_review, the worst severity it weighed and the branch that decided.
type Decision struct {
	gatecases.Want
	Worst  string `json:"worstSeverity"`
	Branch string `json:"branch"`
}

// The gate's branches, as the table names them.
const (
	branchCritical             = "critical"
	branchWildcardEgressReject = "wildcard-egress-reject"
	branchWildcardEgressReview = "wildcard-egress-review"
	branchForcedReview         = "forced-review"
	branchCouldNotScan         = "could-not-scan"
	branchReviewAlways         = "review-always"
	branchReviewCapability     = "review-capability"
	branchWithinThreshold      = "within-threshold"
	branchOverThreshold        = "over-threshold"
)

// The gate's fallback auto-publish threshold, when the policy names none.
const fallbackMaxAuto = "low"

// couldNotScan are the scanner ids that say a stage could not read part of
// the package: they force review whatever the threshold.
var couldNotScan = map[string]bool{
	"S1/unreadable-file":     true,
	"S1/text-scan-truncated": true,
	"S2/tool-error":          true,
	"S2/history-error":       true,
	"SK/unread-text":         true,
}

var severityRanks = map[string]int{"info": 0, "low": 1, "medium": 2, "high": 3, "critical": 4}

var confidenceRanks = map[string]int{"low": 1, "medium": 2, "high": 3}

// Gate is the registry gate's rule over one input.
func Gate(in GateInput) Decision {
	worst := "info"
	for _, f := range in.Findings {
		if confidenceRanks[f.Confidence] < confidenceRanks["medium"] {
			continue
		}
		if severityRanks[f.Severity] > severityRanks[worst] {
			worst = f.Severity
		}
	}
	if worst == "critical" {
		return Decision{Want: gatecases.Want{Outcome: gatecases.OutcomeRejected}, Worst: worst, Branch: branchCritical}
	}
	if in.WildcardEgress {
		if in.Policy.WildcardEgress == "reject" {
			return Decision{Want: gatecases.Want{Outcome: gatecases.OutcomeRejected}, Worst: worst, Branch: branchWildcardEgressReject}
		}
		return held(in, worst, branchWildcardEgressReview)
	}
	if in.JudgeForcesReview {
		return held(in, maxSeverity(worst, "medium"), branchForcedReview)
	}
	for _, f := range in.Findings {
		if couldNotScan[f.Scanner] {
			return held(in, maxSeverity(worst, "medium"), branchCouldNotScan)
		}
	}
	// A form the gate cannot read is "never" to it.
	always, when, _ := parseReviewMode(in.Policy.RequireHumanReview)
	if always {
		return held(in, worst, branchReviewAlways)
	}
	declared := map[string]bool{}
	for _, c := range in.DeclaredCapabilities {
		declared[c] = true
	}
	for _, c := range when {
		if declared[c] {
			return held(in, worst, branchReviewCapability)
		}
	}
	if severityRanks[worst] <= severityRanks[AutoPublishUpTo(in.Policy)] {
		return Decision{Want: gatecases.Want{Outcome: gatecases.OutcomePublished}, Worst: worst, Branch: branchWithinThreshold}
	}
	return held(in, worst, branchOverThreshold)
}

// AutoPublishUpTo is the worst severity a policy publishes: its
// maxAutoPublishSeverity, or the gate's fallback when it names none.
func AutoPublishUpTo(p gatecases.Policy) string {
	if _, ok := severityRanks[p.MaxAutoPublishSeverity]; ok {
		return p.MaxAutoPublishSeverity
	}
	return fallbackMaxAuto
}

// held routes a needs_review decision: an organization's namespace goes to
// its own reviewers unless the version is public with a high finding, and
// everything else to the platform's.
func held(in GateInput, worst, branch string) Decision {
	queue := gatecases.QueuePlatform
	publicHigh := in.Public && severityRanks[worst] >= severityRanks["high"]
	if in.NamespaceIsOrg && !publicHigh {
		queue = gatecases.QueueOrg
	}
	return Decision{Want: gatecases.Want{Outcome: gatecases.OutcomeNeedsReview, Queue: queue}, Worst: worst, Branch: branch}
}

func maxSeverity(a, b string) string {
	if severityRanks[a] >= severityRanks[b] {
		return a
	}
	return b
}

// parseReviewMode reads requireHumanReview: absent or null is "never", and
// so are "never", "always" and a whenCapabilities list as they say. Any
// other form is an error; the gate reads it as "never", and the pre-step
// will not trust a registry's policy that holds it.
func parseReviewMode(raw json.RawMessage) (always bool, when []string, err error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return false, nil, nil
	}
	var s string
	if json.Unmarshal(t, &s) == nil {
		if s != "never" && s != "always" {
			return false, nil, fmt.Errorf("requireHumanReview is %q", s)
		}
		return s == "always", nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(t))
	dec.DisallowUnknownFields()
	var obj struct {
		WhenCapabilities []string `json:"whenCapabilities"`
	}
	if err := dec.Decode(&obj); err != nil {
		return false, nil, fmt.Errorf("requireHumanReview %s is not a form the gate reads", t)
	}
	return false, obj.WhenCapabilities, nil
}

// manifestCaps is the capabilities block of the manifest's canonical JSON,
// decoded as the core's gate input decodes it.
type manifestCaps struct {
	Network struct {
		Egress manifest.Egress `json:"egress"`
	} `json:"network"`
	Filesystem struct {
		Read  json.RawMessage `json:"read"`
		Write json.RawMessage `json:"write"`
	} `json:"filesystem"`
	Exec    json.RawMessage `json:"exec"`
	Secrets []string        `json:"secrets"`
}

func readCaps(manifestJSON []byte) (manifestCaps, bool) {
	var m struct {
		Capabilities *manifestCaps `json:"capabilities"`
	}
	if err := json.Unmarshal(manifestJSON, &m); err != nil || m.Capabilities == nil {
		return manifestCaps{}, false
	}
	return *m.Capabilities, true
}

// DeclaredCapabilities are the capability tokens the gate reads from a
// manifest's canonical JSON: network (a host other than none), fs-read,
// fs-write, exec and env (a declared secret).
func DeclaredCapabilities(manifestJSON []byte) []string {
	c, ok := readCaps(manifestJSON)
	if !ok {
		return nil
	}
	var out []string
	for _, h := range c.Network.Egress.Hosts {
		if h != "none" && h != "" {
			out = append(out, "network")
			break
		}
	}
	if notNone(c.Filesystem.Read) {
		out = append(out, "fs-read")
	}
	if notNone(c.Filesystem.Write) {
		out = append(out, "fs-write")
	}
	if notNone(c.Exec) {
		out = append(out, "exec")
	}
	if len(c.Secrets) > 0 {
		out = append(out, "env")
	}
	return out
}

// notNone says a filesystem or exec value declares more than "none": the
// string "none" or "" does not, an object or a list does.
func notNone(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s != "none" && s != ""
	}
	return true
}

// WildcardEgress says the manifest's canonical JSON declares "*" egress.
func WildcardEgress(manifestJSON []byte) bool {
	c, _ := readCaps(manifestJSON)
	for _, h := range c.Network.Egress.Hosts {
		if strings.ToLower(h) == "*" {
			return true
		}
	}
	return false
}

// AIReviewFinding is one rubric question's answer as stage S5 rates it: a
// score of 2 is medium and 3 high, except that an attempted injection and
// an undeclared capability are high from 2 (the core's verdictFindings,
// DEV-186); below 2 it is no finding. The core's further finding for a
// verdict showing manipulation comes from its generative-model providers
// alone; the Jev judge AI Review asks never sets it. The core publishes no
// table of this mapping, so nothing but this restatement holds it to the
// core's.
func AIReviewFinding(id string, score int) (gatecases.Finding, bool) {
	if score < judge.WithholdScore {
		return gatecases.Finding{}, false
	}
	sev := "medium"
	if score >= 3 || id == judge.RubricIDInjection || id == judge.RubricIDUndeclaredCapability {
		sev = "high"
	}
	return gatecases.Finding{Scanner: "S5/" + id, Severity: sev, Confidence: "medium"}, true
}
