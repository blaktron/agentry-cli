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
}

// Attestation is the verify-gate outcome (§R1).
type Attestation struct {
	// State is verified | unsigned | local.
	State string `json:"state"`
	// KeyID is the pinned signing key that verified (state verified).
	KeyID string `json:"keyId,omitempty"`
	// Verified is the boolean shorthand clients gate on.
	Verified bool `json:"verified"`
}

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
