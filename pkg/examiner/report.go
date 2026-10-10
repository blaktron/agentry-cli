package examiner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/gatecases"
	"github.com/blaktron/agentry/pkg/judge"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// The post-step (plan §6.1): one report from the pre-step's findings, the
// run's own record of AI Review and the Examiner's answer. The expected
// outcome is computed from the first two kinds alone, by Gate; the third is
// reported beside it and never counted. Nothing the model wrote reaches the
// first two kinds: AI Review's scores are read from the review ledger the
// tools container wrote, not from the model's answer.

// ReportFile is the report's name in the output folder.
const ReportFile = "report.json"

// ReportSchemaVersion is the report's shape (README, "The report").
const ReportSchemaVersion = "1"

// The three kinds of finding, as the report labels them.
const (
	// KindRegistryRules: the registry's own rules, run here as the registry
	// runs them.
	KindRegistryRules = "registry-rules"
	// KindAIReview: AI Review, asked as stage S5 asks it.
	KindAIReview = "ai-review"
	// KindExaminer: the Examiner's own reading, never counted.
	KindExaminer = "examiner"
)

// The expected outcomes.
const (
	OutcomePublished   = gatecases.OutcomePublished
	OutcomeNeedsReview = gatecases.OutcomeNeedsReview
	OutcomeRejected    = gatecases.OutcomeRejected
	// OutcomeRefused: Submit refuses the manifest before the gate runs.
	OutcomeRefused = "refused_at_submit"
	// OutcomeUndetermined: what ran here cannot decide between outcomes.
	OutcomeUndetermined = "undetermined"
)

// Notice ends every report.
const Notice = "The registry scans again at Submit. AI Review can answer differently on the same files, and the registry's answer is the one that counts."

// NotRunHere are the registry's checks no client has the code or the
// standing to run: every report names them.
var NotRunHere = []string{
	"the registry's secrets stage (S2), which scans the files and their history with its own tools",
	"the registry's scripts stage (S6), which checks each support script against the declared capabilities",
	"the publisher signals (S8): the name against popular QuickAgents, and the account's own record",
	"the diff against the last published version (S9)",
	"an organization's policy, which can be stricter than the platform's baseline",
	"a shipped knowledge pack's secrets scan, URL reputation and AI Review, in the registry's stage for packs (its hidden-content and injection rules run here)",
	"the text the registry recovers from image metadata, and its comparison of the instruction file's front matter with the manifest",
	"Submit's own refusals: the policy's blocked capabilities, blocked MCP servers, allowed licences and required evaluations, a quota, two-step sign-in, credits, a version already published",
}

// Finding is one finding of any kind.
type Finding struct {
	Kind string `json:"kind"`
	// Scanner is the registry's scanner id (S1/…, S4/…, S5/…, S7/…) for the
	// first two kinds, and secret, script, instruction or other for the
	// Examiner's.
	Scanner    string `json:"scanner"`
	Severity   string `json:"severity,omitempty"`
	Confidence string `json:"confidence,omitempty"`
	Title      string `json:"title"`
	File       string `json:"file,omitempty"`
	Line       int    `json:"line,omitempty"`
	Excerpt    string `json:"excerpt,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// Report is report.json.
type Report struct {
	SchemaVersion string   `json:"schemaVersion"`
	Examiner      Examined `json:"examiner"`
	Package       Package  `json:"package"`
	Expected      Expected `json:"expected"`
	// RegistryRules is kind 1.
	RegistryRules RegistryRules `json:"registryRules"`
	// AIReview is kind 2.
	AIReview AIReview `json:"aiReview"`
	// Reading is kind 3.
	Reading Reading `json:"examinerReading"`
	// NotRunHere is what the registry runs and this review did not.
	NotRunHere []string `json:"notRunHere"`
	Notice     string   `json:"notice"`
}

// Examined names the Examiner run.
type Examined struct {
	Ref   string `json:"ref"`
	Build string `json:"build"`
	Model string `json:"model,omitempty"`
	RunID string `json:"runId,omitempty"`
}

// Package names what was examined.
type Package struct {
	Ref   string `json:"ref"`
	Dir   string `json:"dir"`
	Files int    `json:"files"`
}

// Expected is the outcome the gate would be expected to reach.
type Expected struct {
	// Outcome is published, needs_review, rejected, refused_at_submit or
	// undetermined.
	Outcome string `json:"outcome"`
	// Worst is the worst severity the gate would weigh, and Branch its rule
	// that decides, when the gate decides.
	Worst  string `json:"worstSeverity,omitempty"`
	Branch string `json:"branch,omitempty"`
	// Reason says why, in a sentence.
	Reason string `json:"reason"`
	// Assumes says what the outcome takes as given that only Submit knows.
	Assumes string     `json:"assumes"`
	Policy  PolicyUsed `json:"policy"`
	// Counted says which kinds of finding were weighed.
	Counted []string `json:"counted"`
}

// RegistryRules is kind 1.
type RegistryRules struct {
	// Manifest are the manifest rules' errors and warnings; an error is
	// refused at Submit.
	Manifest []ManifestIssue `json:"manifest"`
	Findings []Finding       `json:"findings"`
	Bundle   struct {
		Version string `json:"version"`
		Source  string `json:"source"`
	} `json:"bundle"`
	// Servers are the registry's records of the declared MCP servers.
	Servers []ServerRecord `json:"servers"`
}

// ManifestIssue is one manifest rule's result.
type ManifestIssue struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Path     string `json:"path,omitempty"`
	Message  string `json:"message"`
}

// ServerRecord is the registry's record of one declared server, as asked
// here.
type ServerRecord struct {
	Ref    string `json:"ref"`
	Status string `json:"status"`
	// Error says why there is no answer, for status unknown or unreachable.
	Error string `json:"error,omitempty"`
}

// AIReview is kind 2: the first answered ai_review call over the staged
// package, from the run's review ledger.
type AIReview struct {
	Ran        bool      `json:"ran"`
	Model      string    `json:"model,omitempty"`
	RubricHash string    `json:"rubricHash,omitempty"`
	Axes       []Axis    `json:"axes"`
	Findings   []Finding `json:"findings"`
	Files      int       `json:"files"`
	NotRead    int       `json:"notRead"`
	// Calls counts the ai_review calls the run made.
	Calls int `json:"calls"`
	// Failure says why it did not run, when it did not.
	Failure string `json:"failure,omitempty"`
}

// Axis is one rubric question's answer.
type Axis struct {
	ID       string  `json:"id"`
	Score    int     `json:"score"`
	Severity string  `json:"severity,omitempty"`
	File     string  `json:"file,omitempty"`
	Prob     float64 `json:"probability"`
}

// Reading is kind 3: the Examiner's own reading, and what was withheld
// from its model.
type Reading struct {
	Counted  bool      `json:"counted"`
	Summary  string    `json:"summary,omitempty"`
	Findings []Finding `json:"findings"`
	// NotRead are the files the model says it was not given or did not
	// read; Withheld the files AI Review withheld from it, from the run's
	// own record.
	NotRead  []NotRead `json:"notRead"`
	Withheld []NotRead `json:"withheld"`
	// Error says why there is no reading: the run failed, or its answer
	// was not one.
	Error string `json:"error,omitempty"`
	// Recovered says the runner refused the model's last message at the
	// output schema (exit 17), and the reading is a terminal JSON object
	// inside it, which output.schema.json passes.
	Recovered bool `json:"recovered,omitempty"`
}

// recoverReading is a schema-valid terminal object in a last message the
// runner refused at its output schema (engine.RecoverJSONResult). The reading
// is never counted, so a recovered one changes nothing but the report's
// third kind.
func recoverReading(result json.RawMessage) (json.RawMessage, error) {
	var text string
	if err := json.Unmarshal(result, &text); err != nil {
		return nil, errors.New("the refused message is not text")
	}
	schemaBytes, err := fs.ReadFile(FS(), compile.OutputSchemaFile)
	if err != nil {
		return nil, err
	}
	sch, err := compile.CompileOutputSchema(schemaBytes)
	if err != nil {
		return nil, err
	}
	result, _, err = engine.RecoverJSONResult(text, sch)
	return result, err
}

// NotRead is a file and why it was not read.
type NotRead struct {
	File string `json:"file"`
	Why  string `json:"why"`
}

// answer is the Examiner's answer, output.schema.json's shape.
type answer struct {
	Summary  string `json:"summary"`
	Findings []struct {
		Kind   string `json:"kind"`
		File   string `json:"file"`
		Line   int    `json:"line"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	} `json:"findings"`
	NotRead []NotRead `json:"notRead"`
}

// Task is the task the Examiner is given: the staged inputs' names and
// size, and nothing of the package's own text, so no word of it reaches
// the model except through read_input.
func Task(p *Prepared) string {
	return fmt.Sprintf("Examine the QuickAgent in your inputs: %s is its manifest's canonical JSON and %s/ holds its %d file(s). "+
		"Call ai_review with manifest %q and dir %q first, then read every file, and answer as your instruction says.",
		InputManifest, InputPackage, p.Files, InputManifest, InputPackage)
}

// Finish is the post-step: the report of a review whose run returned env
// (nil when it failed before it ran) and runErr, with the run's receipts
// under outDir. It always returns a report: a run that failed still has
// kind 1, and says why the rest is missing.
func Finish(p *Prepared, env *receipt.Envelope, runErr error, outDir, ref, build string) *Report {
	r := &Report{
		SchemaVersion: ReportSchemaVersion,
		Examiner:      Examined{Ref: ref, Build: build},
		Package:       Package{Ref: p.Ref, Dir: p.Dir, Files: p.Files},
		RegistryRules: registryRules(p),
		NotRunHere:    append([]string{}, NotRunHere...),
		Notice:        Notice,
	}
	if env != nil {
		r.Examiner.Model, r.Examiner.RunID = env.Harness.Model, env.RunID
	}
	// The ledger is read only for a run whose own receipt counted a review:
	// a run that failed before it was set up wrote none, and a ledger left
	// in the folder by another run is never this one's.
	var entries []toolsrv.ReviewEntry
	if env != nil && env.Review != nil {
		entries, _, _ = engine.ReadReviewLedger(filepath.Join(outDir, "receipts", engine.ReviewLedgerFile))
	}
	r.AIReview = aiReviewOf(entries)
	if !r.AIReview.Ran && r.AIReview.Failure == "" {
		r.AIReview.Failure = "the Examiner did not ask it"
		if runErr != nil {
			r.AIReview.Failure = "the run ended before it was asked: " + runErr.Error()
		}
	}
	r.Reading = readingOf(env, runErr, entries)
	if !r.AIReview.Ran {
		r.NotRunHere = append(r.NotRunHere, "AI Review: "+r.AIReview.Failure)
	}
	for _, s := range p.Servers {
		if unresolved(s) {
			r.NotRunHere = append(r.NotRunHere, "a current lookup of "+s.Ref+" (the registry's record here: "+s.Status+"): the registry looks it up again at Submit")
		}
	}
	if serversNotAsked(p) {
		r.NotRunHere = append(r.NotRunHere, "the registry's record of the declared MCP servers: no registry was asked")
	}
	if r.AIReview.Ran && p.OverReviewBudget {
		r.NotRunHere = append(r.NotRunHere, "AI Review of the files one review leaves out past its budget (4 MiB, 500 files), which the registry reads")
	}
	if p.Policy.Source != PolicyFromRegistry {
		r.NotRunHere = append(r.NotRunHere, "the registry's own baseline policy ("+p.Policy.Error+"); the baseline compiled into this build was used")
	}
	r.Expected = Expect(p, r.AIReview)
	return r
}

func registryRules(p *Prepared) RegistryRules {
	rr := RegistryRules{Manifest: []ManifestIssue{}, Findings: append([]Finding{}, p.Findings...), Servers: []ServerRecord{}}
	for _, is := range p.Validation.Issues {
		rr.Manifest = append(rr.Manifest, ManifestIssue(is))
	}
	rr.Bundle.Version, rr.Bundle.Source = p.Validation.Bundle.Version, p.Validation.Bundle.Source
	for _, s := range p.Servers {
		rr.Servers = append(rr.Servers, ServerRecord{Ref: s.Ref, Status: s.Status, Error: s.Err})
	}
	return rr
}

// aiReviewOf reads kind 2 from the ledger: the first answered call over the
// staged manifest and package, all six axes present. A later call is
// counted, never preferred: the model cannot ask again for a kinder answer.
func aiReviewOf(entries []toolsrv.ReviewEntry) AIReview {
	a := AIReview{Axes: []Axis{}, Findings: []Finding{}}
	var failure string
	for _, e := range entries {
		if e.Tool != toolsrv.ToolAIReview {
			continue
		}
		if e.Refused == "" {
			a.Calls++
		}
		switch {
		case a.Ran:
			continue
		case e.Refused != "":
			failure = "it was refused: " + e.Refused
			continue
		case e.Verdict == webscan.NotScanned:
			failure = "it did not finish: " + e.Failure
			continue
		case e.Dir != InputPackage || e.Manifest != InputManifest:
			failure = fmt.Sprintf("the Examiner asked it about %s and %s, not the staged package", e.Manifest, e.Dir)
			continue
		case !allAxes(e.Axes):
			failure = "its record does not hold the six questions' scores"
			continue
		}
		a.Ran, a.Model, a.RubricHash, a.Files, a.NotRead = true, e.Model, e.RubricHash, e.Files, e.NotRead
		for _, ax := range e.Axes {
			out := Axis{ID: ax.ID, Score: ax.Score, File: ax.File, Prob: ax.Probability}
			if f, ok := AIReviewFinding(ax.ID, ax.Score); ok {
				out.Severity = f.Severity
				a.Findings = append(a.Findings, Finding{Kind: KindAIReview, Scanner: f.Scanner, Severity: f.Severity, Confidence: f.Confidence,
					Title: "AI Review: " + axisTitle[ax.ID], File: ax.File, Detail: fmt.Sprintf("score %d of 3", ax.Score)})
			}
			a.Axes = append(a.Axes, out)
		}
	}
	if !a.Ran {
		a.Failure = failure
	}
	return a
}

// axisTitle names each rubric question as the registry's report does.
var axisTitle = map[string]string{
	judge.RubricIDInjection:            "prompt-injection or instruction override",
	judge.RubricIDExfiltration:         "data exfiltration",
	judge.RubricIDUndeclaredCapability: "dangerous capability not declared and outside the stated purpose",
	judge.RubricIDDeception:            "end-user deception",
	judge.RubricIDObfuscation:          "obfuscation indicators",
	judge.RubricIDDestructive:          "destructive actions without confirmation",
}

// allAxes says a ledger entry holds each rubric question once.
func allAxes(axes []toolsrv.ReviewAxis) bool {
	seen := map[string]bool{}
	for _, ax := range axes {
		if _, ok := axisTitle[ax.ID]; !ok || seen[ax.ID] || ax.Score < 0 || ax.Score > 3 {
			return false
		}
		seen[ax.ID] = true
	}
	return len(seen) == len(axisTitle)
}

// readingOf is kind 3, and the files withheld from the model.
func readingOf(env *receipt.Envelope, runErr error, entries []toolsrv.ReviewEntry) Reading {
	rd := Reading{Findings: []Finding{}, NotRead: []NotRead{}, Withheld: []NotRead{}}
	for _, e := range entries {
		if e.Tool == toolsrv.ToolReadInput && e.Refused == "" && !e.Passed() {
			rd.Withheld = append(rd.Withheld, NotRead{File: e.Path, Why: e.Reason()})
		}
	}
	result := json.RawMessage(nil)
	if env != nil {
		result = env.Result
	}
	var ce *engine.ClassifiedError
	switch {
	case runErr != nil && errors.As(runErr, &ce) && ce.Code == engine.ExitOutputSchema && env != nil:
		recovered, err := recoverReading(env.Result)
		if err != nil {
			rd.Error = "the run did not finish: " + runErr.Error()
			return rd
		}
		result, rd.Recovered = recovered, true
	case runErr != nil:
		rd.Error = "the run did not finish: " + runErr.Error()
		return rd
	case len(result) == 0:
		rd.Error = "the run returned no answer"
		return rd
	}
	var a answer
	if err := json.Unmarshal(result, &a); err != nil {
		rd.Error = "the Examiner's answer is not a reading: " + err.Error()
		return rd
	}
	rd.Summary = a.Summary
	for _, f := range a.Findings {
		rd.Findings = append(rd.Findings, Finding{Kind: KindExaminer, Scanner: f.Kind, Title: f.Title, File: f.File, Line: f.Line, Detail: f.Detail})
	}
	if a.NotRead != nil {
		rd.NotRead = a.NotRead
	}
	return rd
}

// Assumption is what every expected outcome takes as given: the manifest
// rules run as for a public version (V7), and a held version's queue is not
// named, since only Submit knows the version's visibility and whether its
// namespace is an organization's.
const Assumption = "a public version, as the manifest rules check it; the review queue a held version would go to is not named"

// Expect is the outcome the gate would be expected to reach from kinds 1
// and 2 under the policy the pre-step found. A manifest the rules refuse is
// refused at Submit, before the gate. Where something the gate weighs did
// not answer here (AI Review, or answered for part of the package only; a
// declared server's record), the outcome is named only when the best and
// the worst answer it could give decide alike.
func Expect(p *Prepared, ai AIReview) Expected {
	e := Expected{Policy: p.Policy, Counted: []string{KindRegistryRules}, Assumes: Assumption}
	errs := 0
	for _, is := range p.Validation.Issues {
		if is.Severity == "error" {
			errs++
		}
	}
	if errs > 0 {
		e.Outcome = OutcomeRefused
		e.Reason = fmt.Sprintf("Submit would be expected to refuse it before the gate runs: the manifest rules found %d error(s).", errs)
		return e
	}
	in := GateInput{
		Policy: p.Policy.Policy, WildcardEgress: WildcardEgress(p.ManifestJSON), DeclaredCapabilities: DeclaredCapabilities(p.ManifestJSON),
		Public: true,
	}
	for _, f := range p.Findings {
		in.Findings = append(in.Findings, gatecases.Finding{Scanner: f.Scanner, Severity: f.Severity, Confidence: f.Confidence})
	}
	if ai.Ran {
		e.Counted = append(e.Counted, KindAIReview)
		for _, f := range ai.Findings {
			in.Findings = append(in.Findings, gatecases.Finding{Scanner: f.Scanner, Severity: f.Severity, Confidence: f.Confidence})
		}
	}
	var open []string
	var worst []gatecases.Finding
	switch {
	case !ai.Ran:
		open = append(open, "AI Review did not answer here")
		worst = append(worst, worstAIReview()...)
	case p.OverReviewBudget:
		open = append(open, "AI Review read only part of the package here")
		worst = append(worst, worstAIReview()...)
	}
	if unresolvedServers(p) {
		open = append(open, "the registry had no record here of a declared MCP server")
		// The worst a server's lookup can add is an advisory rated critical,
		// at high confidence, as stage S7 rates every advisory.
		worst = append(worst, gatecases.Finding{Scanner: "S7/mcp-vuln-unresolved", Severity: "critical", Confidence: "high"})
	}
	best := Gate(in)
	if len(open) == 0 {
		e.Outcome, e.Worst, e.Branch, e.Reason = best.Outcome, best.Worst, best.Branch, reasonOf(best)
		return e
	}
	worstIn := in
	worstIn.Findings = append(append([]gatecases.Finding{}, in.Findings...), worst...)
	because := strings.Join(open, ", and ")
	if w := Gate(worstIn); w.Outcome != best.Outcome {
		e.Outcome = OutcomeUndetermined
		e.Reason = upperFirst(because) + ", and the answer would decide between " + OutcomeWords(best.Outcome) + " and " + OutcomeWords(w.Outcome) + "."
		return e
	}
	e.Outcome, e.Worst, e.Branch = best.Outcome, best.Worst, best.Branch
	e.Reason = reasonOf(best) + " " + upperFirst(because) + ", and the gate would be expected to decide alike whatever it answered."
	return e
}

// unresolvedServers says a declared server has no answer here: the registry
// was not asked, did not answer, has never seen the version, or holds only
// a failed lookup. At Submit it looks each one up again.
func unresolvedServers(p *Prepared) bool {
	if serversNotAsked(p) {
		return true
	}
	for _, s := range p.Servers {
		if unresolved(s) {
			return true
		}
	}
	return false
}

// serversNotAsked: the manifest declares servers and no registry was asked.
func serversNotAsked(p *Prepared) bool { return p.DeclaredServers > 0 && len(p.Servers) == 0 }

// serverRecordLifetime is how long a server's record is taken as settled
// here: the registry looks a record up again at Submit once it is 24 hours
// old (the core's mcpScanTTL), and an hour is left for the time between
// this review and a Submit, and for the two clocks.
const serverRecordLifetime = 23 * time.Hour

// unresolved: the registry's answer for this server here is not the one
// Submit would weigh. It did not answer, has never seen the version, holds
// a failed lookup (whose last known advisories are still counted), or a
// record Submit would look up again for its age.
func unresolved(s mcpinstall.VulnStatus) bool {
	switch s.Status {
	case "unknown", "unreachable", "error":
		return true
	}
	return s.CheckedAt.IsZero() || time.Since(s.CheckedAt) >= serverRecordLifetime
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// worstAIReview is every rubric question at score 3.
func worstAIReview() []gatecases.Finding {
	var out []gatecases.Finding
	for _, q := range judge.RubricQuestions() {
		f, _ := AIReviewFinding(q.ID, 3)
		out = append(out, f)
	}
	return out
}

// reasonOf says why the gate would decide as it does.
func reasonOf(d Decision) string {
	switch d.Branch {
	case branchCritical:
		return "The gate would be expected to reject it for a critical finding."
	case branchWildcardEgressReject:
		return "The policy refuses wildcard network egress, so the gate would be expected to reject it."
	case branchWildcardEgressReview:
		return "It declares wildcard network egress, so the gate would be expected to send it to review."
	case branchForcedReview:
		return "A stage could not finish, so the gate would be expected to send it to review."
	case branchCouldNotScan:
		return "Part of it could not be scanned, so the gate would be expected to send it to review."
	case branchReviewAlways:
		return "The policy sends every version to review."
	case branchReviewCapability:
		return "The policy sends a version with one of its declared capabilities to review."
	case branchWithinThreshold:
		return fmt.Sprintf("Its worst finding (%s) is within the policy's auto-publish threshold, so the gate would be expected to publish it.", d.Worst)
	case branchOverThreshold:
		return fmt.Sprintf("Its worst finding (%s) is past the policy's auto-publish threshold, so the gate would be expected to send it to review.", d.Worst)
	}
	return ""
}

// OutcomeWords is an outcome as a person reads it.
func OutcomeWords(o string) string {
	switch o {
	case OutcomePublished:
		return "published"
	case OutcomeNeedsReview:
		return "held for review"
	case OutcomeRejected:
		return "rejected"
	case OutcomeRefused:
		return "refused at Submit"
	}
	return "not determined here"
}

// Write writes the report to outDir/report.json.
func Write(outDir string, r *Report) (string, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(outDir, ReportFile)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("the report's folder: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil { //nolint:gosec // the person's own output folder
		return "", fmt.Errorf("write %s: %w", ReportFile, err)
	}
	return path, nil
}
