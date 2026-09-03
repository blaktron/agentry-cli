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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// Exit codes (RUNNER-PLAN §R8). The distinguishable failure classes are
// the runner's composability contract: a cron job or pipeline branches on
// these, so each class has exactly one meaning.
const (
	ExitOK           = 0
	ExitCLI          = 1
	ExitVerify       = 10
	ExitPolicy       = 11
	ExitCompile      = 12
	ExitGateway      = 13
	ExitHarness      = 14
	ExitTimeout      = 15
	ExitBudget       = 16
	ExitOutputSchema = 17
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
	// RunRoot is where runs/<id>/ ephemera live (the install dir).
	RunRoot string
	// OutDir is the mounted output folder (RQ3), already absolute.
	OutDir string
	// Task is the task input (may be nil for an empty task).
	Task io.Reader
	// InputPaths are operator-provided files/directories copied into the
	// run's read-only inputs channel (RUNNER-PLAN-2 §P4).
	InputPaths []string
	// Adapter interprets the harness event stream.
	Adapter adapter.Adapter
	// Policy supplies the secret source.
	Policy *policy.Policy
	// SecretOverrides resolve declared secrets ahead of the policy source
	// — the BriefAgent's MCP profile rides in here (P2 §P2); the engine
	// itself stays profile-agnostic.
	SecretOverrides map[string]string
	// ServersDir is the signature's installed MCP-server store
	// (RUNNER-PLAN-3), substituted for the PHServers placeholder.
	// Required when the plan declares MCP servers; unused otherwise.
	ServersDir string
	// Budgets are the effective hard stops (plan defaults merged with
	// any tighter run flags by the caller).
	Budgets spec.Budgets
	// Attestation is the verify-gate outcome for the envelope.
	Attestation receipt.Attestation
	// KeepRunDir preserves runs/<id>/ for debugging (secrets.env is
	// still always deleted).
	KeepRunDir bool
	// RunID names the run; "" mints one. The CLI passes the id it used
	// for the default output folder so the two always agree.
	RunID string
	// Stderr receives progress and diagnostics.
	Stderr io.Writer
}

// Run executes the plan. The returned envelope is always usable — on
// failure it carries status "error", the exit class, and whatever was
// known — and the returned error (nil on success) is already classified.
func Run(ctx context.Context, d dockerx.Client, p Params) (*receipt.Envelope, error) {
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
		CapabilitiesEnforced: p.Plan.Enforcement,
		Harness: receipt.Harness{
			Adapter: p.Plan.Harness.Adapter,
			Image:   p.Plan.Harness.Agent.Image,
			Model:   p.Plan.Harness.Model,
		},
		Result: receipt.TextResult(""),
		OutDir: p.OutDir,
	}
	planHash, err := p.Plan.Hash()
	if err != nil {
		return fail(env, Classify(ExitCLI, err))
	}
	receiptsDir := filepath.Join(p.OutDir, "receipts")
	env.Receipts = receipt.Receipts{Dir: receiptsDir, RunPlanSHA256: planHash}

	// Everything below must tear down whatever it built, success or not
	// (§R11 invariant 6): td runs in reverse order via defer, then sweeps
	// anything still carrying this run's label.
	td := &teardown{d: d, stderr: p.Stderr, runID: runID}
	defer td.run()

	rd, err := prepareRunDirs(p, runID, td)
	if err != nil {
		return fail(env, err)
	}
	if err := writeSecretsFile(p, rd.secretsPath); err != nil {
		return fail(env, err)
	}
	task, err := readTask(p.Task, rd.receiptsDir)
	if err != nil {
		return fail(env, err)
	}

	token, err := newToken()
	if err != nil {
		return fail(env, Classify(ExitCLI, err))
	}
	rep := rd.replacer()

	if err := startInfra(ctx, d, p, rd, td, runID, token, rep); err != nil {
		return fail(env, err)
	}

	outcome := runAgent(ctx, d, p, rd, runID, token, rep, task)
	env.Usage = outcome.usage
	env.Usage.WallMS = time.Since(start).Milliseconds()

	// The gateway's own log (tool-call log included, --log-calls) is
	// part of the audit trail; collect before teardown kills it.
	if logs, lerr := d.Logs(ctx, containerName(runID, "gateway"), 2000); lerr == nil && logs != "" {
		_ = os.WriteFile(filepath.Join(rd.receiptsDir, "gateway.log"), []byte(logs), 0o644)
	}
	writeMeta(rd.receiptsDir, runID, p, start)

	if outcome.err != nil {
		env.Result = receipt.TextResult(outcome.lastText)
		return fail(env, outcome.err)
	}

	schema, serr := outputSchema(p)
	if serr != nil {
		return nil, serr
	}
	result, rerr := finalResult(p.Plan.Harness.ResultKind, outcome.lastText, schema)
	if rerr != nil {
		env.Result = receipt.TextResult(outcome.lastText)
		return fail(env, rerr)
	}
	env.Result = result
	env.Status = "ok"
	env.ExitCode = ExitOK
	if err := env.Write(p.OutDir); err != nil {
		_, _ = fmt.Fprintf(p.Stderr, "warning: %v\n", err)
	}
	return env, nil
}

// fail stamps the envelope with the classified failure, writes it to the
// output folder (a failed run still leaves its record, RQ3), and returns.
func fail(env *receipt.Envelope, err error) (*receipt.Envelope, error) {
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
}

// startInfra creates the run network and starts proxy + gateway, then
// health-probes the gateway from inside the network (§R6 execute order).
// Every failure here is the §R8 gateway/environment class.
func startInfra(ctx context.Context, d dockerx.Client, p Params, rd *runDirs,
	td *teardown, runID, token string, rep *strings.Replacer,
) error {
	labels := runLabels(runID)
	netName := networkName(runID)
	if err := d.NetworkCreate(ctx, netName, true, labels); err != nil {
		return Classify(ExitGateway, fmt.Errorf("create run network: %w", err))
	}
	td.add(func(c context.Context) { _ = d.NetworkRemove(c, netName) })

	// LLM egress proxy: the agent container's only route out (§R4).
	proxyName := containerName(runID, "proxy")
	proxySpec := dockerx.RunSpec{
		Name:  proxyName,
		Image: p.Plan.Proxy.Image,
		Argv: []string{"proxy",
			"--allow", strings.Join(p.Plan.Proxy.Allow, ","),
			"--addr", fmt.Sprintf(":%d", spec.ProxyPort)},
		Network: netName,
		Alias:   spec.ProxyAlias,
		Labels:  labels,
	}
	if err := d.RunDetached(ctx, proxySpec); err != nil {
		return Classify(ExitGateway, fmt.Errorf("start llm proxy: %w", err))
	}
	td.add(func(c context.Context) { _ = d.RemoveForce(c, proxyName) })
	// The proxy needs a route to the model endpoint; the run network is
	// internal by design, so it joins the default bridge as its second
	// leg. The agent container never does (§R11 invariant 2).
	if err := d.NetworkConnect(ctx, "bridge", proxyName); err != nil {
		return Classify(ExitGateway, fmt.Errorf("connect proxy to bridge: %w", err))
	}

	gwName := containerName(runID, "gateway")
	gwEnv, err := resolveEnv(p.Plan.Gateway.Env, rep, token)
	if err != nil {
		return err
	}
	gwSpec := dockerx.RunSpec{
		Name:    gwName,
		Image:   p.Plan.Gateway.Image,
		Argv:    substituteAll(p.Plan.Gateway.Args, rep),
		Env:     gwEnv,
		Mounts:  resolveMounts(p.Plan.Gateway.Mounts, rep),
		Network: netName,
		Alias:   spec.GatewayAlias,
		Labels:  labels,
	}
	if err := d.RunDetached(ctx, gwSpec); err != nil {
		return Classify(ExitGateway, fmt.Errorf("start mcp gateway: %w", err))
	}
	// Graceful stop first: the gateway reaps its tool-server containers
	// on SIGTERM; rm -f would orphan them.
	td.add(func(c context.Context) {
		_ = d.Stop(c, gwName, 10)
		_ = d.RemoveForce(c, gwName)
	})
	if err := d.NetworkConnect(ctx, "bridge", gwName); err != nil {
		return Classify(ExitGateway, fmt.Errorf("connect gateway to bridge: %w", err))
	}

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
	code, perr := d.RunForeground(ctx, probeSpec, nil, io.Discard, p.Stderr)
	if perr != nil || code != 0 {
		diag := gatewayDiagnostics(ctx, d, gwName)
		if perr == nil {
			perr = fmt.Errorf("gateway health probe failed (exit %d)", code)
		}
		return Classify(ExitGateway, fmt.Errorf("%w%s", perr, diag))
	}
	return nil
}

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
	}

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
		maxTurns:   p.Budgets.MaxTurns,
		maxCostUSD: p.Budgets.MaxCostUSD,
		overBudget: func() {
			cancelCause(errBudget)
		},
		overCost: func() {
			cancelCause(errCostBudget)
		},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		collector.consume(pr)
	}()

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
	// The kill raced the docker client; make the container's absence a
	// fact rather than a hope. Not-found is the normal case (--rm).
	_ = d.RemoveForce(ctx, name)

	out := agentOutcome{lastText: collector.lastText, usage: collector.usage()}
	switch {
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
	adapter    adapter.Adapter
	events     io.Writer
	maxTurns   int
	overBudget func()
	// maxCostUSD > 0 makes the harness's reported running cost a hard
	// stop (agentry-cli#7, DR-4). Usage events carry a running total on
	// docker-agent; the collector keeps the high-water mark, so a harness
	// that reported per-call costs would under-enforce, never over-kill.
	maxCostUSD float64
	overCost   func()

	turns              int
	inTok, outTok      int64
	costUSD            float64
	lastText           string
	budgetTripped      bool
	costTripped        bool
	harnessErrReported string
}

func (c *eventCollector) consume(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		_, _ = c.events.Write(append(append([]byte{}, line...), '\n'))
		for _, ev := range c.adapter.Events(line) {
			switch ev.Kind {
			case adapter.EventToolCall:
				c.turns++
				if c.maxTurns > 0 && c.turns > c.maxTurns && !c.budgetTripped {
					c.budgetTripped = true
					c.overBudget()
				}
			case adapter.EventAssistantText:
				if ev.Text != "" {
					c.lastText = ev.Text
				}
			case adapter.EventUsage:
				c.inTok += ev.InputTokens
				c.outTok += ev.OutputTokens
				if ev.CostUSD > c.costUSD {
					c.costUSD = ev.CostUSD
				}
				if c.maxCostUSD > 0 && c.costUSD > c.maxCostUSD && !c.costTripped && c.overCost != nil {
					c.costTripped = true
					c.overCost()
				}
			case adapter.EventError:
				c.harnessErrReported = ev.Text
			}
		}
	}
}

func (c *eventCollector) usage() receipt.Usage {
	return receipt.Usage{
		InputTokens:  c.inTok,
		OutputTokens: c.outTok,
		CostUSD:      c.costUSD,
		Turns:        c.turns,
	}
}
