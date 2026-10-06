// Package engine executes a compiled run plan (RUNNER-PLAN §R6): render
// the plan's placeholder-bearing files into a per-run directory, resolve
// secrets, stand up the run-scoped network + proxy + gateway + agent
// containers, stream and budget the harness event stream, collect the
// result envelope, and tear everything down on every exit path. The
// engine trusts the plan (compile owns policy) and owns exactly the
// run-scoped values plans must not contain: paths, tokens, secret values.
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
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/intake"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// Exit codes (RUNNER-PLAN §R8). The distinguishable failure classes are
// the runner's composability contract: a cron job or pipeline branches on
// these, so each class has exactly one meaning.
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
	// by its own end_blocked or by the runner's evidence (blocked.go;
	// agentry-cli#629). A release's one-shot run alone.
	ExitBlocked = 18
)

// ClassifiedError pairs an error with its §R8 exit class so main can exit
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
	// generated files live (§R6 install layout).
	Plan    *spec.Plan
	PlanDir string
	// WorkspaceSrc is the pristine verified workspace to copy per run.
	WorkspaceSrc string
	// CheckWorkspace, when set, is handed the run's copy of WorkspaceSrc
	// before anything reads it, and an error refuses the run with the
	// verify class: an installed release's copy is held to its integrity
	// record there (installdir.CheckCopy, agentry-cli#326), so an edit
	// made to the install after the client's own check does not run.
	CheckWorkspace func(copyDir string) error
	// RunRoot is where runs/<id>/ ephemera live (the install dir).
	RunRoot string
	// RunnerHome is the runner home, shared by every client on the host.
	// A run whose plan needs relays holds its relay lock (RelayLockFile)
	// for the whole run (agentry-cli#268). Empty takes no lock, leaving
	// the relay listing as the only check.
	RunnerHome string
	// OutDir is the mounted output folder (RQ3), already absolute.
	OutDir string
	// Task is the task input (may be nil for an empty task). An empty
	// task on the autonomous path sends the agent's own instruction
	// instead (agentry-cli#449).
	Task io.Reader
	// InputPaths are operator-provided files/directories copied into the
	// run's read-only inputs channel (RUNNER-PLAN-2 §P4).
	InputPaths []string
	// Knowledge are the knowledge packs attached to the run (plan
	// knowledge.md §4.5): each copied into the run's read-only /knowledge
	// channel and verified there, the three knowledge tools granted and the
	// receipt's knowledge line written only when there is one (DR-38).
	Knowledge []AttachedPack
	// Voice is the person's Writing Voice when the run attaches it (plan
	// writing-voice.md §5): written under the run's /knowledge channel,
	// voice_read granted and the receipt's voice line written only then.
	// A built-in's run never carries one (D5).
	Voice *AttachedVoice
	// Decoded, when set, is the decode step the caller ran on the inputs
	// before this run (pkg/runner/decode); the receipt names it.
	Decoded *decode.Result
	// TextInputs says the caller staged only files it made itself (every
	// built-in: localrun.Run sets it): the engine does not decode them.
	// Otherwise a run's inputs are decoded before the agent starts (plan
	// file-handling.md §5, agentry-cli#560).
	TextInputs bool
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
	// Intake holds the answers to the manifest's up-front questions (plan
	// §3.5): staged as /inputs/intake.json, file answers staged like inputs,
	// secret answers already merged into SecretOverrides by the caller.
	Intake *intake.Answers
	// Adapter interprets the harness event stream.
	Adapter adapter.Adapter
	// Policy supplies the secret source.
	Policy *policy.Policy
	// PolicyFile names the operator policy file Policy was read from and
	// how its path was chosen (policy.Resolve); the envelope records it
	// with the plan's policySha256 as its digest (agentry-cli#152). Nil
	// leaves the envelope's policy block out.
	PolicyFile *policy.File
	// SecretOverrides resolve declared secrets ahead of the policy source
	// — the QuickAgent's MCP profile rides in here (P2 §P2); the engine
	// itself stays profile-agnostic.
	SecretOverrides map[string]string
	// Connections are the access tokens of the servers that sign in by
	// OAuth, keyed by spec.MCPServerSpec.ConnectionKey (a remote server's
	// address, a package server's "pypi:name"): the caller refreshed each
	// from the person's connection (pkg/runner/connection, agentry-cli#664,
	// #665). An access token is all a run is given of one; a server with
	// none here refuses the run.
	Connections map[string]ConnectionToken
	// RenewConnection, when set, is asked for a new access token for a
	// server (by its ConnectionKey) before the one the run holds expires
	// (plan mcp-oauth-connections.md D24): the engine rewrites the
	// server's connection folder with it, which a bridge and a server that
	// re-reads its token file pick up. Nil leaves each token as given for
	// the whole run.
	RenewConnection func(ctx context.Context, server string) (ConnectionToken, error)
	// ServersDir is the signature's installed MCP-server store
	// (RUNNER-PLAN-3), substituted for the PHServers placeholder.
	// Required when the plan declares MCP servers; unused otherwise.
	ServersDir string
	// Budgets are the effective hard stops (plan defaults merged with
	// any tighter run flags by the caller).
	Budgets spec.Budgets
	// Attestation is the verify-gate outcome for the envelope.
	Attestation receipt.Attestation
	// ExecPackages are the catalogued packages' resolved versions the exec
	// image build recorded ("name-version"); appended to the receipt's
	// exec line so it names what was installed (agentry-cli#100).
	ExecPackages []string
	// KeepRunDir preserves runs/<id>/ for debugging (secrets.env is
	// still always deleted).
	KeepRunDir bool
	// RunID names the run; "" mints one. The CLI passes the id it used
	// for the default output folder so the two always agree.
	RunID string
	// Stderr receives progress and diagnostics.
	Stderr io.Writer
	// OnUsage, when set, is called every time the harness reports usage,
	// with the run's running totals. It is what lets a driver meter a run
	// while it is live: the envelope's usage arrives only at the end, so
	// before this nothing could enforce a token cap during the run
	// (agentry-cli#138). Called from the stream-consuming goroutine, in
	// order, and never after the harness stream closes.
	//
	// It MUST NOT BLOCK. That goroutine is also what enforces --max-turns
	// and --max-cost, and its end is what releases teardown (§R11), so a
	// callback that waits stops the budgets and leaves containers up. A
	// callback that panics is caught and switched off rather than allowed
	// to take the teardown down with it.
	OnUsage func(receipt.Usage)
	// Events, when set, receives the harness event stream as JSON lines
	// while the run is live — the same lines the receipt's events.ndjson
	// gets, redacted the same way. A driver tails this instead of parsing
	// the narration (--events). It must not block, for the same reason
	// OnUsage must not: it is written from the same goroutine, on the same
	// terms as the receipt's own copy.
	Events io.Writer
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
			Adapter: p.Plan.Harness.Adapter,
			Image:   p.Plan.Harness.Agent.Image,
			Model:   p.Plan.Harness.Model,
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

	// Everything below must tear down whatever it built, success or not
	// (§R11 invariant 6): td runs in reverse order via defer, then sweeps
	// anything still carrying this run's label.
	// What a run whose process died left behind goes first, before this
	// run adds its own (agentry-cli#161).
	SweepAbandoned(ctx, d, p.Stderr)
	td := &teardown{d: d, stderr: p.Stderr, runID: runID}
	defer func() {
		td.run()
		// What teardown could not delete turns the run's outcome
		// (agentry-cli#422).
		if err := td.reportLeftFiles(env); err != nil && outErr == nil {
			outErr = err
		}
	}()

	rd, err := prepareRunDirs(p, runID, td)
	if err != nil {
		return fail(env, redact, err)
	}
	// The inputs are decoded before anything starts (plan
	// file-handling.md §5, agentry-cli#560).
	dec, err := decodeInputs(ctx, d, p, rd)
	if err != nil {
		return fail(env, redact, err)
	}
	if line := dec.line(); line != "" {
		env.CapabilitiesEnforced["decode"] = line
	}
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
	// The optional secrets it goes without, before any replacer is built:
	// the System Prompt names them (spec.PHSecretsAbsent), and so does the
	// receipt (DR-70).
	rd.secretsAbsent = absentSecrets(p.Plan, secrets)
	env.CapabilitiesEnforced = withAbsentNote(env.CapabilitiesEnforced, rd.secretsAbsent)
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
	// lives (plan D24); teardown stops the renewer before it deletes the
	// connection folders.
	stopRenewal := renewConnections(ctx, p, rd)
	td.add(func(context.Context) { stopRenewal() })

	env.Stage = receipt.StageInfra
	err = startInfra(ctx, d, p, rd, td, runID, token, rep, nil)
	if len(rd.modelHosts) > 0 {
		env.CapabilitiesEnforced["llm-proxy.hosts"] = modelHostsLine(rd.modelHosts)
	}
	if err != nil {
		env.CapabilitiesEnforced["daemon"] = daemonLine(rd)
		return fail(env, redact, withRefusals(err, rd))
	}
	// Harness files are rendered once the infrastructure is up: a host-run
	// harness (plan runner-m-r4-adapters §5) needs the published gateway
	// and proxy addresses, known only now; the replacer learns them too.
	rep = rd.replacer()
	if err := renderPlanFiles(p.Plan, p.PlanDir, rd, nativePath, "harness"); err != nil {
		return fail(env, redact, err)
	}

	env.Stage = receipt.StageAgent
	var outcome agentOutcome
	if p.Plan.Harness.Host != nil {
		outcome = runHostHarness(ctx, p, rd, token, rep, task)
	} else {
		outcome = runAgent(ctx, d, p, rd, runID, token, rep, task)
	}
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
	// are the point of tearing down cleanly (agentry-cli#124).
	keep := context.WithoutCancel(ctx)
	if logs, lerr := d.Logs(keep, containerName(runID, "gateway"), 2000); lerr == nil && logs != "" {
		_ = os.WriteFile(filepath.Join(rd.receiptsDir, "gateway.log"), []byte(redact.String(logs)), 0o644)
	}
	// The proxy's decisions (allow/deny per CONNECT) are the evidence of
	// what left the run (DR-6); collected the same way.
	if logs, lerr := d.Logs(keep, containerName(runID, "proxy"), 2000); lerr == nil && logs != "" {
		_ = os.WriteFile(filepath.Join(rd.receiptsDir, "proxy.log"), []byte(redact.String(logs)), 0o644)
	}
	// The calls ledger holds the agent's own end_blocked sentence: masked
	// like every receipt the run could have written into.
	if rd.callsLedger != "" {
		rd.redact.File(rd.callsLedger)
	}
	writeMeta(rd.receiptsDir, runID, p, start, taskSent)
	env.CapabilitiesEnforced["daemon"] = daemonLine(rd)
	// The agent has ended, so the web ledger is complete.
	if env.Web = webRecord(rd.receiptsDir, p.Web); env.Web != nil {
		env.CapabilitiesEnforced["web.aiReview"] = webLine(env.Web)
	}
	if rs := p.Plan.WebResolvers(); len(rs) > 0 {
		env.CapabilitiesEnforced["web.resolvers"] = resolversLine(rs)
	}
	if env.Review = reviewRecord(rd.receiptsDir, p.Review); env.Review != nil {
		env.CapabilitiesEnforced["review.aiReview"] = reviewLine(env.Review)
	}

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

	env.Stage = receipt.StageOutput
	schema, serr := outputSchema(p)
	if serr != nil {
		env.Result = receipt.TextResult(outcome.lastText)
		return fail(env, redact, serr)
	}
	result, rerr := finalResult(p.Plan.Harness.ResultKind, outcome.lastText, schema)
	if rerr != nil {
		env.Result = receipt.TextResult(outcome.lastText)
		return fail(env, redact, rerr)
	}
	env.Result = result
	env.Status = "ok"
	env.ExitCode = ExitOK
	env.Stage = ""
	if err := env.Write(p.OutDir); err != nil {
		_, _ = fmt.Fprintf(p.Stderr, "warning: %v\n", err)
	}
	return env, nil
}

// fail stamps the envelope with the classified failure — its text masked
// by the run's redactor, since a gateway log tail or an agent's stderr
// tail rides along in it — writes it to the output folder (a failed run
// still leaves its record, RQ3), and returns.
func fail(env *receipt.Envelope, r *redactor, err error) (*receipt.Envelope, error) {
	// The reason is read before the redactor, which rebuilds a masked
	// error without the chain; it holds no value to mask (receipt.Reason).
	env.Reason = receipt.ReasonOf(err)
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
	usage    receipt.Usage
	err      error // classified, nil on success
	// hostInfo names the executable (and version) a host-run harness
	// ran, for the envelope; hostEnv the host variables it could inherit.
	hostInfo string
	hostEnv  []string
}

// startInfra creates the run network and starts proxy + gateway, then
// health-probes the gateway from inside the network (§R6 execute order).
// Every failure here is the §R8 gateway/environment class. When served is
// non-nil (a session's), the probe also lists the tools the gateway serves
// and startInfra stores them there (agentry-cli#578).
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

	// LLM egress proxy: the agent container's only route out (§R4). It is
	// also the one run container with a bridge leg, so for a host-run
	// harness it is the host's door to everything: its own CONNECT port
	// and a plain TCP forward to the gateway, which never leaves the run
	// network (a gateway on the bridge makes every MCP server it launches
	// inherit that mix of networks, which docker refuses; agentry-cli#105).
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
	// (modelhosts.go, agentry-cli#592).
	rd.modelHosts = modelHostPins(ctx, p.Plan.Proxy.Allow)
	proxySpec.ExtraHosts = append(proxySpec.ExtraHosts, rd.modelHosts...)
	// Under the run's runtime where the host's name servers are named
	// (agentry-cli#337): it reads the run's own resolv.conf, and its egress
	// leg is a network of the run's own joined at create, since a
	// sandboxed runtime takes no network connected after start and docker
	// refuses the default bridge beside the run network at create. A
	// host-run harness's proxy publishes ports to the host and stays on
	// the daemon's default, as measured.
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
	if err := d.RunDetached(ctx, proxySpec); err != nil {
		return Classify(ExitGateway, fmt.Errorf("start llm proxy: %w", err))
	}
	td.add(func(c context.Context) { _ = d.RemoveForce(c, proxyName) })
	// The proxy needs a route to the model endpoint; the run network is
	// internal by design, so it joins the default bridge as its second
	// leg (or, sandboxed, took its own egress network at create). The
	// agent container never does (§R11 invariant 2).
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

	gwName := containerName(runID, "gateway")
	// The gateway's env names host paths (its bind allow-lists) in the
	// gateway's spelling; its mounts are made by this CLI, in the host's,
	// and its argv names only paths inside it (agentry-cli#414).
	gwEnv, err := rd.gatewayEnv(p.Plan, token)
	if err != nil {
		return err
	}
	gwSpec := dockerx.RunSpec{
		Name:    gwName,
		Image:   p.Plan.Gateway.Image,
		Argv:    append(substituteAll(p.Plan.Gateway.Args, rep), gatewayLimitArgs(p.limits())...),
		Env:     gwEnv,
		Mounts:  resolveMounts(p.Plan.Gateway.Mounts, rep),
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
		// networks its contract calls for (agentry-cli#105).
		Hostname: gatewayHostname(runID),
		Labels:   labels,
		// The gateway is the one container that drives the host daemon
		// (§R4 calls that its documented privilege level, and it is why the
		// agent container never sees this socket). An SELinux-enabled
		// daemon would otherwise refuse it the socket outright.
		DisableSELinuxLabel: mountsDockerSocket(p.Plan.Gateway.Mounts),
	}
	// Its root stays writable: a vendor image, whose writes are its own.
	// It opens the socket as its owner, root, with no capability; reading
	// the run's secrets file, 0600 and the invoking user's, takes
	// DAC_READ_SEARCH, which it keeps and nothing else.
	gwSpec = hardened(gwSpec, p.limits())
	gwSpec.CapAdd = []string{"DAC_READ_SEARCH"}
	// The gateway's own runtime, a registration that opens the bind-mounted
	// socket of the run's docker filter (docker.gatewayRuntime,
	// agentry-cli#337); "" is the daemon's default.
	if gatewaySandboxed(p.Plan) {
		gwSpec.Runtime = p.Plan.GatewayRuntime()
	}
	// A run that will give the gateway relays to start: where the docker
	// filter runs, the relay networks are its own (agentry-cli#220);
	// elsewhere it refuses to share the host's one relay network with
	// another gateway's (relays.go, agentry-cli#108, DR-22) and holds the
	// runner home's relay lock from the listing to the end of its teardown,
	// so two such runs starting together cannot both list an empty network
	// (relaylock.go, agentry-cli#268). Either way the release or removal is
	// queued before the gateway's teardown, so it runs after the relays
	// are reaped.
	switch {
	case !relaysNeeded(p.Plan):
	case filterSupported:
		// The gateway's two fixed relay networks are this run's own, made
		// here and removed with the run; the filter translates the names
		// (dockerfilter.go, agentry-cli#220). Runs with relays share a host
		// without seeing each other's.
		if err := makeRelayNetworks(ctx, d, runID, td); err != nil {
			return err
		}
	default:
		// Without the filter the gateway's relay network is the host's one
		// fixed network (DR-22): such runs take turns on the host.
		release, err := holdRelayLock(p.RunnerHome)
		if err != nil {
			return err
		}
		td.add(func(context.Context) { release() })
		if err := refuseForeignRelays(ctx, d); err != nil {
			return err
		}
	}
	// The web tools' server has a network of the run's own, where the
	// filter runs; elsewhere the gateway gives it the daemon's default and
	// the receipt's daemon line says the filter did not run. The tools
	// hold their own address rule either way (toolsrv/web.go).
	if filterSupported && p.Plan.Capabilities.WebTools {
		if err := makeWebNetwork(ctx, d, runID, td); err != nil {
			return err
		}
	}
	// The gateway's daemon socket is this run's filter, not the daemon's
	// (dockerfilter.go, agentry-cli#219). Queued before the gateway's
	// teardown, so it closes after the gateway has stopped and reaped the
	// containers it launched through it.
	if filterSupported {
		filter, sock, err := startDaemonFilter(rd, runID, gwEnv, p.limits(), p.Plan, p.RunnerHome)
		if err != nil {
			return err
		}
		rd.filter = filter
		td.add(func(context.Context) { closeDaemonFilter(rd) })
		gwSpec.Mounts = throughFilter(gwSpec.Mounts, sock)
	}
	if err := d.RunDetached(ctx, gwSpec); err != nil {
		return Classify(ExitGateway, fmt.Errorf("start mcp gateway: %w", err))
	}
	// Graceful stop first: the gateway reaps its tool-server containers
	// on SIGTERM; rm -f would orphan them. Its relay sidecars it does not
	// reap on our stop (Docker 29.7.2, agentry-cli#106), so they are removed
	// here by the names its log gave them (relays.go), before the log goes
	// with the container.
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
	// abort rather than degrade to tool-less execution (§6.1 annex rule).
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
	// given the gateway's address rather than its name (agentry-cli#221).
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
	code, perr := d.RunForeground(ctx, probeSpec, nil, listing, p.Stderr)
	if perr != nil || code != 0 {
		diag := gatewayDiagnostics(ctx, d, gwName)
		if perr == nil {
			perr = fmt.Errorf("gateway health probe failed (exit %d)", code)
		}
		return Classify(ExitGateway, fmt.Errorf("%w%s", perr, diag))
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
	// serves the probe, so the log is complete by now.
	// The tools server is checked the same way, on every run: a gateway
	// that could not start it (a relay the docker filter refused, say)
	// carries on with the declared servers alone, and the agent would run
	// without the tools its contract grants (agentry-cli#346).
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
		// Unsafe Mode's build goes on without what it could not install
		// (plan unsafe-mode.md D8), and the run without that tool.
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
	runID, token string, rep *strings.Replacer, task []byte,
) agentOutcome {
	agentCtx := ctx
	var cancelTimeout context.CancelFunc
	if p.Budgets.TimeoutSeconds > 0 {
		agentCtx, cancelTimeout = context.WithTimeout(agentCtx, time.Duration(p.Budgets.TimeoutSeconds)*time.Second)
		defer cancelTimeout()
	}
	agentCtx, cancelCause := context.WithCancelCause(agentCtx)
	defer cancelCause(nil)

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
		Argv:        substituteAll(p.Plan.Harness.Agent.Argv, rep),
		Env:         agentEnv,
		Mounts:      resolveMounts(p.Plan.Harness.Agent.Mounts, rep),
		Network:     networkName(runID),
		Labels:      runLabels(runID),
		Interactive: p.Plan.Harness.Agent.StdinTask,
		AutoRemove:  true,
		Runtime:     p.Plan.Runtime(),
		ExtraHosts:  hosts,
	}
	runSpec = hardenedAgent(runSpec, p)

	eventsFile, err := os.Create(filepath.Join(rd.receiptsDir, "events.ndjson")) //nolint:gosec // our receipts dir
	if err != nil {
		return agentOutcome{err: Classify(ExitCLI, fmt.Errorf("create events receipt: %w", err))}
	}
	defer func() { _ = eventsFile.Close() }()

	// The stream is consumed as it arrives so --max-turns can stop a
	// looping agent instead of billing it to completion (§R8 exit 16).
	pr, pw := io.Pipe()
	collector := &eventCollector{
		adapter:    p.Adapter,
		events:     eventsFile,
		tee:        p.Events,
		onUsage:    p.OnUsage,
		redact:     rd.redact,
		maxTurns:   p.Budgets.MaxTurns,
		maxCostUSD: p.Budgets.MaxCostUSD,
		overBudget: func() {
			cancelCause(errBudget)
		},
		overCost: func() {
			cancelCause(errCostBudget)
		},
	}
	done := consumeGuarded(collector, pr, cancelCause)

	var stdin io.Reader
	if runSpec.Interactive {
		stdin = bytes.NewReader(task)
	}
	stderrPath := filepath.Join(rd.receiptsDir, "agent-stderr.log")
	stderrFile, err := os.Create(stderrPath) //nolint:gosec // our receipts dir
	if err != nil {
		return agentOutcome{err: Classify(ExitCLI, fmt.Errorf("create stderr receipt: %w", err))}
	}
	defer func() { _ = stderrFile.Close() }()

	exit, runErr := d.RunForeground(agentCtx, runSpec, stdin, pw, stderrFile)
	_ = pw.Close()
	<-done
	// The harness wrote its stderr straight into the receipt; mask it now,
	// before the tail below quotes it.
	_ = stderrFile.Close()
	rd.redact.File(stderrPath)
	// The kill raced the docker client; make the container's absence a
	// fact rather than a hope. Not-found is the normal case (--rm).
	_ = d.RemoveForce(ctx, name)

	out := agentOutcome{lastText: collector.lastText, usage: collector.usage()}
	switch {
	// First, because a panic that stopped the reader is the reason anything
	// else downstream looks wrong — the harness blocked on a pipe nobody was
	// draining, and so was killed.
	case collector.panicErr != nil:
		out.err = Classify(ExitCLI, collector.panicErr)
	case context.Cause(agentCtx) == errBudget:
		out.err = Classify(ExitBudget, fmt.Errorf("turn budget exceeded (%d tool calls)", p.Budgets.MaxTurns))
	case context.Cause(agentCtx) == errCostBudget:
		out.err = Classify(ExitBudget, fmt.Errorf("cost budget exceeded (harness reported $%.4f, budget $%.2f)", collector.costUSD, p.Budgets.MaxCostUSD))
	case errors.Is(context.Cause(agentCtx), context.DeadlineExceeded):
		out.err = Classify(ExitTimeout, fmt.Errorf("run timed out after %ds", p.Budgets.TimeoutSeconds))
	case runErr != nil:
		out.err = Classify(ExitGateway, fmt.Errorf("agent container failed to run: %w", runErr))
	case exit != 0:
		detail := stderrTail(stderrPath)
		if collector.harnessErrReported != "" {
			detail = "\nharness error: " + collector.harnessErrReported + detail
		}
		out.err = Classify(ExitHarness, fmt.Errorf("agent exited %d%s", exit, detail))
	}
	return out
}

var (
	errBudget     = errors.New("turn budget exceeded")
	errCostBudget = errors.New("cost budget exceeded")
)

// outputSchema loads and compiles the package's output.schema.json for a
// json-kind plan (DR-8 closed, agentry-cli#11). The bytes are re-hashed
// against the plan: the plan is what was accepted at install, the
// workspace is what the run reads, and they must agree.
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

// finalResult applies the §R2 output convention: json-kind results must
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

// JSONResult is the §R2 output convention for a json-kind result, as the
// run itself applies it: a host post-step that recovers an answer from a
// message the run refused holds it to exactly this (the Examiner's,
// agentry-cli#504).
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
	// maxCostUSD > 0 makes the harness's reported running cost a hard
	// stop (agentry-cli#7, DR-4). docker-agent's usage events carry cost as
	// a running total, so the collector keeps its high-water mark: a harness
	// that reported per-call costs would under-enforce, never over-kill.
	// Tokens are the opposite, and are summed: docker-agent's token_usage
	// carries that model request's own tokens (fresh plus cached input),
	// and claude and codex report once per autonomous run (agentry-cli#141;
	// usage_recorded_test.go pins this against the recorded streams).
	maxCostUSD float64
	overCost   func()

	turns         int
	inTok, outTok int64
	costUSD       float64
	// curText accumulates the streamed deltas of the assistant message in
	// progress; lastText is the complete text of the most recent message
	// (a streaming provider's last delta alone was the result before
	// agentry-cli#53). A tool call or a usage event ends a message.
	curText            strings.Builder
	lastText           string
	budgetTripped      bool
	costTripped        bool
	harnessErrReported string
	// panicErr is set when the consuming goroutine recovered a panic. It is
	// the run's error: a panic in the adapter's mapper or the tee writer
	// would otherwise take the process down, and teardown is a defer in
	// Run's own goroutine, which a dying goroutine never runs
	// (agentry-cli#161).
	panicErr error
	// totTok accumulates a harness's context-token reports, for the
	// harnesses that give a total without an input/output split.
	totTok int64
	// onUsage is fired with the running totals on every usage event, and
	// tee receives every raw line beside the receipt's own copy.
	onUsage func(receipt.Usage)
	tee     io.Writer
	// usageErr records a callback that panicked, so a driver metering the
	// run learns its meter is dead instead of reading silence as zero.
	usageErr error
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
// died (agentry-cli#161). So the panic is recovered, recorded as the
// collector's error, and the pipe is closed with it, which makes the harness's
// next write fail instead of blocking forever on a reader that is gone.
// onCancel ends the agent's context for the case where nothing is writing: a
// docker client blocked reading the attach stream never notices the closed
// pipe, and without it the run would sit there with its budget goroutine dead
// and the sandbox up.
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
		masked := append([]byte(c.redact.String(string(line))), '\n')
		_, _ = c.events.Write(masked)
		if c.tee != nil {
			// A driver reading this file is metering a live run, so the
			// line goes out as it arrives; a write it cannot take is its
			// problem, never the run's.
			_, _ = c.tee.Write(masked)
		}
		for _, ev := range c.adapter.Events(line) {
			switch ev.Kind {
			case adapter.EventToolCall:
				c.curText.Reset() // text before a tool call is its own message
				c.turns++
				if c.maxTurns > 0 && c.turns > c.maxTurns && !c.budgetTripped {
					c.budgetTripped = true
					c.overBudget()
				}
			case adapter.EventAssistantText:
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
				if ev.CostUSD > c.costUSD {
					c.costUSD = ev.CostUSD
				}
				if c.maxCostUSD > 0 && c.costUSD > c.maxCostUSD && !c.costTripped && c.overCost != nil {
					c.costTripped = true
					c.overCost()
				}
				c.notifyUsage()
			case adapter.EventError:
				c.harnessErrReported = ev.Text
			}
		}
	}
}

// notifyUsage hands the running totals to the driver's callback without
// letting the driver end the run. This goroutine enforces the turn and cost
// budgets and its end releases teardown, so a panicking callback used to
// mean containers left up; one that panics is reported through the run's
// own error channel and then switched off, because a callback that failed
// once will fail on every event.
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

func (c *eventCollector) usage() receipt.Usage {
	return receipt.Usage{
		InputTokens:  c.inTok,
		OutputTokens: c.outTok,
		TotalTokens:  c.totTok,
		CostUSD:      c.costUSD,
		Turns:        c.turns,
	}
}

// sandboxHosts pins the run network's two aliases — the gateway and the
// llm-proxy — into the /etc/hosts of a run container under the plan's
// runtime, the agent and the gateway's health probe (DR-23,
// agentry-cli#221): a sandboxed runtime's own network stack does not reach
// docker's embedded DNS at 127.0.0.11, and those two names are the whole
// world of both. Without a runtime nothing is pinned and the run is the
// run it was. Both containers are up before either starts (startInfra).
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
