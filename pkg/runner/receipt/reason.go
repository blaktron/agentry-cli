package receipt

import (
	"context"
	"errors"
	"regexp"
	"slices"
)

// Reason says why a failed run stopped, in fixed words:
// a code from a closed list, the release's own declared names it concerns
// (the commands, MCP servers or secrets its manifest declares), and for
// some codes a detail from that code's own closed list. It carries no text
// the run, the model, the package's author or the person wrote, and no
// path, so a driver that may keep nothing else (a hosted run's host keeps
// no log) can keep it and still say why. Stage says where a run
// stopped; Reason, when the step that decided it knows, says why.
type Reason struct {
	Code   ReasonCode `json:"code"`
	Names  []string   `json:"names,omitempty"`
	Detail string     `json:"detail,omitempty"`
	// Count is the count a refusal over a cap names, in typed fields: only on
	// input-refused with source-cap, or not-reviewed for a source's Markdown.
	Count *ReasonCount `json:"count,omitempty"`
}

// ReasonCount is a cap refusal's count: which cap refused and its value,
// and what the step counted and how many. Numbers and closed words only,
// so a driver that keeps nothing the run wrote can say it. What was
// counted need not be what the cap counts: a repository's pack is refused
// by its objects against the file cap, or at a bound past the byte cap
// (More), before its files are known.
type ReasonCount struct {
	// Cap is the cap that refused: CapFiles, CapBytes or CapAIReview.
	Cap string `json:"cap"`
	// Limit is the cap's value; for CapAIReview, how many of the source's
	// Markdown files fit in what AI Review reads of a run.
	Limit int64 `json:"limit"`
	// Of is what was counted (Of*), and Count how many.
	Of    string `json:"of"`
	Count int64  `json:"count"`
	// More says the step stopped counting at Count: there are more.
	More bool `json:"more,omitempty"`
}

// The caps a ReasonCount names.
const (
	// CapFiles: a source's file cap.
	CapFiles = "files"
	// CapBytes: a source's byte cap.
	CapBytes = "bytes"
	// CapAIReview: what AI Review reads of a run (its calls and its text),
	// which a source's Markdown is read whole within or refused.
	CapAIReview = "ai-review"
)

// What a ReasonCount counted.
const (
	// OfFiles and OfBytes: a repository's files, or a source's bytes.
	OfFiles = "files"
	OfBytes = "bytes"
	// OfObjects: the objects in a repository's pack, more than a
	// repository within the file cap holds.
	OfObjects = "objects"
	// OfEntries: a repository's files, folders and links together.
	OfEntries = "entries"
	// OfPackBytes and OfInflatedBytes: a repository's pack, as sent and
	// as inflated, more than a repository within the byte cap makes.
	OfPackBytes     = "pack-bytes"
	OfInflatedBytes = "inflated-bytes"
	// OfMarkdownFiles: a source's Markdown files, which AI Review reads.
	OfMarkdownFiles = "markdown-files"
)

// countOf is each cap's closed list of what may be counted against it.
var countOf = map[string][]string{
	CapFiles:    {OfFiles, OfObjects, OfEntries},
	CapBytes:    {OfBytes, OfPackBytes, OfInflatedBytes},
	CapAIReview: {OfMarkdownFiles},
}

// countDetails are the details of ReasonInputRefused that carry a count,
// and sourceDetails those that may name a source: no other input refusal
// names anything, whatever its names look like.
var (
	countDetails  = []string{InputSourceCap, InputNotReviewed}
	sourceDetails = []string{InputSourceCap, InputSourceFetch, InputNotReviewed}
)

// maxCount bounds a count, within what a JSON number holds exactly.
const maxCount = 1 << 53

// CountsAgainst says whether of is a word a count against cap may count,
// for the step that reports one (pkg/runner/sources holds the fetch
// step's report to it).
func CountsAgainst(cap, of string) bool {
	return slices.Contains(countOf[cap], of)
}

// clean is c held to the closed lists, or nil.
func (c *ReasonCount) clean() *ReasonCount {
	if c == nil || !CountsAgainst(c.Cap, c.Of) ||
		c.Count < 0 || c.Count > maxCount || c.Limit < 0 || c.Limit > maxCount {
		return nil
	}
	out := *c
	return &out
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
	// ReasonInputRefused: an input given to the run was refused before the
	// agent started (prepare). Detail: why. Names: an input source's question
	// id, for a source's refusal; never a file name: those are the person's.
	// Count: a cap's, for source-cap and a source's not-reviewed.
	ReasonInputRefused ReasonCode = "input-refused"
	// ReasonInputsNotDecoded: the decode step could not run (prepare), a
	// fault of this machine and not of the inputs. Detail: which part.
	ReasonInputsNotDecoded ReasonCode = "inputs-not-decoded"
	// ReasonAgentBlocked: the agent ended the run as blocked with the
	// runner's end_blocked tool (agent): the task could
	// not be done with what the run had. Its own sentence is in the error
	// and the receipts, never here: the model wrote it.
	ReasonAgentBlocked ReasonCode = "agent-blocked"
	// ReasonRunnerBlocked: the runner ended the run as blocked on its own
	// evidence (agent): the runner's tools failed the agent. Detail: what the
	// evidence was.
	ReasonRunnerBlocked ReasonCode = "runner-blocked"
	// ReasonSignInMissing: the run spends a model sign-in and was given none,
	// or none it could use (prepare). Never on a hosted run, which never spends
	// one.
	ReasonSignInMissing ReasonCode = "signin-missing"
	// ReasonPlanLimit: the person's plan refused the model's calls for its
	// usage limit (agent). The run is not retried; the vendor's words, and its
	// reset time when it gave one, are in the error and the receipts.
	ReasonPlanLimit ReasonCode = "plan-limit"
	// ReasonOrgPolicy: a policy of the person's organization or enterprise at
	// the model vendor refused the run (agent). Detail: which policy
	// (OrgPolicy*). The vendor's words are in the error and the receipts.
	ReasonOrgPolicy ReasonCode = "org-policy"
	// ReasonOutOfCredits: the model's provider refused the model's calls
	// because the account that pays has no credits left, an HTTP 402 or its
	// words for one (agent). The run is not retried; the provider's words are
	// in the error and the receipts.
	ReasonOutOfCredits ReasonCode = "out-of-credits"
	// ReasonAnswerInReasoning: the release's output is schema-checked and
	// the model ended the run on its reasoning, with no final message after
	// it (output; exit 17). Reasoning is never taken as
	// the result; the events receipt keeps it.
	ReasonAnswerInReasoning ReasonCode = "answer-in-reasoning"
	// ReasonHostRefused: a target host the person named was not reached
	// (prepare or infra). Names: the host questions' ids; Detail: why (Host*).
	// Never the host's name or address: those are the person's.
	ReasonHostRefused ReasonCode = "host-refused"
)

// The details of ReasonHostRefused.
const (
	// HostAddress: the address is one a target host never is (loopback,
	// link-local), the policy's hosts lists exclude it, or it did not
	// resolve to an IPv4 address.
	HostAddress = "host-address"
	// HostKeyUnknown: the host's key is on no file and no one trusted it.
	HostKeyUnknown = "host-key-unknown"
	// HostKeyChanged: the host's key differs from the one on file.
	HostKeyChanged = "host-key-changed"
	// HostAuth: the host did not accept the sign-in.
	HostAuth = "host-auth"
	// HostCredential: no sign-in was given, or the one given could not be
	// used (a key it cannot read, a passphrase that does not unlock it).
	HostCredential = "host-credential"
	// HostUnreachable: the host did not answer on its port.
	HostUnreachable = "host-unreachable"
	// HostHosted: a hosted run never reaches a target host.
	HostHosted = "hosted"
)

// The details of ReasonOrgPolicy: the organization's policy that refused.
const (
	// OrgPolicyClient: the policy that turns the vendor's client (Copilot
	// CLI) off for the person, or bars their account from it.
	OrgPolicyClient = "client-disabled"
	// OrgPolicyManagedApproval: the organization's managed settings require
	// a person to approve each tool call, which a run's driver, approving
	// the run's own tools itself, does not do.
	OrgPolicyManagedApproval = "managed-approval"
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
	// the agent (a hosted run's review). The review ran,
	// so a hosted run settles AI Review's credit alone.
	InputFlagged = "ai-review"
	// InputNotReviewed: AI Review could not read an input's text (the
	// registry did not answer, refused the call, or the run stopped
	// first), and no text it has not read reaches the agent: the
	// platform's fault, refunded.
	InputNotReviewed = "not-reviewed"
	// InputSourceCap: an input source is over its cap: refused with its count,
	// never staged in part.
	InputSourceCap = "source-cap"
	// InputSourceFetch: an input source could not be fetched: the host
	// refused it, has no such repository or ref, or could not be reached.
	InputSourceFetch = "source-fetch"
)

// The details of ReasonInputsNotDecoded.
const (
	// DecodeImage: the decode image could not be made ready.
	DecodeImage = "image"
	// DecodeRun: the decode container ran and failed.
	DecodeRun = "run"
)

// The details of ReasonRunnerBlocked: the runner's evidence, each a fault of
// the run's machinery rather than of the agent (such a run is refunded when
// hosted).
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
// holds a reason to the same list.
const MaxReasonNames = 8

// nameKind is the grammar a code's names are held to.
type nameKind int

const (
	namesNone nameKind = iota
	namesCommand
	namesServer
	namesSecret
	namesQuestion
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
	ReasonMCPStartFailed:    {names: namesServer},
	ReasonSecretMissing:     {names: namesSecret},
	ReasonInputRefused:      {names: namesQuestion, details: []string{InputMalware, InputNameClash, InputUnreadable, InputFlagged, InputNotReviewed, InputSourceCap, InputSourceFetch}},
	ReasonInputsNotDecoded:  {details: []string{DecodeImage, DecodeRun}},
	ReasonAgentBlocked:      {},
	ReasonRunnerBlocked:     {details: []string{BlockedToolsUnrecorded, BlockedInputsUnread, BlockedReportUnwritten}},
	ReasonSignInMissing:     {},
	ReasonPlanLimit:         {},
	ReasonOrgPolicy:         {details: []string{OrgPolicyClient, OrgPolicyManagedApproval}},
	ReasonOutOfCredits:      {},
	ReasonAnswerInReasoning: {},
	ReasonHostRefused: {names: namesQuestion, details: []string{
		HostAddress, HostKeyUnknown, HostKeyChanged, HostAuth, HostCredential, HostUnreachable, HostHosted,
	}},
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
	// An intake question's id, in the core's rule.
	questionRE = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
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
	case namesQuestion:
		return questionRE.MatchString(name)
	}
	return false
}

// Clean holds r to the closed list: nil for an unknown code; a detail
// outside the code's list is dropped, and so is each name outside the
// code's grammar, a repeated name, every name past MaxReasonNames, and a
// count outside its lists or on a code and detail that carry none. A
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
	if r.Code == ReasonInputRefused {
		if slices.Contains(countDetails, out.Detail) {
			out.Count = r.Count.clean()
		}
		if !slices.Contains(sourceDetails, out.Detail) {
			return out
		}
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

// WithCount attaches a reason with a cap's count to err; nil stays nil.
func WithCount(err error, code ReasonCode, detail string, count *ReasonCount, names ...string) error {
	if err == nil {
		return nil
	}
	return &reasonError{Reason: Reason{Code: code, Detail: detail, Names: names, Count: count}, Err: err}
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
