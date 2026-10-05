// Package receipt defines the run result envelope (RUNNER-PLAN §R8) — the
// runtime counterpart of the registry's public scan report. Every claim in
// it is either verified upstream (ref, digest, attestation), compiled
// (capabilities_enforced, run plan hash), or measured (usage); nothing in
// it is ever a secret value or a gateway token (§R11 invariant 7).
package receipt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// EnvelopeVersion is the result envelope schema id.
const EnvelopeVersion = "run/v1"

// Envelope is the machine-readable result of one run. With
// --output-format json it IS stdout; in text mode it is written to
// <out>/result.json and stdout carries only the result text — either way
// the output folder keeps the full record (RQ3).
type Envelope struct {
	// Agentry is EnvelopeVersion.
	Agentry string `json:"agentry"`
	// Status is "ok" or "error"; ExitCode is the §R8 class the process
	// exits with; Error carries the failure detail for status error.
	Status   string `json:"status"`
	ExitCode int    `json:"exitCode"`
	// ExitClass names ExitCode (ok, usage, verify, policy, compile, gateway,
	// harness, timeout, budget, outputSchema, cli): a client acts on the
	// name, never on the number (agentry-cli#102). Set by Marshal.
	ExitClass string `json:"exitClass"`
	Error     string `json:"error,omitempty"`
	// Stage names the step a failed run stopped in, one word from a fixed
	// list (the Stage constants), so a driver that may not keep the
	// error's text (a hosted run's host keeps nothing the run could have
	// written) can still say where it failed (agentry-cli#332). It says
	// where; ExitClass says what: a policy refusal while the declared MCP
	// servers are checked is class policy in stage mcp-servers. Empty on
	// success.
	Stage string `json:"stage,omitempty"`
	// Reason says why, in fixed words, when the step that stopped the run
	// decided it (agentry-cli#628): a code from a closed list and the
	// release's own declared names it concerns, never text (Reason).
	// Absent on success and on a failure no step named.
	Reason *Reason `json:"reason,omitempty"`
	// RunID names the run (container labels, run dirs).
	RunID string `json:"runId"`
	// Ref and Digest identify the exact release that ran.
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
	// Attestation reports the install-gate outcome the run inherited.
	Attestation Attestation `json:"attestation"`
	// CapabilitiesEnforced names each contract line's mechanism (§R4).
	CapabilitiesEnforced map[string]string `json:"capabilities_enforced"`
	// Harness identifies the adapter, image, and model that ran.
	Harness Harness `json:"harness"`
	// Policy names the operator policy file the run enforced: its path,
	// how that path was chosen, and the digest the plan was compiled
	// against (agentry-cli#152). Absent in envelopes from clients that do
	// not say, and in those written before it existed.
	Policy *Policy `json:"policy,omitempty"`
	// Usage is measured from the harness event stream, best-effort where
	// the stream is (RUNNER-DEVIATIONS DR-4).
	Usage Usage `json:"usage"`
	// Result is the agent's final output: parsed JSON when the package
	// declared an output schema and the output parsed, else plain text.
	Result json.RawMessage `json:"result"`
	// OutDir is the mounted output folder (reports/, receipts/).
	OutDir string `json:"outDir"`
	// ResultFile is <OutDir>/result.json, this envelope's own record on
	// disk, named so a client need not derive it. Set by Marshal.
	ResultFile string `json:"resultFile,omitempty"`
	// Receipts locates the audit trail.
	Receipts Receipts `json:"receipts"`
	// InstalledChanges names what changed in an installed release a run
	// refused because it no longer matches what was laid down (exit 10,
	// class verify; agentry-cli#326), so a client can show the files and
	// offer the repair without parsing Error. Absent otherwise.
	InstalledChanges *InstalledChanges `json:"installedChanges,omitempty"`
	// Web counts what the built-in Drafter's web tools read and what AI
	// Review withheld (plan builder-rework.md §7.3; agentry-cli#345), from
	// the run's web ledger (receipts/web.ndjson, which names each address
	// and verdict). Absent for a run without the web tools.
	Web *Web `json:"web,omitempty"`
	// Review is AI Review's record of a run with the review grant (plan
	// desktop-review-key-and-run-cards.md §6.2, agentry-cli#503): what of
	// the inputs it scanned before the model, and the rubric calls; absent
	// for any other run.
	Review *Review `json:"review,omitempty"`
}

// Web is a run's count of the web tools' reads and AI Review's verdicts.
type Web struct {
	// Scan is where the texts were scanned: "registry" (the registry's
	// route, with the job's credential) or "jev" (the person's own key).
	Scan string `json:"scan"`
	// Search is where web_search went ("registry", a provider, or "" when
	// it was not granted).
	Search string `json:"search,omitempty"`
	// Pages, SearchResults and RegistryCalls are the texts the tools
	// brought back of each kind; Searches the searches that answered.
	Pages         int `json:"pages"`
	Searches      int `json:"searches"`
	SearchResults int `json:"searchResults"`
	RegistryCalls int `json:"registryCalls"`
	// Read is every text the tools brought back: Pages + SearchResults +
	// RegistryCalls. Each was sent to AI Review before the model.
	Read int `json:"read"`
	// Passed reached the model. Withheld were flagged; NotScanned could not
	// be scanned and were withheld as well.
	Passed     int `json:"passed"`
	Withheld   int `json:"withheld"`
	NotScanned int `json:"notScanned"`
	// Refused are calls the tools refused before reading anything: an
	// address that is not public, a spent budget, a type that is not text.
	Refused int `json:"refused"`
	// ScanRequests and ScanInputTokens are what the scans cost the judge,
	// for the record; AI Review is 1 credit an operation (DEV-150).
	ScanRequests    int   `json:"scanRequests"`
	ScanInputTokens int64 `json:"scanInputTokens"`
	// Model and QuestionSet name the judge and what it was asked, when a
	// scan answered.
	Model       string `json:"model,omitempty"`
	QuestionSet string `json:"questionSet,omitempty"`
}

// Review counts a review-grant run's ledger (receipts/review.ndjson).
type Review struct {
	// Read is every input file read_input scanned before the model; Passed
	// reached the model, Withheld were flagged and NotScanned could not be
	// scanned (too large, the judge failed), withheld as well.
	Read       int `json:"read"`
	Passed     int `json:"passed"`
	Withheld   int `json:"withheld"`
	NotScanned int `json:"notScanned"`
	// Refused are reads and calls refused before anything was scanned: a
	// file that is not text, a spent budget, no key.
	Refused int `json:"refused"`
	// Reviews are the ai_review calls that answered; Failed those that did
	// not finish, and Findings the rubric questions that scored 2 or more
	// over all of them.
	Reviews  int `json:"reviews"`
	Failed   int `json:"failed"`
	Findings int `json:"findings"`
	// ScanRequests and ScanInputTokens are what the scans and the rubric
	// cost the judge, for the record; AI Review is 1 credit an operation
	// (DEV-150).
	ScanRequests    int   `json:"scanRequests"`
	ScanInputTokens int64 `json:"scanInputTokens"`
	// Model, QuestionSet and RubricHash name the judge and what it was
	// asked, when it answered.
	Model       string `json:"model,omitempty"`
	QuestionSet string `json:"questionSet,omitempty"`
	RubricHash  string `json:"rubricHash,omitempty"`
}

// InstalledChanges is an installed release's difference from what was laid
// down. Paths are relative to the release's own files, as its archive
// carries them.
type InstalledChanges struct {
	Changed []string `json:"changed,omitempty"`
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	// Record is set when install.json or the integrity record itself is
	// what failed.
	Record string `json:"record,omitempty"`
	// Repair is the command that lays the release down again.
	Repair string `json:"repair"`
}

// Attestation is the verify-gate outcome (§R1).
type Attestation struct {
	// State is verified | unsigned | local | unscanned | builtin.
	State string `json:"state"`
	// KeyID is the pinned signing key that verified (state verified).
	KeyID string `json:"keyId,omitempty"`
	// Verified is the boolean shorthand clients gate on.
	Verified bool `json:"verified"`
	// Consent names the flag that admitted a release the gate would have
	// refused — `--allow-unsigned` for unsigned and local, `--allow-unscanned`
	// for a version the registry admitted without a scan — so the receipt
	// says not only what the verdict was but who accepted it. Empty for a
	// verified release, which needed none.
	Consent string `json:"consent,omitempty"`
	// Build is, for state builtin, the build of the application that
	// carried the package (agentry-cli#318): a built-in has no registry
	// digest to name its provenance, so the receipt names the binary.
	Build string `json:"build,omitempty"`
}

// Policy is the operator policy a run enforced. Path is the file whose
// bytes SHA256 fingerprints — the digest the run plan records as
// policySha256 — so a reader can tell which file a run obeyed without
// knowing the digest to expect.
type Policy struct {
	Path string `json:"path"`
	// Source is how Path was chosen: PolicySourceDefault (the runner
	// home's policy.yaml), PolicySourceFlag (--policy) or PolicySourceEnv
	// (the runner home relocated by AGENTRY_RUNNER_HOME). An environment
	// variable is inherited silently, where a flag shows in the command,
	// so the receipt says which it was.
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
}

// The policy sources a receipt records (policy.Resolve chooses one).
const (
	PolicySourceDefault = "default"
	PolicySourceFlag    = "flag"
	PolicySourceEnv     = "env"
)

// The consents a receipt records.
const (
	ConsentAllowUnsigned  = "--allow-unsigned"
	ConsentAllowUnscanned = "--allow-unscanned"
)

// Harness identifies what looped the model.
type Harness struct {
	Adapter string `json:"adapter"`
	Image   string `json:"image"`
	// Host is the executable (and version, when known) a host-run
	// harness adapter ran; empty for a container harness.
	Host  string `json:"host,omitempty"`
	Model string `json:"model"`
	// Mode is the run mode (autonomous or interactive); empty in
	// envelopes written before run modes existed.
	Mode string `json:"mode,omitempty"`
}

// Usage is the measured cost of the run.
type Usage struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	// TotalTokens is set when the harness reports context tokens without an
	// input/output split (docker-agent's ACP usage_update); input and output
	// are then zero, not estimates (agentry-cli#71).
	TotalTokens int64   `json:"totalTokens,omitempty"`
	CostUSD     float64 `json:"costUsd"`
	WallMS      int64   `json:"wallMs"`
	// Turns counts tool-call events (the --max-turns meter).
	Turns int `json:"turns"`
	// UserTurns counts the user's messages in an interactive session.
	UserTurns int `json:"userTurns,omitempty"`
}

// Total is the run's token count as a meter should read it: the harness's
// own total where it reports one, otherwise input plus output. The two
// shapes are exclusive — a harness that splits input and output leaves
// TotalTokens zero, and one that reports only context tokens leaves the
// split zero — so this is the single rule for anything counting a run's
// tokens, live or at settlement (agentry-cli#138).
func (u Usage) Total() int64 {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.InputTokens + u.OutputTokens
}

// Receipts locates the audit artifacts.
type Receipts struct {
	// Dir is <out>/receipts.
	Dir string `json:"dir"`
	// RunPlanSHA256 is the §R4 plan hash — the receipt's link back to
	// exactly what was compiled and enforced.
	RunPlanSHA256 string `json:"runPlanSha256"`
}

// TextResult wraps plain text as the Result field's JSON.
func TextResult(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		// Marshaling a string cannot fail; keep the signature honest.
		return json.RawMessage(`""`)
	}
	return b
}

// ExitClassName names a §R8 exit code; the table mirrors engine's constants
// (engine imports this package, so the names live here), and engine's tests
// hold the two together.
func ExitClassName(code int) string {
	switch code {
	case 0:
		return "ok"
	case 1:
		return "cli"
	case 2:
		return "usage"
	case 10:
		return "verify"
	case 11:
		return "policy"
	case 12:
		return "compile"
	case 13:
		return "gateway"
	case 14:
		return "harness"
	case 15:
		return "timeout"
	case 16:
		return "budget"
	case 17:
		return "outputSchema"
	}
	return fmt.Sprintf("exit-%d", code)
}

// The steps of a run as Stage names them, in about the order a run meets
// them: the policy read; the release verified and installed; its
// manifest read, the adapter chosen and the plan compiled (compile); its
// capabilities consented to; the declared MCP servers checked, resolved
// and installed (mcp-servers, on both sides of the compile); the images
// installed; the profile loaded; the intake answered; the run's
// directories and secrets prepared; the proxy, gateway and probe started;
// the agent run; and its output checked. A draft adds one step after
// the policy: its description read by AI Review (agentry-cli#384), which
// stops the Drafter before anything else runs when it does not pass.
const (
	StagePolicy      = "policy"
	StageDescription = "description"
	StageVerify      = "verify"
	StageConsent     = "consent"
	StageMCPServers  = "mcp-servers"
	StageCompile     = "compile"
	StageImages      = "images"
	StageProfile     = "profile"
	StageIntake      = "intake"
	StagePrepare     = "prepare"
	StageInfra       = "infra"
	StageAgent       = "agent"
	StageOutput      = "output"
)

// Marshal renders the envelope for stdout and result.json, deriving the
// exit class name and the result file's path from what is already set.
func (e *Envelope) Marshal() ([]byte, error) {
	e.ExitClass = ExitClassName(e.ExitCode)
	if e.OutDir != "" {
		e.ResultFile = filepath.Join(e.OutDir, "result.json")
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal envelope: %w", err)
	}
	return append(b, '\n'), nil
}

// Write stores the envelope as <outDir>/result.json.
func (e *Envelope) Write(outDir string) error {
	b, err := e.Marshal()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "result.json"), b, 0o644); err != nil { //nolint:gosec // the run's own output folder
		return fmt.Errorf("write result.json: %w", err)
	}
	return nil
}
