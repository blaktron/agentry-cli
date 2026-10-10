package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/harnessprobe"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/procx"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// Host-run harnesses (plan runner-m-r4-adapters §5, decision D2): the
// adapter's harness is a process of this user from the person's own
// installation, not a container. The run's gateway and proxy are published
// on 127.0.0.1 for it (hostPublish), the harness files are rendered with
// those addresses, and the process gets a clean environment: the plan's
// variables, the two addresses, and the host variables the adapter names.
// Budgets, receipts and exit classification are the container path's.

// hostPublish publishes a container port on 127.0.0.1 with an ephemeral
// host port when the plan's harness runs on the host; nil otherwise, so a
// container harness keeps the run entirely on the internal network.
func hostPublish(p Params, containerPort int) []dockerx.PortPublish {
	if p.Plan.Harness.Host == nil {
		return nil
	}
	return []dockerx.PortPublish{{HostIP: "127.0.0.1", ContainerPort: containerPort}}
}

// waitPublishedPort reads the host port a run container's published port
// landed on, retrying until the mapping exists.
//
// The run network is internal, which has no NAT, so docker drops the -p
// binding at creation and only programs it when the container is
// afterwards connected to the bridge — and that lands a moment after
// `docker run -d` and `docker network connect` have both returned. Reading
// the port once therefore fails intermittently (it failed on the gateway
// back when the gateway published its own port, and, by luck of a faster
// image, not on the proxy). Nothing here is worth a fixed sleep: poll
// until the mapping is there.
func waitPublishedPort(ctx context.Context, d dockerx.Client, name string, containerPort int) (int, error) {
	deadline := time.Now().Add(publishedPortTimeout)
	var err error
	for {
		var port int
		if port, err = d.Port(ctx, name, containerPort); err == nil {
			return port, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return 0, fmt.Errorf("%s never published port %d on 127.0.0.1 (%ds): %w",
				name, containerPort, int(publishedPortTimeout/time.Second), err)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(publishedPortPoll):
		}
	}
}

// publishedPortTimeout bounds that wait; publishedPortPoll is how often it
// asks. Generous: on a loaded machine a container can take seconds to run.
const (
	publishedPortTimeout = 30 * time.Second
	publishedPortPoll    = 100 * time.Millisecond
)

// resolveHostCommand finds the harness executable the way the probe does
// (harnessprobe.Find: the policy's harness.<adapter>.path when set, else
// PATH, else the adapter's well-known install locations), so a run finds
// exactly what doctor reported and what the desktop's wizard showed. A
// plan whose adapter this build does not register as a host harness (a
// test adapter; a plan from another build) has no well-known locations
// to offer, so Find is given a stand-in that names the plan's command
// and nothing else — one resolution, one wording. Not installed is a
// not-runnable refusal (exit 12) with the remedy in the message.
func resolveHostCommand(pol *policy.Policy, adapterName, command string) (string, error) {
	path, _, err := harnessprobe.FindExecutable(pol, adapterName, command)
	if err != nil {
		return "", Classify(ExitCompile, fmt.Errorf("harness %s: %w", adapterName, err))
	}
	return path, nil
}

// ancestorFile walks from dir up to the root and returns the first of
// names that exists in any directory on the way (dir itself included),
// "" when none does. Names are relative and may have a directory part.
func ancestorFile(dir string, names []string) (string, error) {
	if len(names) == 0 {
		return "", nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("harness dir: %w", err)
	}
	for d := abs; ; d = filepath.Dir(d) {
		for _, name := range names {
			p := filepath.Join(d, filepath.FromSlash(name))
			if _, err := os.Lstat(p); err == nil {
				return p, nil
			}
		}
		if filepath.Dir(d) == d {
			return "", nil
		}
	}
}

// hostStart is what startHostProcess reports: the executable (and version)
// for the receipts, and the host variables the process was allowed to
// inherit.
type hostStart struct {
	info      string
	inherited []string
}

// hostOS is the host's OS for the harness's environment, a variable so a
// test can take Windows' path without also moving the gateway's spelling
// of host paths (gatewayOS), which other tests hold on the real OS.
var hostOS = runtime.GOOS

// windowsHostEnv are what a Windows program needs to find its profile, its
// npm shims (%APPDATA%\npm), a temporary directory, the extensions it may
// run and its shell (agentry-cli#423). Still an allowlist: nothing else in
// the runner's environment reaches the harness (RUNNER-PLAN §R6).
var windowsHostEnv = []string{"USERPROFILE", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP", "PATHEXT", "ComSpec", "SystemRoot"}

// hostInherit is the plan's InheritEnv, with windowsHostEnv added on
// Windows. The plan itself stays the same on every host.
func hostInherit(names []string, goos string) []string {
	out := append([]string{}, names...)
	if goos != "windows" {
		return out
	}
	for _, n := range windowsHostEnv {
		if !slices.ContainsFunc(out, func(o string) bool { return sameEnvName(o, n, goos) }) {
			out = append(out, n)
		}
	}
	return out
}

// sameEnvName compares variable names as the OS does: Windows ignores case.
func sameEnvName(a, b, goos string) bool {
	if goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// hostEnvLine is the receipt's word on what a host-run harness inherited.
func hostEnvLine(inherited []string) string {
	return "host process given only these of the runner's environment variables, where set, unchanged: " + strings.Join(inherited, ", ") +
		"; everything else it has is set by the plan and the run (the run token, the model key the policy names, the gateway and proxy addresses)"
}

// startHostProcess starts the harness process with stdin/stdout/stderr
// wired as given and returns a wait function (exit code, or a run error),
// plus the executable and version line for the receipts.
func startHostProcess(ctx context.Context, p Params, rd *runDirs, rep *strings.Replacer, token string,
	stdin io.Reader, stdout, stderr io.Writer,
) (func() (int, error), hostStart, error) {
	hs := p.Plan.Harness.Host
	path, err := resolveHostCommand(p.Policy, p.Plan.Harness.Adapter, hs.Command)
	if err != nil {
		return nil, hostStart{}, err
	}
	envMap, err := resolveEnv(hs.Env, rep, token)
	if err != nil {
		return nil, hostStart{}, err
	}
	// The model sign-in's access token as it is now (the restart token
	// route): read from the run's own token file, which the renewer keeps
	// current, so a process started again starts on the renewed token.
	for _, v := range hs.Env {
		if !v.FromModelSignIn {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(rd.modelSignInDir(), spec.ModelSignInTokenFile)) //nolint:gosec // the run's own folder
		if rerr != nil {
			return nil, hostStart{}, Classify(ExitCLI, fmt.Errorf("read the model sign-in's token for %s: %w", v.Name, rerr))
		}
		envMap[v.Name] = string(b)
	}
	envMap[spec.HostGatewayURLEnv] = rd.hostGatewayURL
	envMap[spec.HostProxyURLEnv] = rd.hostProxyURL
	inherited := hostInherit(hs.InheritEnv, hostOS)
	for _, name := range inherited {
		if slices.ContainsFunc(slices.Collect(maps.Keys(envMap)), func(k string) bool { return sameEnvName(k, name, hostOS) }) {
			continue
		}
		if v, ok := os.LookupEnv(name); ok {
			envMap[name] = v
		}
	}
	env := envPairs(envMap)
	dir := filepath.Join(rd.cfg, "harness")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, hostStart{}, Classify(ExitCLI, fmt.Errorf("harness dir: %w", err))
	}
	if found, err := ancestorFile(dir, hs.RefuseAncestorFiles); err != nil {
		return nil, hostStart{}, Classify(ExitCLI, err)
	} else if found != "" {
		return nil, hostStart{}, Classify(ExitPolicy, fmt.Errorf("harness %s: %s would enter the run as context — the harness reads it from every ancestor of its working directory, and this run has no --bare to skip it; move it, or use an API key (agentry-cli#171)",
			p.Plan.Harness.Adapter, found))
	}
	info := path
	if len(hs.VersionArgv) > 0 {
		vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		vcmd := exec.CommandContext(vctx, path, hs.VersionArgv...)
		vcmd.Env, vcmd.Dir = env, dir
		// As the probe: once the process has exited or been killed, a
		// grandchild left holding stdout may delay the read this long,
		// then it is abandoned.
		vcmd.WaitDelay = 2 * time.Second
		procx.NoWindow(vcmd)
		// stdout only: a harness that warns on stderr at startup (codex
		// does, about its config home) would otherwise have the warning
		// recorded as its version.
		out, _ := vcmd.Output()
		cancel()
		if line := firstLine(out); line != "" {
			info += " (" + line + ")"
		}
	}
	cmd := exec.CommandContext(ctx, path, substituteAll(hs.Argv, rep)...)
	cmd.Env, cmd.Dir = env, dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	// configProcess replaces SysProcAttr (the process group on unix), so
	// the window rule, which keeps what it finds, goes after it. Ten seconds
	// for the harness and its children to answer SIGTERM before the whole
	// group is SIGKILLed (agentry-cli#161).
	waited := configProcess(cmd, 10*time.Second)
	procx.NoWindow(cmd)
	if err := cmd.Start(); err != nil {
		waited()
		return nil, hostStart{info: info, inherited: inherited}, Classify(ExitHarness, fmt.Errorf("start harness %s: %w", hs.Command, err))
	}
	wait := func() (int, error) {
		// The leader's pid is the process group's id, and once it is reaped
		// the kernel may hand that pid to something else: call off any pending
		// group escalation before that can happen (agentry-cli#161).
		defer waited()
		err := cmd.Wait()
		if err == nil {
			return 0, nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return wait, hostStart{info: info, inherited: inherited}, nil
}

// runHostHarness is runAgent for a host-run harness: the same event
// collection, budgets and classification, with the process in place of
// the container.
func runHostHarness(ctx context.Context, p Params, rd *runDirs, token string, rep *strings.Replacer, task []byte, tools []string) agentOutcome {
	agentCtx := ctx
	var cancelTimeout context.CancelFunc
	if p.Budgets.TimeoutSeconds > 0 {
		agentCtx, cancelTimeout = context.WithTimeout(agentCtx, time.Duration(p.Budgets.TimeoutSeconds)*time.Second)
		defer cancelTimeout()
	}
	agentCtx, cancelCause := context.WithCancelCause(agentCtx)
	defer cancelCause(nil)
	eventsFile, err := os.Create(filepath.Join(rd.receiptsDir, "events.ndjson")) //nolint:gosec // our receipts dir
	if err != nil {
		return agentOutcome{err: Classify(ExitCLI, fmt.Errorf("create events receipt: %w", err))}
	}
	defer func() { _ = eventsFile.Close() }()
	pr, pw := io.Pipe()
	collector := &eventCollector{
		adapter:    p.Adapter,
		events:     eventsFile,
		tee:        p.Events,
		onUsage:    p.OnUsage,
		redact:     rd.redact,
		maxTurns:   p.Budgets.MaxTurns,
		maxCostUSD: p.Budgets.MaxCostUSD,
		overBudget: func() { cancelCause(errBudget) },
		overCost:   func() { cancelCause(errCostBudget) },
		// A run paid by a sign-in ends on its plan's limit, as in a
		// container (I9).
		signIn:         p.Plan.Harness.ModelSignIn != nil,
		onPlanLimit:    func() { cancelCause(errPlanLimit) },
		onOrgPolicy:    func() { cancelCause(errOrgPolicy) },
		onOutOfCredits: func() { cancelCause(errOutOfCredits) },
	}
	stderrPath := filepath.Join(rd.receiptsDir, "agent-stderr.log")
	stderrFile, err := os.Create(stderrPath) //nolint:gosec // our receipts dir
	if err != nil {
		_ = pw.Close()
		return agentOutcome{err: Classify(ExitCLI, fmt.Errorf("create stderr receipt: %w", err))}
	}
	defer func() { _ = stderrFile.Close() }()
	var exit int
	var runErr, turnErr error
	var info string
	var inherited []string
	if p.Plan.Harness.OneTurn != nil {
		// The task is the one turn of the harness's session (oneturn.go).
		_ = pw.Close()
		stdinR, stdinW := io.Pipe()
		stdoutR, stdoutW := io.Pipe()
		wait, started, err := startHostProcess(agentCtx, p, rd, rep, token, stdinR, stdoutW, stderrFile)
		info, inherited = started.info, started.inherited
		if err != nil {
			return agentOutcome{err: err, hostInfo: info, hostEnv: inherited}
		}
		ran := make(chan struct{})
		go func() {
			defer close(ran)
			exit, runErr = wait()
			_ = stdoutW.Close()
			_ = stdinR.Close()
		}()
		turnErr = oneTurn(agentCtx, p, rwPipe{Reader: stdoutR, WriteCloser: stdinW}, task, sessionOptions(p, rd, token, tools), collector, cancelCause)
		<-ran
	} else {
		done := consumeGuarded(collector, pr, cancelCause)
		var stdin io.Reader
		if p.Plan.Harness.Host.StdinTask {
			stdin = bytes.NewReader(task)
		}
		wait, started, err := startHostProcess(agentCtx, p, rd, rep, token, stdin, pw, stderrFile)
		info, inherited = started.info, started.inherited
		if err != nil {
			_ = pw.Close()
			<-done
			return agentOutcome{err: err, hostInfo: info, hostEnv: inherited}
		}
		exit, runErr = wait()
		_ = pw.Close()
		<-done
	}
	// The harness wrote its stderr straight into the receipt; mask it now,
	// before the tail below quotes it.
	_ = stderrFile.Close()
	rd.redact.File(stderrPath)
	var refused *adapter.OrgPolicyRefusal
	out := agentOutcome{lastText: collector.lastText, answerInReasoning: collector.reasoningLast, usage: collector.usage(), hostInfo: info, hostEnv: inherited}
	switch {
	// First, as in the container path: a panic that stopped the reader is the
	// reason anything downstream looks wrong (agentry-cli#161).
	case collector.panicErr != nil:
		out.err = Classify(ExitCLI, collector.panicErr)
	case collector.planLimit != nil:
		out.err = planLimitError(collector.planLimit.Text, collector.planLimit.ResetsAt)
	case collector.orgPolicy != nil:
		out.err = orgPolicyError(collector.orgPolicy)
	case collector.outOfCredits != nil:
		out.err = outOfCreditsError(collector.outOfCredits.Text)
	case errors.As(turnErr, &refused):
		out.err = orgPolicyError(&adapter.Event{Policy: refused.Policy, Text: refused.Text})
	case context.Cause(agentCtx) == errBudget:
		out.err = Classify(ExitBudget, fmt.Errorf("turn budget exceeded (%d tool calls)", p.Budgets.MaxTurns))
	case context.Cause(agentCtx) == errCostBudget:
		out.err = Classify(ExitBudget, fmt.Errorf("cost budget exceeded (harness reported $%.4f, budget $%.2f)", collector.costUSD, p.Budgets.MaxCostUSD))
	case errors.Is(context.Cause(agentCtx), context.DeadlineExceeded):
		out.err = Classify(ExitTimeout, fmt.Errorf("run timed out after %ds", p.Budgets.TimeoutSeconds))
	case turnErr != nil:
		out.err = Classify(ExitHarness, turnFailure(turnErr, collector.harnessErrReported, stderrPath))
	case runErr != nil:
		out.err = Classify(ExitHarness, fmt.Errorf("harness process failed: %w", runErr))
	case exit != 0:
		detail := stderrTail(stderrPath)
		if collector.harnessErrReported != "" {
			detail = "\nharness error: " + collector.harnessErrReported + detail
		}
		out.err = Classify(ExitHarness, fmt.Errorf("harness exited %d%s", exit, detail))
	}
	return out
}

func envPairs(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
