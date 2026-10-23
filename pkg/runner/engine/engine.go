// Package engine executes a compiled run plan: render the plan's
// placeholder-bearing files into a per-run directory, resolve secrets,
// stand up the run-scoped network + proxy + gateway + agent containers,
// stream and budget the harness event stream, collect the result
// envelope, and tear everything down on every exit path. The engine
// trusts the plan (compile owns policy) and owns exactly the run-scoped
// values plans must not contain: paths, tokens, secret values.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/destination"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/intake"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// Exit codes. The distinguishable failure classes are the runner's
// composability contract: a cron job or pipeline branches on these, so
// each class has exactly one meaning.
const (
	ExitOK  = 0
	ExitCLI = 1
	// ExitUsage: the invocation cannot work as asked (interactive without a
	// terminal, an output format the mode cannot produce).
	ExitUsage        = 2
	ExitVerify       = 10
	ExitPolicy       = 11
	ExitCompile      = 12
	ExitGateway      = 13
	ExitHarness      = 14
	ExitTimeout      = 15
	ExitBudget       = 16
	ExitOutputSchema = 17
	// ExitBlocked: the agent could not do the task with what the run had,
	// by its own end_blocked or by the runner's evidence (blocked.go). A
	// release's one-shot run alone.
	ExitBlocked = 18
)

// ClassifiedError pairs an error with its exit class so main can exit
// precisely without string-matching.
type ClassifiedError struct {
	Code int
	Err  error
}

func (e *ClassifiedError) Error() string { return e.Err.Error() }
func (e *ClassifiedError) Unwrap() error { return e.Err }

// Classify wraps err with an exit class.
func Classify(code int, err error) *ClassifiedError {
	return &ClassifiedError{Code: code, Err: err}
}

// Params configures one run.
type Params struct {
	// Plan is the verified, compiled plan; PlanDir is where it and its
	// generated files live (install layout).
	Plan    *spec.Plan
	PlanDir string
	// WorkspaceSrc is the pristine verified workspace to copy per run.
	WorkspaceSrc string
	// CheckWorkspace, when set, is handed the run's copy of WorkspaceSrc
	// before anything reads it, and an error refuses the run with the
	// verify class: an installed release's copy is held to its integrity
	// record there (installdir.CheckCopy), so an edit made to the
	// install after the client's own check does not run.
	CheckWorkspace func(copyDir string) error
	// RunRoot is where runs/<id>/ ephemera live (the install dir).
	RunRoot string
	// RunnerHome is the runner home, shared by every client on the host.
	// A run whose plan needs relays holds its relay lock (RelayLockFile)
	// for the whole run. Empty takes no lock, leaving the relay listing
	// as the only check.
	RunnerHome string
	// OutDir is the mounted output folder, already absolute.
	OutDir string
	// Task is the task input (may be nil for an empty task). An empty
	// task on the autonomous path sends the agent's own instruction
	// instead.
	Task io.Reader
	// InputPaths are operator-provided files/directories copied into the
	// run's read-only inputs channel.
	InputPaths []string
	// Knowledge are the knowledge packs attached to the run: each copied
	// into the run's read-only /knowledge channel and verified there, the
	// three knowledge tools granted and the receipt's knowledge line
	// written only when there is one.
	Knowledge []AttachedPack
	// Voice is the person's Writing Voice when the run attaches it:
	// written under the run's /knowledge channel, voice_read granted and
	// the receipt's voice line written only then. A built-in's run never
	// carries one.
	Voice *AttachedVoice
	// DeliverTo, when set, is an absolute folder the run copies its
	// deliverables to once it has succeeded: <DeliverTo>/<QuickAgent>/<date
	// time> <title or run id>/, each file only after its SHA-256 is shown to be
	// the one the envelope's deliverables block names. The output folder is
	// left as it is; the copies are recorded in the envelope's deliveries
	// block and receipts/deliveries.ndjson. Empty delivers nothing.
	DeliverTo string
	// Destinations are the places the person asked the run to send its
	// result and deliverables to, saved by name with their credentials read
	// (destination.Resolve). The delivery step runs for each once the run has
	// succeeded and its containers are gone; a failed delivery is recorded and
	// never changes the run. Nothing but the person's own choice puts a
	// destination here: no manifest, plan or QuickAgent names one. Empty runs
	// no delivery step.
	Destinations []destination.Resolved
	// ReportTheme is the report design export_pdf lays the run's PDFs out
	// with, and what chose it: a design document, or a built-in theme's id;
	// neither is the default, Plain. The type keeps its v1 name so that a
	// caller passing a built-in's id (the desktop) still builds. Used only
	// when the plan grants export_pdf.
	ReportTheme ReportTheme
	// Decoded, when set, is the decode step the caller ran on the inputs
	// before this run (pkg/runner/decode); the receipt names it.
	Decoded *decode.Result
	// Enforced are receipt lines the caller adds to CapabilitiesEnforced,
	// keyed as the receipt's: a knowledge build's sources, fetched before
	// this run by the step a run's own sources take.
	Enforced map[string]string
	// TextInputs says the caller staged only files it made itself (every
	// built-in: localrun.Run sets it): the engine does not decode them.
	// Otherwise a run's inputs are decoded before the agent starts.
	TextInputs bool
	// InputReview, when set, is AI Review of the inputs' text before the
	// agent starts (review.go): a hosted run's, through the registry with
	// the job's credential. A flagged input refuses the run, and a
	// session's added file alone. Nil reviews nothing, as every local run.
	InputReview webscan.Scanner
	// Hosted says the run is a sandbox host's (its driver marks it: a run
	// grant, a job's credential, AGENTRY_HOSTED). A hosted run's browser
	// opens public sites only. A run with InputReview set is hosted whatever
	// this says.
	Hosted bool
	// HostedRender lets a sandbox host's run render pages for a rendered
	// web_read: its driver dispatched it as a browser job, whose slots count
	// the renderer beside the run. A hosted run without it renders nothing.
	HostedRender bool
	// EndBlocked grants end_blocked and the calls ledger (blocked.go): the
	// run ends blocked, not ok, when its agent says it could not do the task
	// or the ledger shows the runner's tools failed it. A release's one-shot
	// run sets it; a built-in, which has outcomes of its own, does not, and
	// a session refuses it.
	EndBlocked bool
	// Web is the built-in Drafter's web configuration and keys (web.go):
	// required for a plan with the web tools, refused for any other.
	Web *WebRun
	// Review is a review-grant run's key (review.go): required for a plan
	// with the review grant, refused for any other.
	Review *ReviewRun
	// Intake holds the answers to the manifest's up-front questions: staged
	// as /inputs/intake.json, file answers staged like inputs, secret
	// answers already merged into SecretOverrides by the caller.
	Intake *intake.Answers
	// Adapter interprets the harness event stream.
	Adapter adapter.Adapter
	// Policy supplies the secret source.
	Policy *policy.Policy
	// PolicyFile names the operator policy file Policy was read from and
	// how its path was chosen (policy.Resolve); the envelope records it
	// with the plan's policySha256 as its digest. Nil leaves the
	// envelope's policy block out.
	PolicyFile *policy.File
	// SecretOverrides resolve declared secrets ahead of the policy source
	// — the QuickAgent's MCP profile rides in here; the engine itself
	// stays profile-agnostic. A persistent agent's stored secrets that are
	// on ride here too, and are the values of the Coding agent's programs'
	// secrets (spec.CoderGrant.Secrets).
	SecretOverrides map[string]string
	// WithheldSecrets are a persistent agent's stored secrets its roster
	// has off: a declared secret of one of these names is written empty,
	// and the policy source is not asked for it.
	WithheldSecrets []string
	// AccountTokens are the tokens of the Coding agent's git host accounts
	// its plan names (spec.CoderGrant.Accounts), by each one's TokenEnv:
	// the tools container's alone, never a server's.
	AccountTokens map[string]string
	// Connections are the access tokens of the servers that sign in by
	// OAuth, keyed by spec.MCPServerSpec.ConnectionKey (a remote server's
	// address, a package server's "pypi:name"): the caller refreshed each
	// from the person's connection (pkg/runner/connection). An access token
	// is all a run is given of one; a server with none here refuses the
	// run.
	Connections map[string]ConnectionToken
	// RenewConnection, when set, is asked for a new access token for a
	// server (by its ConnectionKey) before the one the run holds expires:
	// the engine rewrites the server's connection folder with it, which a
	// bridge and a server that re-reads its token file pick up. Nil leaves
	// each token as given for the whole run.
	RenewConnection func(ctx context.Context, server string) (ConnectionToken, error)
	// ModelSignIn is what the run is given of the person's model sign-in
	// when its plan spends one (spec.HarnessSpec.ModelSignIn, signin.go):
	// an access token alone, or the harness's own config directory. The
	// run refuses one its plan was not compiled for, and a plan that
	// spends one refuses to run without it. Never set by a hosted run's
	// driver.
	ModelSignIn *ModelSignInGrant
	// RenewModelSignIn, when set, is asked for a new access token for a
	// token-file sign-in before the one the run holds expires; the engine
	// rewrites the token file with it. Nil leaves the token as given.
	RenewModelSignIn func(ctx context.Context) (ConnectionToken, error)
	// ServersDir is the signature's installed MCP-server store,
	// substituted for the PHServers placeholder. Required when the plan
	// declares MCP servers; unused otherwise.
	ServersDir string
	// Budgets are the effective hard stops (plan defaults merged with
	// any tighter run flags by the caller).
	Budgets spec.Budgets
	// Attestation is the verify-gate outcome for the envelope.
	Attestation receipt.Attestation
	// ExecPackages are the catalogued packages' resolved versions the exec
	// image build recorded ("name-version"); appended to the receipt's
	// exec line so it names what was installed.
	ExecPackages []string
	// KeepRunDir preserves runs/<id>/ for debugging (secrets.env is
	// still always deleted).
	KeepRunDir bool
	// RunID names the run; "" mints one. The CLI passes the id it used
	// for the default output folder so the two always agree.
	RunID string
	// Stderr receives progress and diagnostics.
	Stderr io.Writer
	// OnStage, when set, is called as a session's start reaches each of
	// its steps (Stage), so a client can say what it is doing. Nil does
	// nothing; a run (Run) reports none.
	OnStage func(Stage)
	// OnUsage, when set, is called every time the harness reports usage,
	// with the run's running totals. It is what lets a driver meter a run
	// while it is live: the envelope's usage arrives only at the end, so
	// without this nothing could enforce a token cap during the run.
	// Called from the stream-consuming goroutine, in order, and never
	// after the harness stream closes.
	//
	// It MUST NOT BLOCK. That goroutine is also what enforces --max-turns
	// and --max-cost, and its end is what releases teardown, so a
	// callback that waits stops the budgets and leaves containers up. A
	// callback that panics is caught and switched off rather than allowed
	// to take the teardown down with it.
	OnUsage func(receipt.Usage)
	// place is where the run's docker filter runs on its engine, read when
	// the run starts (placementFor); nil until then.
	place *filterPlacement

	// Events, when set, receives the harness event stream as JSON lines
	// while the run is live — the same lines the receipt's events.ndjson
	// gets, redacted the same way. A driver tails this instead of parsing
	// the narration (--events). It must not block, for the same reason
	// OnUsage must not: it is written from the same goroutine, on the same
	// terms as the receipt's own copy.
	Events io.Writer
	// OnEvent, when set, receives each normalized harness event of an
	// autonomous run while it is live, in order: the tool calls, each
	// tool's result, the assistant's text, the usage and the harness's
	// errors. An interactive session streams its own events and does not
	// call it. A tool result whose harness did not repeat the tool's name
	// carries the name of the call it answers, and its text is cut to
	// adapter.ToolExcerptMax. The assistant's streamed chunks arrive
	// joined, one event per message, when the message ends. Every text is
	// masked with the run's secret values before it is cut, so no part of
	// one reaches the driver; Raw is left out (Events has the lines). It
	// is called on the terms of OnUsage: from the stream-consuming
	// goroutine, never after the harness stream closes, and it MUST NOT
	// BLOCK; one that panics is switched off and the run goes on.
	OnEvent func(adapter.Event)
	// Persistent makes a session one segment of a persistent agent: the
	// plan must be persistent, and the session store is the agent's own
	// volume, which outlives the segment. Nil for every other run.
	Persistent *PersistentSegment
}

// PersistentSegment is what a persistent agent's supervisor tells the
// engine about the segment it starts.
type PersistentSegment struct {
	// Agent is the agent's name (its built-in's name).
	Agent string
	// Segment numbers this start among the agent's segments, from 1.
	Segment int
	// SessionVolume and DataVolume are the agent's two volumes. The session
	// volume is mounted where the harness keeps its conversation; neither
	// carries the run's label, so neither the segment's teardown nor a
	// sweep of a dead run removes it.
	SessionVolume, DataVolume string
	// Home identifies the runner home the agent belongs to: the volumes
	// carry it in a label, so what lists or removes them acts on this
	// home's alone.
	Home string
	// LegacySessionVolume and LegacyDataVolume, when set, are the volumes
	// named by the agent alone (legacy names), which every runner home on
	// the engine could use. Set only for an agent this home has already run:
	// when one of the agent's volumes does not exist yet and its legacy
	// volume does, the volume is made as a copy of it, so the conversation
	// and the files carry over and the legacy volume is left as it is.
	LegacySessionVolume, LegacyDataVolume string
	// ContinueID is the harness's id for the conversation the agent's last
	// segment was in, to continue; empty starts a new one.
	ContinueID string
	// Share is the person's share folder, absolute, which the segment's
	// tools server mounts read-write and live when its plan mounts a share
	// (spec.AgentFiles.Share); empty when the agent has its share off.
	Share string
	// Roster is the agent's roster for the segment, for its envelope.
	Roster []receipt.RosterEntry
	// AppLink is the socket the agent's supervisor answers its app link on,
	// absolute, which the app link's container binds when the plan has one
	// (spec.CapSummary.AppLink); empty otherwise.
	AppLink string
	// ShareVerdicts is a file of the agent's own the share's verdicts are
	// kept in between segments, so a file checked in one is not checked
	// again in the next while it is unchanged; empty keeps none.
	ShareVerdicts string
	// ShareText is a folder of the agent's own in which the text read out
	// of the share's documents is kept between segments, one file for each
	// checked digest, beside ShareVerdicts. Empty keeps none, and a
	// document whose text was not kept is checked again.
	ShareText string
	// ShareMirror is a folder of the agent's own the share's mirror is kept
	// in, for a plan whose servers see it (spec.ServerShareMirror): before
	// each turn it is made to hold a copy of each share file whose bytes the
	// share check passed, and nothing else. The engine keeps its index of
	// what it copied in ShareMirror + ".json" and stages each copy in
	// ShareMirror + ".staging", beside it. Empty for any other plan.
	ShareMirror string
	// Skills is the folder of the agent's skills for this segment, which
	// its supervisor filled with the skills the roster has on, for a plan
	// that has any (spec.AgentFiles.Skills); mounted read-only into the
	// tools server alone. Empty for any other plan.
	Skills string
	// DependencyDB is the folder of OSV's databases the Coding agent's
	// dependency scan reads, kept under the runner home, for a plan that
	// has the scan (spec.CoderGrant.DependencyScan); mounted read-only into
	// the tools server alone. Empty for any other plan.
	DependencyDB string
	// Memory is the agent's memory, for an agent that keeps one
	// (spec.AgentFiles.Memory); nil for any other agent.
	Memory *PersistentMemory
}

// PersistentMemory is a persistent agent's memory volume: made, labelled
// and kept like its other two, mounted read-write into the tools server
// alone, and seeded once.
type PersistentMemory struct {
	// Volume is the volume's name.
	Volume string
	// Seed is what the volume is filled with when it is seeded, by path
	// beneath it: a file, its folders made for it, or a folder of its own
	// where the path ends in "/". A file beneath MemoryCarriedDir is
	// appended to the file of the same path beneath the memory, and only
	// when Carry was copied in; that folder itself is not kept.
	Seed map[string][]byte
	// Carry, when set, is a folder beneath the data volume that the seed
	// copies to CarryTo beneath the memory when it holds anything: files
	// and folders, never a link. The data volume's folder is left as it is.
	Carry, CarryTo string
	// Seeded says an earlier start seeded the volume. Unset, a volume
	// that exists is seeded too, unless it already holds the seed's
	// SCHEMA.md: a start stopped between making the volume and seeding it
	// leaves it empty.
	Seeded bool
}

// MemoryCarriedDir is the folder of PersistentMemory.Seed whose files are
// appended to the memory's when PersistentMemory.Carry was copied in.
const MemoryCarriedDir = ".carried"

// engine is the run's container engine once placed (placementFor), and
// Docker Engine on this machine until then.
func (p Params) engine() dockerx.Engine {
	if p.place != nil {
		return p.place.engine
	}
	return dockerx.LocalDockerEngine()
}

// Run executes the plan. The returned envelope is always usable — on
// failure it carries status "error", the exit class, and whatever was
// known — and the returned error (nil on success) is already classified.
func Run(ctx context.Context, d dockerx.Client, p Params) (outEnv *receipt.Envelope, outErr error) {
	start := time.Now()
	runID := p.RunID
	if runID == "" {
		var err error
		if runID, err = NewRunID(); err != nil {
			return nil, Classify(ExitCLI, err)
		}
	}
	env := &receipt.Envelope{
		Agentry:              receipt.EnvelopeVersion,
		Status:               "error", // flipped to ok at the end
		RunID:                runID,
		Ref:                  p.Plan.Ref,
		Digest:               p.Plan.Digest,
		Attestation:          p.Attestation,
		CapabilitiesEnforced: enforcedAtStart(p),
		Harness: receipt.Harness{
			Adapter:        p.Plan.Harness.Adapter,
			Image:          p.Plan.Harness.Agent.Image,
			Model:          p.Plan.Harness.Model,
			ImagePin:       p.Plan.Harness.ImagePin.String(),
			CredentialKind: credentialKind(p.Plan),
		},
		Policy: policyRecord(p),
		Result: receipt.TextResult(""),
		OutDir: p.OutDir,
		Stage:  receipt.StagePrepare,
	}
	// The run's redactor exists once the token and the secrets are known
	// (rd.redact, from then on); every failure before that has nothing to
	// mask, so the local stands in as nil for them.
	var redact *redactor
	planHash, err := p.Plan.Hash()
	if err != nil {
		return fail(env, redact, Classify(ExitCLI, err))
	}
	receiptsDir := filepath.Join(p.OutDir, "receipts")
	env.Receipts = receipt.Receipts{Dir: receiptsDir, RunPlanSHA256: planHash}
	// A plan with the web tools runs only with its scanner, before
	// anything touches the daemon.
	if err := checkWeb(p); err != nil {
		return fail(env, redact, err)
	}
	if err := checkReview(p); err != nil {
		return fail(env, redact, err)
	}
	if err := checkVoice(p); err != nil {
		return fail(env, redact, err)
	}
	if err := checkModelSignIn(p); err != nil {
		return fail(env, redact, err)
	}
	// A saved destination is the person's on this computer: a hosted run
	// delivers only to the destinations its host handed it in a destinations
	// file, and never to one on a private network, whatever its caller put in
	// the parameters.
	if p.Hosted || p.InputReview != nil {
		for _, d := range p.Destinations {
			if !d.Hosted || d.OnMyNetwork {
				return fail(env, redact, Classify(ExitUsage, errors.New("a hosted run delivers only to the destinations its host hands it, at public addresses")))
			}
		}
	}
	// A target host is reached only as the plan declares it, and never
	// by a hosted run: refused before anything starts.
	if err := checkTargetHosts(p); err != nil {
		return fail(env, redact, err)
	}
	// A browser is given only as the plan declares it, and a hosted run's
	// only to public sites.
	if err := checkBrowser(ctx, p); err != nil {
		return fail(env, redact, err)
	}
	// The engine, and where the run's docker filter runs on it, before
	// anything touches the daemon: an engine the filter cannot be put in
	// front of is refused here.
	place, err := placementFor(ctx, d)
	if err != nil {
		return fail(env, redact, err)
	}
	p.place = &place
	env.CapabilitiesEnforced["engine"] = place.line()
	if p.Plan.Runtime() != "" {
		// The runtime line follows the engine, known only now.
		env.CapabilitiesEnforced["runtime"] = runtimeLine(p)
	}
	env.CapabilitiesEnforced = withSignInRenewal(env.CapabilitiesEnforced, p)

	// Everything below must tear down whatever it built, success or not:
	// td runs in reverse order via defer, then sweeps anything still
	// carrying this run's label. What a run whose process died left
	// behind goes first, before this run adds its own.
	sweepAbandonedRun(ctx, d, p.RunRoot, p.RunnerHome, p.Stderr)
	td := &teardown{d: d, stderr: p.Stderr, runID: runID}
	// finish lists and delivers the run's deliverables once teardown has
	// stopped every container the run started and found nothing it could not
	// delete: a run that teardown fails lists and delivers none.
	var finish func()
	defer func() {
		td.run()
		// What teardown could not delete turns the run's outcome.
		if err := td.reportLeftFiles(env); err != nil && outErr == nil {
			outErr = err
		}
		if finish != nil && outErr == nil && env.Status == "ok" {
			finish()
		}
	}()

	rd, err := prepareRunDirs(p, runID, td)
	if err != nil {
		return fail(env, redact, err)
	}
	// The input sources are fetched first, then everything is decoded
	// before anything starts.
	srcRuns, err := fetchSources(ctx, d, p, rd, td, runID)
	if err != nil {
		return fail(env, redact, err)
	}
	rd.sources = sourceIDs(srcRuns)
	dec, err := decodeInputs(ctx, d, p, rd)
	if err != nil {
		return fail(env, redact, err)
	}
	dec.lines(env.CapabilitiesEnforced)
	sourceLines(env.CapabilitiesEnforced, srcRuns, dec)
	secrets, err := secretValues(p)
	if err != nil {
		return fail(env, redact, err)
	}
	if err := writeSecretsFile(p, rd.secretsPath); err != nil {
		return fail(env, redact, err)
	}
	if err := writeConnectionFiles(p, rd, secrets); err != nil {
		return fail(env, redact, err)
	}
	if err := prepareTargetHosts(ctx, d, p, rd, td, env); err != nil {
		return fail(env, redact, err)
	}
	if err := prepareBrowser(ctx, d, p, rd, td, env, secrets); err != nil {
		return fail(env, redact, err)
	}
	if err := prepareRender(ctx, d, p, rd, td, runID, env); err != nil {
		return fail(env, redact, err)
	}
	// The optional secrets it goes without, before any replacer is built:
	// the System Prompt names them (spec.PHSecretsAbsent), and so does the
	// receipt.
	rd.secretsAbsent = absentSecrets(p.Plan, secrets)
	env.CapabilitiesEnforced = withAbsentNote(env.CapabilitiesEnforced, rd.secretsAbsent)
	env.CapabilitiesEnforced = withWithheldNote(env.CapabilitiesEnforced, withheldSecrets(p))
	task, err := readTask(p.Task, rd.receiptsDir)
	if err != nil {
		return fail(env, redact, err)
	}
	task, taskSent, err := ownInstructionWhenEmpty(task, p.Plan, rd.workspace)
	if err != nil {
		return fail(env, redact, err)
	}

	token, err := newToken()
	if err != nil {
		return fail(env, redact, Classify(ExitCLI, err))
	}
	rep := rd.replacer()
	redact = rd.newRedactor(p, token, secrets)
	env.CapabilitiesEnforced = withRedactionNote(env.CapabilitiesEnforced, p, secrets)
	// Connections are renewed before they expire for as long as the run
	// lives; teardown stops the renewer before it deletes the connection
	// folders.
	stopRenewal := renewConnections(ctx, p, rd)
	td.add(func(context.Context) { stopRenewal() })

	env.Stage = receipt.StageInfra
	// A run driven as one session turn may approve the gateway's tools: its
	// grant, read before any harness starts (a host harness runs as the
	// person and could rewrite the file), and what the gateway serves beyond
	// it (the declared servers' tools), as a session's.
	var oneTurnTools []string
	var served []string
	var servedInto *[]string
	if p.Plan.Harness.OneTurn != nil {
		if oneTurnTools, err = grantedTools(filepath.Join(rd.cfg, "gateway")); err != nil {
			return fail(env, redact, Classify(ExitCLI, fmt.Errorf("the run's tool grant: %w", err)))
		}
		servedInto = &served
	}
	err = startInfra(ctx, d, p, rd, td, runID, token, rep, servedInto)
	if len(rd.modelHosts) > 0 {
		env.CapabilitiesEnforced["llm-proxy.hosts"] = modelHostsLine(rd.modelHosts)
	}
	if err != nil {
		env.CapabilitiesEnforced["daemon"] = daemonLine(rd)
		return fail(env, redact, withRefusals(err, rd))
	}
	// Harness files are rendered once the infrastructure is up: a host-run
	// harness needs the published gateway and proxy addresses, known only
	// now; the replacer learns them too.
	rep = rd.replacer()
	if err := renderPlanFiles(p.Plan, p.PlanDir, rd, nativePath, "harness"); err != nil {
		return fail(env, redact, err)
	}

	oneTurnTools = permitted(oneTurnTools, served)

	env.Stage = receipt.StageAgent
	outcome := retryAtCapacity(ctx, p, rd, func() agentOutcome {
		if p.Plan.Harness.Host != nil {
			return runHostHarness(ctx, p, rd, token, rep, task, oneTurnTools)
		}
		return runAgent(ctx, d, p, rd, runID, token, rep, task, oneTurnTools)
	})
	if outcome.hostInfo != "" {
		env.Harness.Host = outcome.hostInfo
	}
	if len(outcome.hostEnv) > 0 {
		env.CapabilitiesEnforced["harness.env"] = hostEnvLine(outcome.hostEnv)
	}
	// The harness's final text is the result; it is masked like every
	// other receipt (the session does the same with its last message).
	outcome.lastText = redact.String(outcome.lastText)
	env.Usage = outcome.usage
	env.Usage.WallMS = time.Since(start).Milliseconds()

	// The gateway's own log (tool-call log included, --log-calls) is
	// part of the audit trail; collect before teardown kills it.
	// Collected under a context that survives an interrupt: the receipts
	// are the point of tearing down cleanly.
	keep := context.WithoutCancel(ctx)
	if logs, lerr := d.Logs(keep, containerName(runID, "gateway"), 2000); lerr == nil && logs != "" {
		_ = os.WriteFile(filepath.Join(rd.receiptsDir, "gateway.log"), []byte(redact.String(logs)), 0o644)
	}
	// The proxy's decisions (allow/deny per CONNECT) are the evidence of
	// what left the run; collected the same way.
	if logs, lerr := d.Logs(keep, containerName(runID, "proxy"), 2000); lerr == nil && logs != "" {
		_ = os.WriteFile(filepath.Join(rd.receiptsDir, "proxy.log"), []byte(redact.String(logs)), 0o644)
	}
	// The calls ledger holds the agent's own end_blocked sentence: masked
	// like every receipt the run could have written into.
	if rd.callsLedger != "" {
		rd.redact.File(rd.callsLedger)
	}
	// The PDF ledger holds names the agent chose: masked the same way, so
	// a PDF named with a secret's value is not one the receipt can name.
	if rd.pdfLedger != "" {
		rd.redact.File(rd.pdfLedger)
	}
	// The fetch ledger holds paths the agent chose: masked the same way.
	if rd.fetchLedger != "" {
		rd.redact.File(rd.fetchLedger)
	}
	// The deliverables ledger holds paths and titles the agent chose.
	if rd.deliverablesLedger != "" {
		rd.redact.File(rd.deliverablesLedger)
	}
	// Each target host's calls, the agent having ended.
	copyHostLogs(rd, env)
	// The browse proxy's decisions and what the browser saved.
	collectBrowseProxyLog(keep, d, rd, env)
	copyBrowserOut(rd, p.Stderr)
	// The page renderer's count and its proxy's decisions, the agent having
	// ended.
	if rd.render != nil {
		rd.render.close(keep, d, rd, env)
	}
	writeMeta(rd.receiptsDir, runID, p, start, taskSent)
	env.CapabilitiesEnforced["daemon"] = daemonLine(rd)
	// The agent has ended, so the web ledger is complete.
	if env.Web = webRecord(rd.receiptsDir, p.Web); env.Web != nil {
		env.CapabilitiesEnforced["web.aiReview"] = webLine(env.Web)
	}
	if rs := p.Plan.WebResolvers(); len(rs) > 0 {
		env.CapabilitiesEnforced["web.resolvers"] = resolversLine(p.Plan, rs)
	}
	if env.Review = reviewRecord(rd.receiptsDir, p.Review); env.Review != nil {
		env.CapabilitiesEnforced["review.aiReview"] = reviewLine(env.Review)
	}
	env.PDFs = pdfRecord(rd)
	env.FetchSaves = fetchSavesRecord(rd)

	if outcome.err != nil {
		env.Result = receipt.TextResult(outcome.lastText)
		if ctx.Err() != nil {
			// The person (or the client driving the CLI) interrupted the
			// run: not the harness's failure, and not a budget — its own
			// word, under the cli class, with the receipts kept.
			return fail(env, redact, Classify(ExitCLI, errors.New("interrupted")))
		}
		return fail(env, redact, outcome.err)
	}
	if p.EndBlocked {
		if berr := blockedOf(rd.callsLedger); berr != nil {
			env.Result = receipt.TextResult(outcome.lastText)
			return fail(env, redact, berr)
		}
	}
	// A container the gateway created mid-run under another runtime than
	// the policy's: refused it, and the run with it.
	if merr := rd.runtimeMismatch(); merr != nil {
		env.Result = receipt.TextResult(outcome.lastText)
		return fail(env, redact, Classify(ExitGateway, merr))
	}

	env.Stage = receipt.StageOutput
	schema, serr := outputSchema(p)
	if serr != nil {
		env.Result = receipt.TextResult(outcome.lastText)
		return fail(env, redact, serr)
	}
	result, rerr := finalResult(p.Plan.Harness.ResultKind, outcome.lastText, schema)
	if rerr != nil {
		env.Result = receipt.TextResult(outcome.lastText)
		if outcome.answerInReasoning {
			rerr = errAnswerInReasoning()
		}
		return fail(env, redact, rerr)
	}
	env.Result = result
	env.Status = "ok"
	env.ExitCode = ExitOK
	env.Stage = ""
	finish = func() { finishDeliverables(ctx, d, env, rd, p, plainResultText(result)) }
	if err := env.Write(p.OutDir); err != nil {
		_, _ = fmt.Fprintf(p.Stderr, "warning: %v\n", err)
	}
	return env, nil
}

// refusedInputs is what an envelope names of the inputs err's refusal is
// for. An entry whose path carries a value the run masks is left out and
// counted, since a path is shown as it is or not at all.
func refusedInputs(r *redactor, err error) ([]receipt.RefusedInput, int) {
	list, more := receipt.RefusedInputsOf(err)
	out := list[:0:0]
	for _, in := range list {
		if r.String(in.Path) != in.Path {
			more++
			continue
		}
		out = append(out, in)
	}
	if len(out) == 0 {
		out = nil
	}
	return out, more
}

// fail stamps the envelope with the classified failure — its text masked
// by the run's redactor, since a gateway log tail or an agent's stderr
// tail rides along in it — writes it to the output folder (a failed run
// still leaves its record), and returns.
func fail(env *receipt.Envelope, r *redactor, err error) (*receipt.Envelope, error) {
	// The reason is read before the redactor, which rebuilds a masked
	// error without the chain; it holds no value to mask (receipt.Reason).
	env.Reason = receipt.ReasonOf(err)
	env.RefusedInputs, env.RefusedInputsMore = refusedInputs(r, err)
	err = r.Err(err)
	var ce *ClassifiedError
	if !errors.As(err, &ce) {
		ce = Classify(ExitCLI, err)
	}
	env.Status = "error"
	env.ExitCode = ce.Code
	env.Error = ce.Err.Error()
	if env.OutDir != "" {
		if werr := env.Write(env.OutDir); werr != nil {
			env.Error += " (and result.json could not be written: " + werr.Error() + ")"
		}
	}
	return env, ce
}

// agentOutcome is what the agent-container phase produced.
type agentOutcome struct {
	lastText string
	// answerInReasoning: the collector's answerInReasoning at the end.
	answerInReasoning bool
	usage             receipt.Usage
	err               error // classified, nil on success
	// hostInfo names the executable (and version) a host-run harness
	// ran, for the envelope; hostEnv the host variables it could inherit.
	hostInfo string
	hostEnv  []string
	// atCapacity: the harness exited failing on a capacity answer
	// (capacity.go).
	atCapacity bool
}

// startInfra creates the run network and starts proxy + gateway, then
// health-probes the gateway from inside the network (execute order). Every
// failure here is the gateway/environment class. When served is non-nil (a
// session's), the probe also lists the tools the gateway serves and
// startInfra stores them there.
func startInfra(ctx context.Context, d dockerx.Client, p Params, rd *runDirs,
	td *teardown, runID, token string, rep *strings.Replacer, served *[]string,
) error {
	labels := runLabels(runID)
	netName := networkName(runID)
	if err := d.NetworkCreate(ctx, netName, true, labels); err != nil {
		return Classify(ExitGateway, fmt.Errorf("create run network: %w", err))
	}
	td.add(func(c context.Context) { _ = d.NetworkRemove(c, netName) })
	td.networks = append(td.networks, netName)

	if err := startProxy(ctx, d, p, rd, td, runID, netName, labels); err != nil {
		return err
	}

	gwName := containerName(runID, "gateway")
	// The gateway's env names host paths (its bind allow-lists) in the
	// gateway's spelling; its mounts are made by this CLI, in the host's,
	// and its argv names only paths inside it.
	gwEnv, err := rd.gatewayEnv(p.Plan, token)
	if err != nil {
		return err
	}
	gwSpec := dockerx.RunSpec{
		Name:   gwName,
		Image:  p.Plan.Gateway.Image,
		Argv:   append(substituteAll(p.Plan.Gateway.Args, rep), gatewayLimitArgs(p.limits())...),
		Env:    gwEnv,
		Mounts: resolveMounts(p.Plan.Gateway.Mounts, rep),
		// The pinned gateway image declares VOLUME /misc. Left to docker it
		// is an unlabelled anonymous volume that outlived every run;
		// mounted here it carries the run's labels, goes with the container
		// (RemoveForce's -v), and is found by teardown's and the sweeps'
		// run label if the container went some other way.
		Volumes: []dockerx.VolumeMount{{Target: gatewayVolumeDir, Labels: labels}},
		Network: netName,
		Alias:   spec.GatewayAlias,
		// The gateway learns "its" networks by inspecting the container
		// whose name is its hostname (docker/mcp-gateway guessNetworks)
		// and attaches every MCP server it launches to all of them. A
		// server needs none of ours: it speaks stdio to the gateway, and
		// its declared hosts are reached through the gateway's own relay
		// network, where the relay's name must be the server's first
		// network for docker to resolve the link. So the gateway's
		// hostname is not its container name: it finds no container,
		// propagates nothing, and each server sits on exactly the
		// networks its contract calls for.
		Hostname: gatewayHostname(runID),
		Labels:   labels,
		// The gateway drives the daemon, through the run's filter where it
		// runs; that is its documented privilege level, and it is why the
		// agent container never sees this socket. An SELinux-enabled daemon
		// would otherwise refuse it the socket outright. The exemption goes
		// with the role, whatever socket the mount names.
		DisableSELinuxLabel: true,
	}
	// Its root stays writable: a vendor image, whose writes are its own.
	// It opens the socket as its owner, root, with no capability; reading
	// the run's secrets file, 0600 and the invoking user's, takes
	// DAC_READ_SEARCH, which it keeps and nothing else.
	gwSpec = hardened(gwSpec, p.limits())
	gwSpec.CapAdd = []string{"DAC_READ_SEARCH"}
	// The gateway's own runtime, a registration that opens the bind-mounted
	// socket of the run's docker filter (docker.gatewayRuntime); "" is the
	// daemon's default.
	if gatewaySandboxed(p.Plan) {
		gwSpec.Runtime = p.Plan.GatewayRuntime()
	}
	// A run that will give the gateway relays to start: where the docker
	// filter runs, the relay networks are its own; elsewhere it refuses to
	// share the host's one relay network with another gateway's (relays.go)
	// and holds the runner home's relay lock from the listing to the end of
	// its teardown, so two such runs starting together cannot both list an
	// empty network (relaylock.go). Either way the release or removal is
	// queued before the gateway's teardown, so it runs after the relays are
	// reaped.
	switch {
	case !relaysNeeded(p.Plan):
	case filterSupported:
		// The gateway's two fixed relay networks are this run's own, made
		// here and removed with the run; the filter translates the names
		// (dockerfilter.go). Runs with relays share a host without seeing
		// each other's.
		if err := makeRelayNetworks(ctx, d, runID, td); err != nil {
			return err
		}
		// The browser's own network and the browse proxy, on the relay
		// egress network, before the filter that holds the browser to the
		// one and pins the other's address for its relay.
		if err := makeBrowserNetwork(ctx, d, p, runID, td); err != nil {
			return err
		}
		if err := startBrowseProxy(ctx, d, p, rd, td, runID); err != nil {
			return err
		}
	default:
		// Without the filter the gateway's relay network is the host's one
		// fixed network: such runs take turns on the host.
		release, err := holdRelayLock(p.RunnerHome)
		if err != nil {
			return err
		}
		td.add(func(context.Context) { release() })
		if err := refuseForeignRelays(ctx, d); err != nil {
			return err
		}
	}
	// A tools server that reaches past the host (the web tools', or one
	// with unrestricted egress) has a network of the run's own, where the
	// filter runs; elsewhere the gateway gives it the daemon's default and
	// the receipt's daemon line says the filter did not run. The web tools
	// hold their own address rule either way (toolsrv/web.go).
	if filterSupported && p.Plan.Capabilities.ToolsReachPastHost() {
		if err := makeWebNetwork(ctx, d, runID, td); err != nil {
			return err
		}
	}
	// The gateway's daemon socket is this run's filter, not the daemon's
	// (dockerfilter.go). Queued before the gateway's teardown, so it
	// closes after the gateway has stopped and reaped the containers it
	// launched through it.
	place := p.place
	if place == nil && filterSupported {
		pl, err := placementFor(ctx, d)
		if err != nil {
			return err
		}
		place = &pl
	}
	switch {
	case filterSupported && place.inProcess:
		upstream := place.upstream
		if rt := p.Plan.Runtime(); rt != "" && place.engine.Kind == dockerx.KindPodman {
			// podman's API drops a container's runtime: the gateway's
			// containers are created through a service of the run's own
			// whose default runtime is the policy's (podmanservice.go),
			// which podman's own CLI starts. Queued before the filter's
			// close, so it stops after the filter that dials it.
			if place.engine.CLI != dockerx.KindPodman {
				return Classify(ExitGateway, errors.New("the policy's docker.runtime is held on podman only through podman's own CLI (AGENTRY_ENGINE=podman), which can read a container's runtime back; the docker CLI's route to podman cannot"))
			}
			svc, err := startPodmanService(ctx, d, rd, runID, p.RunnerHome, rt)
			if err != nil {
				return err
			}
			td.add(func(context.Context) { svc.stop() })
			upstream = svc.socket
		}
		filter, sock, err := startDaemonFilter(d, rd, runID, gwEnv, p.limits(), p.Plan, p.RunnerHome, place, upstream)
		if err != nil {
			return err
		}
		rd.filter = filter
		td.add(func(context.Context) { closeDaemonFilter(rd) })
		gwSpec.Mounts = throughFilter(gwSpec.Mounts, sock)
	case filterSupported:
		// A daemon in a VM: the same filter, in a container of the run's
		// own (filtersidecar.go). On podman the API drops a container's
		// runtime and only the CLI, serving the filter itself, can start
		// the service that holds it and read each runtime back: a runtime
		// there is refused rather than left unenforced.
		if place.engine.Kind == dockerx.KindPodman && p.Plan.Runtime() != "" {
			return Classify(ExitGateway, fmt.Errorf("the policy's docker.runtime is held on podman only where the CLI serves the run's docker filter (%s): unset %s, or run without docker.runtime", place.reason, EnvDockerFilter))
		}
		cfg, err := filterConfig(rd, runID, gwEnv, p.limits(), p.Plan, place)
		if err != nil {
			return err
		}
		vol, err := startFilterSidecar(ctx, d, rd, runID, p.Plan.Proxy.Image, place.mountSource, cfg, p.limits(), td)
		if err != nil {
			return err
		}
		gwSpec.Mounts = throughFilterVolume(gwSpec.Mounts, vol)
	}
	// Where the daemon runs in a VM, the app link's relay serves its socket
	// before the gateway starts the app link.
	if err := startAppLinkRelay(ctx, d, p, rd, td, runID); err != nil {
		return err
	}
	if err := dockerx.RunDetachedUnder(ctx, d, gwSpec, "gateway"); err != nil {
		return Classify(ExitGateway, fmt.Errorf("start mcp gateway: %w", err))
	}
	// Graceful stop first: the gateway reaps its tool-server containers on
	// SIGTERM; rm -f would orphan them. Its relay sidecars it does not reap
	// on our stop (Docker 29.7.2), so they are removed here by the names its
	// log gave them (relays.go), before the log goes with the container.
	relays := &relayReaper{}
	if filterSupported {
		relays.runID = runID
	}
	td.add(func(c context.Context) {
		_ = d.Stop(c, gwName, 10)
		relays.reap(c, d, gwName)
		_ = d.RemoveForce(c, gwName)
	})
	// No bridge leg for the gateway, on purpose: see the proxy above.

	// Health: an authenticated MCP initialize from inside the run
	// network. A gateway that cannot answer this cannot serve the agent;
	// abort rather than degrade to tool-less execution.
	probeName := containerName(runID, "probe")
	td.add(func(c context.Context) { _ = d.RemoveForce(c, probeName) })
	probeSpec := dockerx.RunSpec{
		Name:  probeName,
		Image: p.Plan.Proxy.Image,
		Argv: []string{"probe",
			"--url", spec.GatewayURL(),
			"--token-env", spec.GatewayTokenEnv,
			"--timeout", "90"},
		Env:        map[string]string{spec.GatewayTokenEnv: token},
		Network:    netName,
		Labels:     labels,
		AutoRemove: true,
	}
	// Under the policy's runtime like the agent and, for the same reason,
	// given the gateway's address rather than its name.
	if probeSpec.ExtraHosts, err = sandboxHosts(ctx, d, p.Plan, runID); err != nil {
		return Classify(ExitGateway, err)
	}
	probeSpec.Runtime = p.Plan.Runtime()
	probeSpec = readOnlyRoot(hardened(probeSpec, p.limits()), "/tmp")
	listing := io.Discard
	var listed probeOutput
	if served != nil {
		probeSpec.Argv = append(probeSpec.Argv, "--list-tools")
		listing = &listed
	}
	code, perr := dockerx.RunForegroundUnder(ctx, d, probeSpec, "gateway's health probe", nil, listing, p.Stderr)
	if perr != nil || code != 0 {
		diag := gatewayDiagnostics(ctx, d, gwName)
		if perr == nil {
			perr = fmt.Errorf("gateway health probe failed (exit %d)", code)
		}
		return Classify(ExitGateway, fmt.Errorf("%w%s", perr, diag))
	}
	// A container the gateway created under another runtime than the
	// policy's was refused it by the filter, and the gateway carried on
	// without it: the run does not.
	if err := rd.runtimeMismatch(); err != nil {
		return Classify(ExitGateway, err)
	}
	if served != nil {
		names, err := parseServedTools(listed.Bytes())
		if err != nil {
			return Classify(ExitGateway, fmt.Errorf("list the tools the gateway serves: %w", err))
		}
		*served = names
	}
	// A declared server the gateway could not start is logged and then
	// carried past ("Can't start <name>"); the gateway serves whatever
	// came up. That is not the run the contract described, so it is the
	// same failure class as a gateway that never answered. The gateway
	// launches its servers while loading its configuration, before it
	// serves the probe, so the log is complete by now. The tools server
	// is checked the same way, on every run: a gateway that could not
	// start it (a relay the docker filter refused, say) carries on with
	// the declared servers alone, and the agent would run without the
	// tools its contract grants.
	logs, err := d.Logs(ctx, gwName, dockerx.LogsAll)
	if err != nil {
		return Classify(ExitGateway, fmt.Errorf("read gateway log to confirm the tools server and declared servers started: %w", err))
	}
	// The relays are up by now and nothing of the agent's has run: the
	// names the log carries are the gateway's own (relays.go).
	relays.snapshot(logs)
	if err := checkToolsServerStarted(logs); err != nil {
		return Classify(ExitGateway, withRefusals(err, rd))
	}
	if err := checkDeclaredServersStarted(logs, p.Plan.MCPServers); err != nil {
		return Classify(ExitGateway, withRefusals(err, rd))
	}
	// The host link checks each target host's sign-in before it serves:
	// a refusal stops the run here, with its reason.
	if p.Plan.HostLink != nil {
		if err := checkHostLinkStarted(logs); err != nil {
			return withRefusals(err, rd)
		}
	}
	return nil
}

// startProxy starts the run's llm egress proxy on netName, with its leg
// out: a run's, and a Copilot model list's (copilotmodels.go).
func startProxy(ctx context.Context, d dockerx.Client, p Params, rd *runDirs, td *teardown, runID, netName string, labels map[string]string) error {
	// LLM egress proxy: the agent container's only route out. It is also
	// the one run container with a bridge leg, so for a host-run harness it
	// is the host's door to everything: its own CONNECT port and a plain
	// TCP forward to the gateway, which never leaves the run network (a
	// gateway on the bridge makes every MCP server it launches inherit that
	// mix of networks, which docker refuses).
	proxyName := containerName(runID, "proxy")
	proxyArgv := []string{"proxy",
		"--allow", strings.Join(p.Plan.Proxy.Allow, ","),
		"--addr", fmt.Sprintf(":%d", spec.ProxyPort)}
	if len(p.Plan.Proxy.PlainHTTP) > 0 {
		proxyArgv = append(proxyArgv, "--plain-http", strings.Join(p.Plan.Proxy.PlainHTTP, ","))
	}
	proxyPublish := hostPublish(p, spec.ProxyPort)
	if p.Plan.Harness.Host != nil {
		proxyArgv = append(proxyArgv, "--forward", fmt.Sprintf(":%d=%s:%d", spec.GatewayPort, spec.GatewayAlias, spec.GatewayPort))
		proxyPublish = append(proxyPublish, hostPublish(p, spec.GatewayPort)...)
	}
	proxySpec := dockerx.RunSpec{
		Name:    proxyName,
		Image:   p.Plan.Proxy.Image,
		Argv:    proxyArgv,
		Network: netName,
		Alias:   spec.ProxyAlias,
		Labels:  labels,
		Publish: proxyPublish,
	}
	proxySpec = readOnlyRoot(hardened(proxySpec, p.limits()), "/tmp")
	// The model endpoints at the addresses this machine resolves for them
	// (modelhosts.go).
	rd.modelHosts = modelHostPins(ctx, p.Plan.Proxy.Allow)
	proxySpec.ExtraHosts = append(proxySpec.ExtraHosts, rd.modelHosts...)
	// Under the run's runtime where the host's name servers are named: it
	// reads the run's own resolv.conf, and its egress leg is a network of
	// the run's own joined at create, since a sandboxed runtime takes no
	// network connected after start and docker refuses the default bridge
	// beside the run network at create. A host-run harness's proxy
	// publishes ports to the host and stays on the daemon's default.
	sandboxed := proxySandboxed(p.Plan)
	if sandboxed {
		egress := proxyEgressNetwork(runID)
		if err := d.NetworkCreate(ctx, egress, false, labels); err != nil {
			return Classify(ExitGateway, fmt.Errorf("create the llm proxy's egress network: %w", err))
		}
		td.add(func(c context.Context) { _ = d.NetworkRemove(c, egress) })
		td.networks = append(td.networks, egress)
		proxySpec.ExtraNetworks = []string{egress}
		proxySpec.Runtime = p.Plan.Runtime()
		proxySpec.Mounts = append(proxySpec.Mounts, dockerx.Mount{
			Source: filepath.Join(rd.cfg, "gateway", resolvConfFile), Target: policy.ResolvConfTarget, ReadOnly: true, Relabel: true,
		})
	}
	if err := dockerx.RunDetachedUnder(ctx, d, proxySpec, "llm proxy"); err != nil {
		return Classify(ExitGateway, fmt.Errorf("start llm proxy: %w", err))
	}
	td.add(func(c context.Context) { _ = d.RemoveForce(c, proxyName) })
	// The proxy needs a route to the model endpoint; the run network is
	// internal by design, so it joins the default bridge as its second
	// leg (or, sandboxed, took its own egress network at create). The
	// agent container never does.
	if !sandboxed {
		if err := d.NetworkConnect(ctx, "bridge", proxyName); err != nil {
			return Classify(ExitGateway, fmt.Errorf("connect proxy to bridge: %w", err))
		}
	}
	if p.Plan.Harness.Host != nil {
		port, perr := waitPublishedPort(ctx, d, proxyName, spec.ProxyPort)
		if perr != nil {
			return Classify(ExitGateway, fmt.Errorf("published proxy port: %w", perr))
		}
		rd.hostProxyURL = fmt.Sprintf("http://127.0.0.1:%d", port)
		gwPort, perr := waitPublishedPort(ctx, d, proxyName, spec.GatewayPort)
		if perr != nil {
			return Classify(ExitGateway, fmt.Errorf("published gateway forward: %w", perr))
		}
		rd.hostGatewayURL = fmt.Sprintf("http://127.0.0.1:%d/mcp", gwPort)
	}
	return nil
}

// checkToolsServerStarted reads the gateway's startup log for its line
// saying the run's own tools server did not start or list its tools, and
// names it (the same v0.43.3 line shapes as checkDeclaredServersStarted).
func checkToolsServerStarted(logs string) error {
	for _, line := range strings.Split(logs, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "> Can't start "+spec.ToolServerName+":") || strings.HasPrefix(t, "> Can't list tools "+spec.ToolServerName+":") {
			return fmt.Errorf("the run's tools server did not start, so the agent would have none of its tools; the gateway said: %s", t)
		}
	}
	return nil
}

// checkDeclaredServersStarted reads the gateway's startup log for the
// lines it writes when a declared server fails to start or to list its
// tools, and names the first such server with the gateway's own line.
// The line shapes are docker/mcp-gateway v0.43.3's (pkg/gateway/
// capabilitites.go "  > Can't start %s: %s", "  > Can't list tools %s:
// %s"; pkg/mcp/stdio.go relays server stderr as "- %s: %s"): a gateway
// bump re-checks them.
func checkDeclaredServersStarted(logs string, servers []spec.MCPServerSpec) error {
	lines := strings.Split(logs, "\n")
	for _, srv := range servers {
		for _, line := range lines {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "> Can't start "+srv.Name+":") || strings.HasPrefix(t, "> Can't list tools "+srv.Name+":") {
				return receipt.WithReason(fmt.Errorf("declared MCP server %s did not start; the gateway said: %s%s", srv.Ref(), t, serverOutputTail(lines, srv.Name)),
					receipt.ReasonMCPStartFailed, "", srv.SourceID())
			}
		}
	}
	return nil
}

// serverOutputTail collects what the gateway relayed from a server's own
// stderr ("- <name>: …" lines), last 15, so a start failure explains
// itself: this path ends the run before the receipts keep the gateway log.
func serverOutputTail(lines []string, name string) string {
	var out []string
	for _, line := range lines {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "- "+name+": ") {
			out = append(out, strings.TrimPrefix(t, "- "+name+": "))
		}
	}
	if len(out) == 0 {
		return ""
	}
	if len(out) > 15 {
		out = out[len(out)-15:]
	}
	return "\nserver output tail:\n" + strings.Join(out, "\n")
}

// enforcedWithPackages is the plan's enforcement map with the exec line
// extended by the package versions the image build resolved — the one
// fact the compiler cannot know. A copy: the plan's map is the plan's.
func enforcedWithPackages(enforced map[string]string, packages []string) map[string]string {
	if len(packages) == 0 {
		return enforced
	}
	var resolved, missing []string
	for _, p := range packages {
		if what, ok := strings.CutPrefix(p, spec.ExecNotInstalledPrefix); ok {
			missing = append(missing, what)
		} else {
			resolved = append(resolved, p)
		}
	}
	out := maps.Clone(enforced)
	if len(resolved) > 0 {
		out["exec"] += "; packages resolved at image build: " + strings.Join(resolved, ", ")
	}
	if len(missing) > 0 {
		// Unsafe Mode's build goes on without what it could not
		// install, and the run without that tool.
		out["exec"] += "; NOT installed at image build (the commands named are absent from this run unless the base image already carries them): " + strings.Join(missing, "; ")
	}
	return out
}

// gatewayHostname is the gateway container's hostname: not a name any
// run container carries, and not hex, so it cannot resolve to a
// container id prefix either.
func gatewayHostname(runID string) string { return "gw-" + runID }

// runAgent runs the harness container under the budgets, streaming events.
func runAgent(ctx context.Context, d dockerx.Client, p Params, rd *runDirs,
	runID, token string, rep *strings.Replacer, task []byte, tools []string,
) agentOutcome {
	agentCtx, cancelTimeout := rd.harnessContext(ctx, p)
	defer cancelTimeout()
	agentCtx, cancelCause := context.WithCancelCause(agentCtx)
	defer cancelCause(nil)
	defer func() { rd.harnessStarts++ }()

	agentEnv, err := resolveEnv(p.Plan.Harness.Agent.Env, rep, token)
	if err != nil {
		return agentOutcome{err: err}
	}
	name := containerName(runID, "agent")
	hosts, err := sandboxHosts(ctx, d, p.Plan, runID)
	if err != nil {
		return agentOutcome{err: Classify(ExitGateway, err)}
	}
	runSpec := dockerx.RunSpec{
		Name:        name,
		Image:       p.Plan.Harness.Agent.Image,
		Entrypoint:  p.Plan.Harness.Agent.Entrypoint,
		Argv:        substituteAll(p.Plan.Harness.Agent.Argv, rep),
		Env:         agentEnv,
		Mounts:      resolveMounts(p.Plan.Harness.Agent.Mounts, rep),
		Network:     networkName(runID),
		Labels:      runLabels(runID),
		Interactive: p.Plan.Harness.Agent.StdinTask || p.Plan.Harness.OneTurn != nil,
		AutoRemove:  true,
		Runtime:     p.Plan.Runtime(),
		ExtraHosts:  hosts,
	}
	runSpec = hardenedAgent(runSpec, p)

	eventsFile, err := rd.harnessReceipt(filepath.Join(rd.receiptsDir, "events.ndjson"))
	if err != nil {
		return agentOutcome{err: Classify(ExitCLI, fmt.Errorf("create events receipt: %w", err))}
	}
	defer func() { _ = eventsFile.Close() }()

	// The stream is consumed as it arrives so --max-turns can stop a
	// looping agent instead of billing it to completion (exit 16).
	pr, pw := io.Pipe()
	collector := &eventCollector{
		adapter:    p.Adapter,
		events:     eventsFile,
		tee:        p.Events,
		onUsage:    p.OnUsage,
		onEvent:    p.OnEvent,
		stderr:     p.Stderr,
		redact:     rd.redact,
		maxTurns:   p.Budgets.MaxTurns,
		maxCostUSD: p.Budgets.MaxCostUSD,
		overBudget: func() {
			cancelCause(errBudget)
		},
		overCost: func() {
			cancelCause(errCostBudget)
		},
		signIn: p.Plan.Harness.ModelSignIn != nil,
		onPlanLimit: func() {
			cancelCause(errPlanLimit)
		},
		onOrgPolicy:    func() { cancelCause(errOrgPolicy) },
		onOutOfCredits: func() { cancelCause(errOutOfCredits) },
		base:           rd.harnessUsage,
	}
	stderrPath := filepath.Join(rd.receiptsDir, "agent-stderr.log")
	stderrFile, err := rd.harnessReceipt(stderrPath)
	if err != nil {
		return agentOutcome{err: Classify(ExitCLI, fmt.Errorf("create stderr receipt: %w", err))}
	}
	defer func() { _ = stderrFile.Close() }()

	var exit int
	var runErr, turnErr error
	if p.Plan.Harness.OneTurn != nil {
		// The task is the one turn of the harness's session (oneturn.go).
		_ = pr.Close()
		stdinR, stdinW := io.Pipe()
		stdoutR, stdoutW := io.Pipe()
		ran := make(chan struct{})
		go func() {
			defer close(ran)
			exit, runErr = dockerx.RunForegroundUnder(agentCtx, d, runSpec, "agent container", stdinR, stdoutW, stderrFile)
			_ = stdoutW.Close()
			_ = stdinR.Close()
		}()
		turnErr = oneTurn(agentCtx, p, rwPipe{Reader: stdoutR, WriteCloser: stdinW}, task, sessionOptions(p, rd, token, tools), collector, cancelCause)
		<-ran
	} else {
		done := consumeGuarded(collector, pr, cancelCause)
		var stdin io.Reader
		if runSpec.Interactive {
			stdin = bytes.NewReader(task)
		}
		exit, runErr = dockerx.RunForegroundUnder(agentCtx, d, runSpec, "agent container", stdin, pw, stderrFile)
		_ = pw.Close()
		<-done
	}
	// The harness wrote its stderr straight into the receipt; mask it now,
	// before the tail below quotes it.
	_ = stderrFile.Close()
	rd.redact.File(stderrPath)
	// The kill raced the docker client; make the container's absence a
	// fact rather than a hope. Not-found is the normal case (--rm).
	_ = d.RemoveForce(ctx, name)

	var refused *adapter.OrgPolicyRefusal
	out := agentOutcome{lastText: collector.lastText, answerInReasoning: collector.answerInReasoning(), usage: collector.usage()}
	switch {
	// First, because a panic that stopped the reader is the reason anything
	// else downstream looks wrong — the harness blocked on a pipe nobody was
	// draining, and so was killed.
	case collector.panicErr != nil:
		out.err = Classify(ExitCLI, collector.panicErr)
	// A plan limit is the outcome whatever the harness did next: it may
	// exit 0 after saying so, and a run its plan refused is no success.
	case collector.planLimit != nil:
		out.err = planLimitError(collector.planLimit.Text, collector.planLimit.ResetsAt)
	// So is an organization's policy: the vendor refused the run.
	case collector.orgPolicy != nil:
		out.err = orgPolicyError(collector.orgPolicy)
	// And a provider out of credits: the account that pays refused.
	case collector.outOfCredits != nil:
		out.err = outOfCreditsError(collector.outOfCredits.Text)
	case errors.As(turnErr, &refused):
		out.err = orgPolicyError(&adapter.Event{Policy: refused.Policy, Text: refused.Text})
	case context.Cause(agentCtx) == errBudget:
		out.err = Classify(ExitBudget, fmt.Errorf("turn budget exceeded (%d tool calls)", p.Budgets.MaxTurns))
	case context.Cause(agentCtx) == errCostBudget:
		out.err = Classify(ExitBudget, fmt.Errorf("cost budget exceeded (harness reported $%.4f, budget $%.2f)", collector.usage().CostUSD, p.Budgets.MaxCostUSD))
	case errors.Is(context.Cause(agentCtx), context.DeadlineExceeded):
		out.err = Classify(ExitTimeout, fmt.Errorf("run timed out after %ds", p.Budgets.TimeoutSeconds))
	case turnErr != nil:
		out.err = Classify(ExitHarness, turnFailure(turnErr, collector.harnessErrReported, stderrPath))
	case runErr != nil:
		out.err = Classify(ExitGateway, fmt.Errorf("agent container failed to run: %w", runErr))
	case exit != 0:
		detail := stderrTail(stderrPath)
		if collector.harnessErrReported != "" {
			detail = "\nharness error: " + collector.harnessErrReported + detail
		}
		out.err = Classify(ExitHarness, fmt.Errorf("agent exited %d%s", exit, detail))
		out.atCapacity = exitedAtCapacity(p, collector.harnessErrReported, stderrPath)
	}
	return out
}

// turnFailure is a one-turn run's failed turn, with the harness's own
// error when it reported one the turn's does not already say.
func turnFailure(turnErr error, reported, stderrPath string) error {
	detail := ""
	if reported != "" && !strings.Contains(turnErr.Error(), reported) {
		detail = "\nharness error: " + reported
	}
	return fmt.Errorf("the harness's turn failed: %w%s%s", turnErr, detail, stderrTail(stderrPath))
}

var (
	errBudget     = errors.New("turn budget exceeded")
	errCostBudget = errors.New("cost budget exceeded")
	errPlanLimit  = errors.New("plan limit reached")
	// errOutOfCredits ends a run whose provider said the paying account has
	// no credits left.
	errOutOfCredits = errors.New("out of credits")
	errOrgPolicy    = errors.New("refused by an organization's policy")
)

// outputSchema loads and compiles the package's output.schema.json for a
// json-kind plan. The bytes are re-hashed against the plan: the
// plan is what was accepted at install, the workspace is what the run
// reads, and they must agree.
func outputSchema(p Params) (*jsonschema.Schema, error) {
	if p.Plan.Harness.ResultKind != "json" || p.Plan.Output.SchemaFile == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(filepath.Join(p.WorkspaceSrc, filepath.FromSlash(p.Plan.Output.SchemaFile))) //nolint:gosec // plan-named file under the verified workspace
	if err != nil {
		return nil, Classify(ExitCompile, fmt.Errorf("read %s: %w", p.Plan.Output.SchemaFile, err))
	}
	if sum := sha256.Sum256(raw); p.Plan.Output.SchemaSHA256 != "" && hex.EncodeToString(sum[:]) != p.Plan.Output.SchemaSHA256 {
		return nil, Classify(ExitVerify, fmt.Errorf("%s does not match the plan's recorded digest — the workspace changed after install", p.Plan.Output.SchemaFile))
	}
	sch, err := compile.CompileOutputSchema(raw)
	if err != nil {
		return nil, Classify(ExitCompile, fmt.Errorf("%s: %w", p.Plan.Output.SchemaFile, err))
	}
	return sch, nil
}

// finalResult applies the output convention: json-kind results must
// parse AND validate against output.schema.json (exit 17 otherwise, the
// violation named); text results pass through.
func finalResult(kind, lastText string, schema *jsonschema.Schema) (json.RawMessage, error) {
	if kind != "json" {
		return receipt.TextResult(lastText), nil
	}
	candidate := stripJSONFences(lastText)
	var probe any
	if err := json.Unmarshal([]byte(candidate), &probe); err != nil {
		return nil, Classify(ExitOutputSchema,
			fmt.Errorf("package declares %s but the final message is not valid JSON: %v (raw output preserved in receipts)", compile.OutputSchemaFile, err))
	}
	if schema != nil {
		// Validate the value the way the schema library expects it (its own
		// JSON decoder keeps numbers exact).
		val, err := jsonschema.UnmarshalJSON(strings.NewReader(candidate))
		if err != nil {
			return nil, Classify(ExitOutputSchema, fmt.Errorf("final message: %v", err))
		}
		if err := schema.Validate(val); err != nil {
			return nil, Classify(ExitOutputSchema,
				fmt.Errorf("final message does not satisfy %s: %v (raw output preserved in receipts)", compile.OutputSchemaFile, err))
		}
	}
	return json.RawMessage(candidate), nil
}

// errAnswerInReasoning refuses a schema-checked run whose model ended on
// reasoning. The runner stays strict, as for leaked markup: reasoning is
// the model's working, not its answer, and docker-agent itself does not
// use it as the response. The reason is there so the run does not read
// "not valid JSON" over an empty message.
func errAnswerInReasoning() error {
	return Classify(ExitOutputSchema, receipt.WithReason(
		fmt.Errorf("package declares %s but the model sent no final message after its reasoning: an answer written only in the model's reasoning is not taken as the result (raw output preserved in receipts)", compile.OutputSchemaFile),
		receipt.ReasonAnswerInReasoning, ""))
}

// JSONResult is the output convention for a json-kind result, as the run
// itself applies it: a host post-step that recovers an answer from a
// message the run refused holds it to exactly this (the Examiner's).
func JSONResult(text string, schema *jsonschema.Schema) (json.RawMessage, error) {
	return finalResult("json", text, schema)
}

// stripJSONFences unwraps a ```json … ``` fenced final message — models
// add fences no matter how firmly the prompt says not to.
func stripJSONFences(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	t = strings.TrimPrefix(t, "```json")
	t = strings.TrimPrefix(t, "```")
	t = strings.TrimSuffix(strings.TrimSpace(t), "```")
	return strings.TrimSpace(t)
}

// eventCollector consumes the harness NDJSON stream line by line.
type eventCollector struct {
	adapter adapter.Adapter
	events  io.Writer
	// redact masks the run's secret values out of each event line before
	// it reaches the receipt; the line is parsed as the harness sent it.
	redact     *redactor
	maxTurns   int
	overBudget func()
	// maxCostUSD > 0 makes the harness's reported running cost a hard stop.
	// docker-agent's usage events carry cost as a running total, so the
	// collector keeps its high-water mark: a harness that reported per-call
	// costs would under-enforce, never over-kill. Tokens are the opposite,
	// and are summed: docker-agent's token_usage carries that model
	// request's own tokens (fresh plus cached input), and claude and codex
	// report once per autonomous run (usage_recorded_test.go pins this
	// against the recorded streams).
	maxCostUSD float64
	overCost   func()
	// signIn says the run is paid by a model sign-in: a harness's reported
	// cost is not the person's to pay, so it is not recorded, and
	// onPlanLimit ends the run when the harness reports the plan's limit,
	// keeping its words in planLimit and planResets.
	signIn      bool
	onPlanLimit func()
	planLimit   *adapter.Event
	// onOrgPolicy ends the run when the harness reports that a policy of
	// the person's organization refused it, keeping the event.
	onOrgPolicy func()
	orgPolicy   *adapter.Event
	// outOfCredits is the provider's first out-of-credits answer, and
	// onOutOfCredits what ends the run on it, as a plan's limit does.
	outOfCredits   *adapter.Event
	onOutOfCredits func()

	turns         int
	inTok, outTok int64
	costUSD       float64
	// curText accumulates the streamed deltas of the assistant message in
	// progress; lastText is the complete text of the most recent message
	// (the result is the whole message, not a streaming provider's last
	// delta). A tool call or a usage event ends a message.
	curText  strings.Builder
	lastText string
	// Since the last tool call, the model reasoned (reasoned) and wrote
	// answer text (answered). A run that ends having reasoned and not
	// answered put whatever it meant to answer in its reasoning, which is
	// never the result; text then reasoning is an answer.
	reasoned, answered bool
	budgetTripped      bool
	costTripped        bool
	harnessErrReported string
	// panicErr is set when the consuming goroutine recovered a panic. It is
	// the run's error: a panic in the adapter's mapper or the tee writer
	// would otherwise take the process down, and teardown is a defer in
	// Run's own goroutine, which a dying goroutine never runs.
	panicErr error
	// totTok accumulates a harness's context-token reports, for the
	// harnesses that give a total without an input/output split.
	totTok int64
	// onUsage is fired with the running totals on every usage event, and
	// tee receives every raw line beside the receipt's own copy.
	onUsage func(receipt.Usage)
	tee     io.Writer
	// onEvent receives every normalized event, masked (Params.OnEvent).
	onEvent func(adapter.Event)
	// toolNames remembers each call's tool by its id, for a result that
	// does not name it.
	toolNames map[string]string
	// liveText joins the assistant's streamed chunks until the message
	// ends, so that the message is masked whole: a secret split across
	// two chunks matches in neither.
	liveText strings.Builder
	// stderr hears that a panicking onEvent was switched off.
	stderr io.Writer
	// usageErr records a callback that panicked, so a driver metering the
	// run learns its meter is dead instead of reading silence as zero.
	usageErr error
	// base is the usage of the harness's earlier starts, when it was
	// started again after a capacity answer (capacity.go).
	base receipt.Usage
}

// consumeGuarded runs the collector's consume in its own goroutine and returns
// the channel that closes when it is done.
//
// A panic in the adapter's mapper or the --events tee writer would otherwise
// take the whole process down, and teardown is a defer in the caller's
// goroutine: a dying goroutine runs none of it, which leaves the agent
// container holding a valid gateway token, the gateway holding the docker
// socket, and the run network and secrets.env on disk — bounded by nothing,
// since the turn, cost and wall budgets are all enforced by the process that
// died. So the panic is recovered, recorded as the collector's error, and the
// pipe is closed with it, which makes the harness's next write fail instead of
// blocking forever on a reader that is gone. onCancel ends the agent's context
// for the case where nothing is writing: a docker client blocked reading the
// attach stream never notices the closed pipe, and without it the run would
// sit there with its budget goroutine dead and the sandbox up.
//
// Both call sites — the container agent and the host-run harness — go through
// this, because they are the same shape and hardening one copy of it left the
// other one leaking.
func consumeGuarded(c *eventCollector, pr *io.PipeReader, onCancel func(error)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				c.panicErr = fmt.Errorf("event stream panicked: %v", r)
				_ = pr.CloseWithError(c.panicErr)
				if onCancel != nil {
					onCancel(c.panicErr)
				}
			}
		}()
		c.consume(pr)
	}()
	return done
}

func (c *eventCollector) consume(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		c.recordRaw(line)
		for _, ev := range c.adapter.Events(line) {
			c.record(ev)
		}
	}
	c.flushLiveText()
}

// recordRaw keeps one harness line in the events receipt, masked, and
// hands it to the tee.
func (c *eventCollector) recordRaw(line []byte) {
	masked := append([]byte(c.redact.String(string(line))), '\n')
	_, _ = c.events.Write(masked)
	if c.tee != nil {
		// A driver reading this file is metering a live run, so the
		// line goes out as it arrives; a write it cannot take is its
		// problem, never the run's.
		_, _ = c.tee.Write(masked)
	}
}

// record applies one normalized event: the turn and cost budgets, the
// result text, the usage, the harness's error and a plan's limit.
func (c *eventCollector) record(ev adapter.Event) {
	ev = c.pair(ev)
	defer c.notifyEvent(ev)
	switch ev.Kind {
	case adapter.EventToolCall:
		c.curText.Reset() // text before a tool call is its own message
		c.reasoned, c.answered = false, false
		c.turns++
		if c.maxTurns > 0 && c.turns > c.maxTurns && !c.budgetTripped {
			c.budgetTripped = true
			c.overBudget()
		}
	case adapter.EventReasoning:
		c.reasoned = true
	case adapter.EventAssistantText:
		// Whitespace alone is not an answer.
		if strings.TrimSpace(ev.Text) != "" {
			c.answered = true
		}
		switch {
		case ev.Text == "":
		case ev.Delta:
			c.curText.WriteString(ev.Text)
			c.lastText = c.curText.String()
		default:
			c.curText.Reset()
			c.lastText = ev.Text
		}
	case adapter.EventUsage:
		c.curText.Reset() // usage is reported once the message is complete
		c.inTok += ev.InputTokens
		c.outTok += ev.OutputTokens
		c.totTok += ev.TotalTokens
		if ev.CostUSD > c.costUSD && !c.signIn {
			c.costUSD = ev.CostUSD
		}
		if c.maxCostUSD > 0 && c.base.CostUSD+c.costUSD > c.maxCostUSD && !c.costTripped && c.overCost != nil {
			c.costTripped = true
			c.overCost()
		}
		c.notifyUsage()
	case adapter.EventError:
		c.harnessErrReported = ev.Text
	case adapter.EventPlanLimit:
		if c.planLimit == nil {
			c.planLimit = &ev
			if c.onPlanLimit != nil {
				c.onPlanLimit()
			}
		}
	case adapter.EventOutOfCredits:
		c.harnessErrReported = ev.Text
		if c.outOfCredits == nil {
			c.outOfCredits = &ev
			if c.onOutOfCredits != nil {
				c.onOutOfCredits()
			}
		}
	case adapter.EventOrgPolicy:
		if c.orgPolicy == nil {
			c.orgPolicy = &ev
			if c.onOrgPolicy != nil {
				c.onOrgPolicy()
			}
		}
	}
}

// notifyUsage hands the running totals to the driver's callback without
// letting the driver end the run. This goroutine enforces the turn and cost
// budgets and its end releases teardown, so a panicking callback would
// leave containers up; one that panics is reported through the run's
// own error channel and then switched off, because a callback that
// failed once will fail on every event.
func (c *eventCollector) notifyUsage() {
	if c.onUsage == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			c.onUsage = nil
			c.usageErr = fmt.Errorf("usage callback panicked: %v", r)
		}
	}()
	c.onUsage(c.usage())
}

// pair remembers a tool call's tool by its id and names it on the result
// that answers it, for the harnesses whose results name only the call.
func (c *eventCollector) pair(ev adapter.Event) adapter.Event {
	if ev.CallID == "" {
		return ev
	}
	switch ev.Kind {
	case adapter.EventToolCall:
		if ev.Tool != "" {
			if c.toolNames == nil {
				c.toolNames = map[string]string{}
			}
			c.toolNames[ev.CallID] = ev.Tool
		}
	case adapter.EventToolResult:
		if ev.Tool == "" {
			ev.Tool = c.toolNames[ev.CallID]
		}
		delete(c.toolNames, ev.CallID)
	}
	return ev
}

// notifyEvent hands one event to the driver's callback. A streamed chunk
// of the assistant's message is held until the message ends, where the
// result's own text ends it: at a tool call, a tool's result, the usage,
// a whole message or the harness's error.
func (c *eventCollector) notifyEvent(ev adapter.Event) {
	if c.onEvent == nil {
		return
	}
	switch ev.Kind {
	case adapter.EventAssistantText:
		if ev.Delta {
			c.liveText.WriteString(ev.Text)
			return
		}
		c.liveText.Reset()
	case adapter.EventToolCall, adapter.EventToolResult, adapter.EventUsage, adapter.EventError,
		adapter.EventPlanLimit, adapter.EventOutOfCredits, adapter.EventOrgPolicy:
		c.flushLiveText()
	}
	c.deliver(ev)
}

// flushLiveText delivers the message the streamed chunks have built so far
// as one event. The stream's end calls it too, so the last message is
// never lost.
func (c *eventCollector) flushLiveText() {
	if c.liveText.Len() == 0 {
		return
	}
	text := c.liveText.String()
	c.liveText.Reset()
	c.deliver(adapter.Event{Kind: adapter.EventAssistantText, Text: text})
}

// deliver masks one event, cuts a tool's output, drops the raw line and
// calls the driver, on the terms of notifyUsage: a callback that panics is
// switched off, said once on stderr, and never ends the run.
func (c *eventCollector) deliver(ev adapter.Event) {
	if c.onEvent == nil {
		return
	}
	ev.Raw = nil
	ev.Delta = false
	ev.Text = c.redact.String(ev.Text)
	if ev.Kind == adapter.EventToolResult {
		// Masked first: a cut through a secret would leave a prefix no
		// mask matches.
		ev.Text = adapter.ToolExcerpt(ev.Text)
	}
	ev.Tool = c.redact.String(ev.Tool)
	ev.CallID = c.redact.String(ev.CallID)
	ev.Policy = c.redact.String(ev.Policy)
	defer func() {
		if r := recover(); r != nil {
			c.onEvent = nil
			if c.stderr != nil {
				_, _ = fmt.Fprintf(c.stderr, "warning: the run's live events stopped: their receiver failed (%s); the run goes on and its receipts keep every event\n", c.redact.String(fmt.Sprint(r)))
			}
		}
	}()
	c.onEvent(ev)
}

// answerInReasoning: since the last tool call the model reasoned and wrote
// no answer.
func (c *eventCollector) answerInReasoning() bool { return c.reasoned && !c.answered }

func (c *eventCollector) usage() receipt.Usage {
	return withBase(receipt.Usage{
		InputTokens:  c.inTok,
		OutputTokens: c.outTok,
		TotalTokens:  c.totTok,
		CostUSD:      c.costUSD,
		Turns:        c.turns,
	}, c.base)
}

// sandboxHosts pins the run network's two aliases — the gateway and the
// llm-proxy — into the /etc/hosts of a run container under the plan's
// runtime, the agent and the gateway's health probe: a sandboxed runtime's
// own network stack does not reach docker's embedded DNS at 127.0.0.11,
// and those two names are the whole world of both. Without a runtime
// nothing is pinned and the run is the run it was. Both containers are up
// before either starts (startInfra).
func sandboxHosts(ctx context.Context, d dockerx.Client, plan *spec.Plan, runID string) ([]string, error) {
	if plan.Runtime() == "" {
		return nil, nil
	}
	net := networkName(runID)
	var hosts []string
	for _, c := range []struct{ name, alias string }{
		{containerName(runID, "gateway"), spec.GatewayAlias},
		{containerName(runID, "proxy"), spec.ProxyAlias},
	} {
		ip, err := d.ContainerIP(ctx, c.name, net)
		if err != nil {
			return nil, fmt.Errorf("pin %s for the sandboxed agent: %w", c.alias, err)
		}
		hosts = append(hosts, c.alias+":"+ip)
	}
	return hosts, nil
}

// policyRecord is the envelope's policy block: the file and source the
// caller resolved, with the digest the plan was compiled against — the
// digest of that same file's bytes, since a plan compiled under another
// policy is recompiled before it runs.
func policyRecord(p Params) *receipt.Policy {
	if p.PolicyFile == nil {
		return nil
	}
	return &receipt.Policy{Path: p.PolicyFile.Path, Source: p.PolicyFile.Source, SHA256: p.Plan.PolicySHA256}
}
