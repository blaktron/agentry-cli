package receipt

import (
	"context"
	"errors"
	"regexp"
	"slices"
)

// Reason says why a failed run stopped, in fixed words (agentry-cli#628):
// a code from a closed list, the release's own declared names it concerns
// (the commands, MCP servers or secrets its manifest declares), and for
// some codes a detail from that code's own closed list. It carries no text
// the run, the model, the package's author or the person wrote, and no
// path, so a driver that may keep nothing else (a hosted run's host keeps
// no log, DEV-130) can keep it and still say why. Stage says where a run
// stopped; Reason, when the step that decided it knows, says why.
type Reason struct {
	Code   ReasonCode `json:"code"`
	Names  []string   `json:"names,omitempty"`
	Detail string     `json:"detail,omitempty"`
}

// ReasonCode is one word of the closed list below.
type ReasonCode string

// The reason codes, each decided by one step of the run.
const (
	// ReasonExecNotProvided: the release declares exec commands this
	// machine's policy does not provide (compile). Names: the commands.
	ReasonExecNotProvided ReasonCode = "exec-not-provided"
	// ReasonMCPVulnerable: the registry's vulnerability scan refuses a
	// declared server and nothing overrode it (mcp-servers). Names: the
	// servers.
	ReasonMCPVulnerable ReasonCode = "mcp-vulnerable"
	// ReasonMCPNotResolved: a declared server's pin was not found in, or
	// could not be read from, the configured registry (mcp-servers).
	// Names: the server.
	ReasonMCPNotResolved ReasonCode = "mcp-not-resolved"
	// ReasonMCPRuntimeExcluded: a declared server's own runtime range
	// (npm's engines.node) excludes the runtime the run would use
	// (mcp-servers). Names: the server.
	ReasonMCPRuntimeExcluded ReasonCode = "mcp-runtime-excluded"
	// ReasonMCPInstallFailed: a declared server could not be installed
	// (mcp-servers). Names: the server; Detail: the install's step.
	ReasonMCPInstallFailed ReasonCode = "mcp-install-failed"
	// ReasonMCPStartFailed: the gateway could not start a declared server
	// (infra). Names: the server.
	ReasonMCPStartFailed ReasonCode = "mcp-start-failed"
	// ReasonSecretMissing: declared secrets have no value from the
	// profile or the policy's secret source (prepare). Names: the
	// secrets' names, never a value.
	ReasonSecretMissing ReasonCode = "secret-missing"
	// ReasonInputRefused: an input given to the run was refused before
	// the agent started (prepare). Detail: why. Never a file name: those
	// are the person's.
	ReasonInputRefused ReasonCode = "input-refused"
	// ReasonInputsNotDecoded: the decode step could not run (prepare), a
	// fault of this machine and not of the inputs. Detail: which part.
	ReasonInputsNotDecoded ReasonCode = "inputs-not-decoded"
	// ReasonAgentBlocked: the agent ended the run as blocked with the
	// runner's end_blocked tool (agent; agentry-cli#629): the task could
	// not be done with what the run had. Its own sentence is in the error
	// and the receipts, never here: the model wrote it.
	ReasonAgentBlocked ReasonCode = "agent-blocked"
	// ReasonRunnerBlocked: the runner ended the run as blocked on its own
	// evidence (agent; agentry-cli#629): the runner's tools failed the
	// agent. Detail: what the evidence was.
	ReasonRunnerBlocked ReasonCode = "runner-blocked"
	// ReasonSignInMissing: the run spends a model sign-in and was given
	// none, or none it could use (prepare; agentry-cli#738). Never on a
	// hosted run, which never spends one.
	ReasonSignInMissing ReasonCode = "signin-missing"
	// ReasonPlanLimit: the person's plan refused the model's calls for its
	// usage limit (agent; plan model-sign-ins.md D12, invariant I9). The
	// run is not retried; the vendor's words, and its reset time when it
	// gave one, are in the error and the receipts.
	ReasonPlanLimit ReasonCode = "plan-limit"
)

// The details of ReasonMCPInstallFailed: the install's step that failed.
const (
	// InstallPinChanged: the signature's store holds another pin than
	// the registry now resolves.
	InstallPinChanged = "pin-changed"
	// InstallStoreChanged: the store was written to since it was
	// installed.
	InstallStoreChanged = "store-changed"
	// InstallImage: the runtime image could not be found or pulled.
	InstallImage = "image"
	// InstallProxy: the install proxy (the builder's only route out)
	// did not start.
	InstallProxy = "install-proxy"
	// InstallBuilder: the builder ran and failed.
	InstallBuilder = "builder"
	// InstallRuntimeCheck: the server's runtime range could not be
	// checked in the runtime image.
	InstallRuntimeCheck = "runtime-check"
	// InstallRecord: the installed store could not be hashed or recorded.
	InstallRecord = "record"
)

// The details of ReasonInputRefused.
const (
	// InputMalware: ClamAV found malware in an input.
	InputMalware = "malware"
	// InputNameClash: two inputs share a name, or one takes a name the
	// run reserves.
	InputNameClash = "name-clash"
	// InputUnreadable: an input could not be read or staged.
	InputUnreadable = "unreadable"
	// InputFlagged: AI Review flagged an input's text as trying to steer
	// the agent (a hosted run's review, agentry-cli#321). The review ran,
	// so a hosted run settles AI Review's credit alone (DEV-150).
	InputFlagged = "ai-review"
	// InputNotReviewed: AI Review could not read an input's text (the
	// registry did not answer, refused the call, or the run stopped
	// first), and nothing it has not read reaches the agent: the
	// platform's fault, refunded.
	InputNotReviewed = "not-reviewed"
	// InputReviewLimit: the inputs' text is more than AI Review reads of
	// one run (engine.MaxInputReviewText); nothing was asked.
	InputReviewLimit = "review-limit"
)

// The details of ReasonInputsNotDecoded.
const (
	// DecodeImage: the decode image could not be made ready.
	DecodeImage = "image"
	// DecodeRun: the decode container ran and failed.
	DecodeRun = "run"
)

// The details of ReasonRunnerBlocked: the runner's evidence, each a fault
// of the run's machinery rather than of the agent (plan
// quickagents-that-run.md D5: such a run is refunded hosted).
const (
	// BlockedToolsUnrecorded: the runner's tools could not write the run's
	// output folder at all, not even their own ledger, so neither an input
	// read nor a report could have reached the run.
	BlockedToolsUnrecorded = "tools-unrecorded"
	// BlockedInputsUnread: the agent called the input tools and every
	// call failed.
	BlockedInputsUnread = "inputs-unread"
	// BlockedReportUnwritten: the agent called write_report and every
	// call failed.
	BlockedReportUnwritten = "report-unwritten"
)

// MaxReasonNames is how many names a reason keeps; the error's text, where
// it is kept, names the rest. Exported, with ReasonCodes, for a driver that
// holds a reason to the same list (agentry-enterprise's hostapi.ReceiptOf).
const MaxReasonNames = 8

// nameKind is the grammar a code's names are held to.
type nameKind int

const (
	namesNone nameKind = iota
	namesCommand
	namesServer
	namesSecret
)

// reasonCodes is the closed list: each code's name grammar and details.
var reasonCodes = map[ReasonCode]struct {
	names   nameKind
	details []string
}{
	ReasonExecNotProvided:    {names: namesCommand},
	ReasonMCPVulnerable:      {names: namesServer},
	ReasonMCPNotResolved:     {names: namesServer},
	ReasonMCPRuntimeExcluded: {names: namesServer},
	ReasonMCPInstallFailed: {names: namesServer, details: []string{
		InstallPinChanged, InstallStoreChanged, InstallImage, InstallProxy,
		InstallBuilder, InstallRuntimeCheck, InstallRecord,
	}},
	ReasonMCPStartFailed:   {names: namesServer},
	ReasonSecretMissing:    {names: namesSecret},
	ReasonInputRefused:     {details: []string{InputMalware, InputNameClash, InputUnreadable, InputFlagged, InputNotReviewed, InputReviewLimit}},
	ReasonInputsNotDecoded: {details: []string{DecodeImage, DecodeRun}},
	ReasonAgentBlocked:     {},
	ReasonRunnerBlocked:    {details: []string{BlockedToolsUnrecorded, BlockedInputsUnread, BlockedReportUnwritten}},
	ReasonSignInMissing:    {},
	ReasonPlanLimit:        {},
}

// ReasonCodes returns the closed list of codes, sorted.
func ReasonCodes() []ReasonCode {
	out := make([]ReasonCode, 0, len(reasonCodes))
	for c := range reasonCodes {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}

var (
	// A command as a release declares one, in the shape a program's name
	// takes; the manifest's own rule (one token) is wider, and a name
	// outside this is dropped rather than repeated.
	commandRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	// A declared server's ecosystem-qualified id (manifest SourceID), in
	// the core's npm and PyPI name rules (pkg/manifest validate.go).
	serverRE = regexp.MustCompile(`^(npm:(@[a-z0-9][a-z0-9-_.]*/)?[a-z0-9][a-z0-9-_.]*|pypi:[a-z0-9]([a-z0-9._-]*[a-z0-9])?)$`)
	// A secret's environment name, in the core's rule.
	secretRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

// maxServerName bounds a server's id: npm caps a package name at 214
// characters, and "npm:" and a scope's "@" fit in the rest.
const maxServerName = 230

// admits says whether name is in the kind's grammar.
func (k nameKind) admits(name string) bool {
	switch k {
	case namesCommand:
		return commandRE.MatchString(name)
	case namesServer:
		return len(name) <= maxServerName && serverRE.MatchString(name)
	case namesSecret:
		return secretRE.MatchString(name)
	}
	return false
}

// Clean holds r to the closed list: nil for an unknown code; a detail
// outside the code's list is dropped, and so is each name outside the
// code's grammar, a repeated name, and every name past MaxReasonNames. A
// driver that keeps a reason keeps what Clean answers, and nothing else.
func (r *Reason) Clean() *Reason {
	if r == nil {
		return nil
	}
	spec, ok := reasonCodes[r.Code]
	if !ok {
		return nil
	}
	out := &Reason{Code: r.Code}
	if slices.Contains(spec.details, r.Detail) {
		out.Detail = r.Detail
	}
	for _, n := range r.Names {
		if len(out.Names) == MaxReasonNames {
			break
		}
		if spec.names.admits(n) && !slices.Contains(out.Names, n) {
			out.Names = append(out.Names, n)
		}
	}
	return out
}

// reasonError carries a run's Reason beside the error that stopped it, so
// the step that decides a refusal names it and whoever writes the
// envelope finds it with ReasonOf, through any wrapping that keeps the
// chain.
type reasonError struct {
	Reason Reason
	Err    error
}

func (e *reasonError) Error() string { return e.Err.Error() }
func (e *reasonError) Unwrap() error { return e.Err }

// WithReason attaches a reason to err; nil stays nil.
func WithReason(err error, code ReasonCode, detail string, names ...string) error {
	if err == nil {
		return nil
	}
	return &reasonError{Reason: Reason{Code: code, Detail: detail, Names: names}, Err: err}
}

// ReasonOf is the reason err carries, held to the closed list by Clean;
// nil when it carries none, and when err is a cancellation: an
// interrupted run says so, not what the step it was in would have.
func ReasonOf(err error) *Reason {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	var re *reasonError
	if !errors.As(err, &re) {
		return nil
	}
	return re.Reason.Clean()
}
