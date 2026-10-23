// Package receipt defines the run result envelope — the
// runtime counterpart of the registry's public scan report. Every claim in
// it is either verified upstream (ref, digest, attestation), compiled
// (capabilities_enforced, run plan hash), or measured (usage); nothing in
// it is ever a secret value or a gateway token.
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
// the output folder keeps the full record.
type Envelope struct {
	// Agentry is EnvelopeVersion.
	Agentry string `json:"agentry"`
	// Status is "ok" or "error"; ExitCode is the class the process
	// exits with; Error carries the failure detail for status error.
	Status   string `json:"status"`
	ExitCode int    `json:"exitCode"`
	// ExitClass names ExitCode (ok, usage, verify, policy, compile, gateway,
	// harness, timeout, budget, outputSchema, blocked, cli): a client acts
	// on the name, never on the number. Set by Marshal.
	ExitClass string `json:"exitClass"`
	Error     string `json:"error,omitempty"`
	// Stage names the step a failed run stopped in, one word from a fixed list
	// (the Stage constants), so a driver that may not keep the error's text (a
	// hosted run's host keeps nothing the run could have written) can still say
	// where it failed. It says where; ExitClass says what: a policy refusal
	// while the declared MCP servers are checked is class policy in stage
	// mcp-servers. Empty on success.
	Stage string `json:"stage,omitempty"`
	// Reason says why, in fixed words, when the step that stopped the run
	// decided it: a code from a closed list and the release's own declared
	// names it concerns, never text (Reason). Absent on success and on a
	// failure no step named.
	Reason *Reason `json:"reason,omitempty"`
	// RefusedInputs names the inputs an input-refused run was refused for,
	// in typed fields (RefusedInput), at most MaxRefusedInputs of them;
	// RefusedInputsMore counts the rest. Absent on every other envelope.
	RefusedInputs     []RefusedInput `json:"refusedInputs,omitempty"`
	RefusedInputsMore int            `json:"refusedInputsMore,omitempty"`
	// RunID names the run (container labels, run dirs).
	RunID string `json:"runId"`
	// Ref and Digest identify the exact release that ran.
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
	// Attestation reports the install-gate outcome the run inherited.
	Attestation Attestation `json:"attestation"`
	// CapabilitiesEnforced names each contract line's mechanism.
	CapabilitiesEnforced map[string]string `json:"capabilities_enforced"`
	// Harness identifies the adapter, image, and model that ran.
	Harness Harness `json:"harness"`
	// Policy names the operator policy file the run enforced: its path,
	// how that path was chosen, and the digest the plan was compiled
	// against. Absent in envelopes from clients that do
	// not say, and in those written before it existed.
	Policy *Policy `json:"policy,omitempty"`
	// Usage is measured from the harness event stream, best-effort where
	// the stream is.
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
	// class verify), so a client can show the files and
	// offer the repair without parsing Error. Absent otherwise.
	InstalledChanges *InstalledChanges `json:"installedChanges,omitempty"`
	// Web counts what the built-in Drafter's web tools read and what AI Review
	// withheld, from the run's web ledger (receipts/web.ndjson, which names
	// each address and verdict). Absent for a run without the web tools.
	Web *Web `json:"web,omitempty"`
	// Review is AI Review's record of a run with the review grant: what of
	// the inputs it scanned before the model, and the rubric calls; absent
	// for any other run.
	Review *Review `json:"review,omitempty"`
	// PDFs is what export_pdf wrote: the report design every PDF of the run was
	// laid out with, and each PDF in reports/ whose bytes are the ones the
	// tools server recorded, with its form. A reader opens in place only a PDF
	// this names with a matching SHA-256; any other is download-only. Absent
	// for a run that exported none.
	PDFs *PDFs `json:"pdfs,omitempty"`
	// FetchSaves counts the response bodies fetch saved in the scratch folder:
	// those the agent asked for with saveTo and those larger than one tool
	// result. The folder is gone with the run; the ledger it names lists each
	// file, the declared host it came from, its size and its SHA-256. Absent
	// for a run that saved none.
	FetchSaves *FetchSaves `json:"fetchSaves,omitempty"`
	// Deliverables are the files the run was for: the ones the agent marked, or
	// else the runner's pick (Deliverables.Files' Marked says which), each by
	// path under reports/ with the SHA-256 the runner computed once the agent
	// had ended. Only this block names a deliverable; a client checks a file
	// against its digest before it copies, opens or sends it. Absent for a run
	// that failed and for one with nothing to list.
	Deliverables *Deliverables `json:"deliverables,omitempty"`
	// Deliveries records each copy of a deliverable the run itself made (a
	// folder given with --deliver-to), one entry a file. A delivery that failed
	// is recorded and never changes the run's outcome. Absent when the run
	// delivered nothing.
	Deliveries []Delivery `json:"deliveries,omitempty"`
	// Persistent is a persistent agent's segment: which agent and which of
	// its starts this was, whether the conversation continued from the
	// segment before, and the volumes that hold it between segments. Absent
	// for every other run.
	Persistent *Persistent `json:"persistent,omitempty"`
}

// Persistent is the envelope's record of one segment of a persistent agent.
type Persistent struct {
	Agent   string `json:"agent"`
	Segment int    `json:"segment"`
	// Resumed says the harness continued the stored conversation; when it
	// did not, ResumeReason says why (a first segment, a store the harness
	// could not load, a harness that cannot continue one) and the runner
	// gave the new conversation the last exchange instead.
	Resumed      bool              `json:"resumed"`
	ResumeReason string            `json:"resumeReason,omitempty"`
	Volumes      PersistentVolumes `json:"volumes"`
	// Share is the share folder the segment mounted; empty when the agent
	// had it off.
	Share string `json:"share,omitempty"`
	// Roster is what the agent had for the segment, each entry with its
	// switch and its consent sentence, as every client shows it.
	Roster []RosterEntry `json:"roster,omitempty"`
}

// RosterEntry is one entry of a persistent agent's roster as a segment's
// envelope records it.
type RosterEntry struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	GrantedBy string `json:"grantedBy"`
	On        bool   `json:"on"`
	Line      string `json:"line"`
	// State is set for an entry that is on and that the segment was not
	// given, and then overrides On: "installing" for a server the agent's
	// store does not hold yet, "failed" for one that could not be installed
	// or started, "needs-set-up" for an offered server that waits for a
	// stored secret or an account. The plan's servers are what the segment
	// ran.
	State string `json:"state,omitempty"`
	// Share says a server's switch to see the agent's share folder is on.
	Share bool `json:"share,omitempty"`
}

// PersistentVolumes names a persistent agent's two volumes: the harness's
// conversation store, mounted for every segment, and the one made for the
// agent's files.
type PersistentVolumes struct {
	Session string `json:"session"`
	Data    string `json:"data"`
	// Memory is the agent's memory volume, for an agent that keeps one.
	Memory string `json:"memory,omitempty"`
}

// MaxDeliverables is the most files Deliverables lists.
const MaxDeliverables = 200

// Deliverables is a run's deliverables.
type Deliverables struct {
	Files []Deliverable `json:"files"`
	// Truncated says the run had more than MaxDeliverables files to list and
	// the rest are not named.
	Truncated bool `json:"truncated,omitempty"`
}

// Deliverable is one file the run was for. Path is relative to the output
// folder (reports/…). Title is one line of plain text: the agent's, or the
// file's name. Type is the file name's extension in lower case, without its
// dot, empty when it has none; it says nothing of the bytes. Marked is false
// when the runner chose the file because the agent marked none.
type Deliverable struct {
	Path   string `json:"path"`
	Title  string `json:"title"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Type   string `json:"type"`
	Marked bool   `json:"marked"`
}

// The kinds of destination a Delivery names, and how a delivery ended.
const (
	DeliveryKindFolder  = "folder"
	DeliveryKindWebhook = "webhook"
	DeliveryKindS3      = "s3"

	DeliveryDelivered = "delivered"
	// DeliveryRefused: the file was not the one the envelope names, or not a
	// file a delivery may read or make (a changed file, a path outside
	// reports/, a link, a type the system acts on when a folder is viewed).
	// Nothing was left in the destination for it.
	DeliveryRefused = "refused"
	// DeliveryFailed: the file could not be written to the destination.
	DeliveryFailed = "failed"
)

// The closed reasons a Delivery's Error gives, never a system's own text: a
// path or a message could name the person's folders or the agent's words.
const (
	DeliveryErrChanged     = "changed"
	DeliveryErrMissing     = "missing"
	DeliveryErrOutside     = "outside-reports"
	DeliveryErrLink        = "link"
	DeliveryErrUnreadable  = "unreadable"
	DeliveryErrType        = "type"
	DeliveryErrDestination = "destination"
	DeliveryErrWrite       = "write"
	// DeliveryErrNotMarked says a file was written but the system's flag for a
	// download (Mark-of-the-Web, the quarantine attribute) could not be set on
	// it, so a client must not open it in one click.
	DeliveryErrNotMarked = "not-marked"
	// The words a send to a destination adds. TooLarge: a file the webhook's
	// body had no room for, listed in it with its digest and no content, or a
	// file past what a bucket takes as one object.
	// Credential: the destination's secret or header value was missing or not
	// one a request may carry. Address: the destination's host, or the address
	// it resolves to, is not one the step may reach (private, loopback or
	// link-local without "On my network", or a name no proxy admits). TLS: the
	// receiver's certificate was not trusted. Unreachable: nothing answered.
	// Timeout: the receiver did not finish in time. Redirect: the receiver
	// answered with a redirect, which is never followed. Status: the receiver
	// answered with an error status. Step: the delivery step itself failed and
	// left no report.
	DeliveryErrTooLarge    = "too-large"
	DeliveryErrCredential  = "credential"
	DeliveryErrAddress     = "address"
	DeliveryErrTLS         = "tls"
	DeliveryErrUnreachable = "unreachable"
	DeliveryErrTimeout     = "timeout"
	DeliveryErrRedirect    = "redirect"
	DeliveryErrStatus      = "status"
	DeliveryErrStep        = "step"
)

// DeliveryErrors is every word a Delivery's Error may be.
var DeliveryErrors = []string{
	DeliveryErrChanged, DeliveryErrMissing, DeliveryErrOutside, DeliveryErrLink, DeliveryErrUnreadable, DeliveryErrType,
	DeliveryErrDestination, DeliveryErrWrite, DeliveryErrNotMarked, DeliveryErrTooLarge, DeliveryErrCredential,
	DeliveryErrAddress, DeliveryErrTLS, DeliveryErrUnreachable, DeliveryErrTimeout, DeliveryErrRedirect, DeliveryErrStatus,
	DeliveryErrStep,
}

// Delivery is one file's copy or send to a destination, as
// receipts/deliveries.ndjson holds it and the envelope repeats it. For a
// folder, Destination is the folder's absolute path, Object the copy's
// absolute path and Host empty. For a webhook, Destination is the name the
// person saved it under, Host the host and port reached, and Object the
// deliverable's path (or "result" for a run that left no file and sent its
// result alone, whose SHA256 is that of the result as sent, or empty when
// there was none). For a bucket, Host is the service's host and port, and
// Object the object written, <bucket>/<key>. OnMyNetwork says the destination
// was saved with "On my network", so the step was allowed to reach a
// private-network address.
type Delivery struct {
	Destination string `json:"destination"`
	Kind        string `json:"kind"`
	Host        string `json:"host,omitempty"`
	Object      string `json:"object,omitempty"`
	SHA256      string `json:"sha256"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	OnMyNetwork bool   `json:"onMyNetwork,omitempty"`
}

// FetchSaves is a run's count of the bodies fetch saved.
type FetchSaves struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
	// Asked is how many of Files the agent named a path for (saveTo).
	Asked int `json:"asked"`
	// Ledger is the ledger's path in the output folder.
	Ledger string `json:"ledger"`
}

// PDFs is a run's exported PDFs. The block keeps its key "theme" for the
// design, which the site's and the desktop's readers know by that name.
type PDFs struct {
	Theme PDFTheme  `json:"theme"`
	Files []PDFFile `json:"files"`
}

// PDFTheme names a report design (theme document version 2): its name, the
// built-in theme it is based on, its version, the SHA-256 of its canonical
// document (pkg/reportdesign's SHA256), and what chose it.
type PDFTheme struct {
	Name    string `json:"name"`
	BasedOn string `json:"basedOn,omitempty"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	From    string `json:"from,omitempty"`
}

// PDFFile is one exported PDF. Path is relative to the output folder
// (reports/…). Form is the form the agent chose: report, letter or memo.
type PDFFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Pages  int    `json:"pages"`
	Bytes  int64  `json:"bytes"`
	Form   string `json:"form,omitempty"`
	// Logo says the person's logo was drawn in the PDF's title block;
	// the design's SHA-256 covers the logo's bytes.
	Logo bool `json:"logo,omitempty"`
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
	// for the record; AI Review is 1 credit an operation.
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
	// cost the judge, for the record; AI Review is 1 credit an operation.
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

// Attestation is the verify-gate outcome.
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
	// Build is, for state builtin, the build of the application that carried
	// the package: a built-in has no registry digest to name its provenance, so
	// the receipt names the binary.
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
	Host string `json:"host,omitempty"`
	// ImagePin is how Image was pinned: "digest sha256:…" for a published
	// image, "artifact sha256:…" for one built on the machine from its
	// vendor's artifact, or "tag (not pinned: …)". Empty for a host harness.
	ImagePin string `json:"imagePin,omitempty"`
	// CredentialKind is the kind of credential that paid for the model:
	// api, subscription, or a model sign-in's kind (chatgpt-plan,
	// claude-signin, copilot-signin). A word of that closed list, never an
	// account or a token.
	CredentialKind string `json:"credentialKind,omitempty"`
	Model          string `json:"model"`
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
	// are then zero, not estimates.
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
// tokens, live or at settlement.
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
	// RunPlanSHA256 is the plan hash — the receipt's link back to
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

// ExitClassName names an exit code; the table mirrors engine's constants
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
	case 18:
		return "blocked"
	}
	return fmt.Sprintf("exit-%d", code)
}

// The steps of a run as Stage names them, in about the order a run meets them:
// the policy read; the release verified and installed; its manifest read, the
// adapter chosen and the plan compiled (compile); its capabilities consented
// to; the declared MCP servers checked, resolved and installed (mcp-servers, on
// both sides of the compile); the images installed; the profile loaded; the
// intake answered; the run's directories and secrets prepared; the proxy,
// gateway and probe started; the agent run; and its output checked. A draft
// adds one step after the policy: its description read by AI Review, which
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
