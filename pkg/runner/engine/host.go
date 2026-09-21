package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
// plan whose adapter this build does not know as a host harness keeps
// the plain rule: the policy path, else a PATH lookup of the plan's
// command name. Not installed is a not-runnable refusal (exit 12) with
// the remedy in the message.
func resolveHostCommand(pol *policy.Policy, adapterName, command string) (string, error) {
	if ad, err := adapter.Get(adapterName); err == nil {
		if _, ok := ad.(adapter.HostHarness); ok {
			path, _, ferr := harnessprobe.Find(pol, adapterName)
			if ferr != nil {
				return "", Classify(ExitCompile, fmt.Errorf("harness %s: %w", adapterName, ferr))
			}
			return path, nil
		}
	}
	if pol != nil {
		if hp, ok := pol.Harness[adapterName]; ok && hp.Path != "" {
			if _, err := os.Stat(hp.Path); err != nil {
				return "", Classify(ExitCompile, fmt.Errorf("harness %s: policy harness.%s.path %q: %w", adapterName, adapterName, hp.Path, err))
			}
			return hp.Path, nil
		}
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return "", Classify(ExitCompile, fmt.Errorf("harness %s: %q is not installed on this machine (not on PATH); install it, or set harness.%s.path in the runner policy", adapterName, command, adapterName))
	}
	return path, nil
}

// startHostProcess starts the harness process with stdin/stdout/stderr
// wired as given and returns a wait function (exit code, or a run error),
// plus the executable and version line for the receipts.
func startHostProcess(ctx context.Context, p Params, rd *runDirs, rep *strings.Replacer, token string,
	stdin io.Reader, stdout, stderr io.Writer,
) (func() (int, error), string, error) {
	hs := p.Plan.Harness.Host
	path, err := resolveHostCommand(p.Policy, p.Plan.Harness.Adapter, hs.Command)
	if err != nil {
		return nil, "", err
	}
	envMap, err := resolveEnv(hs.Env, rep, token)
	if err != nil {
		return nil, "", err
	}
	envMap[spec.HostGatewayURLEnv] = rd.hostGatewayURL
	envMap[spec.HostProxyURLEnv] = rd.hostProxyURL
	for _, name := range hs.InheritEnv {
		if _, set := envMap[name]; set {
			continue
		}
		if v, ok := os.LookupEnv(name); ok {
			envMap[name] = v
		}
	}
	env := envPairs(envMap)
	dir := filepath.Join(rd.cfg, "harness")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", Classify(ExitCLI, fmt.Errorf("harness dir: %w", err))
	}
	info := path
	if len(hs.VersionArgv) > 0 {
		vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		vcmd := exec.CommandContext(vctx, path, hs.VersionArgv...)
		vcmd.Env, vcmd.Dir = env, dir
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
	configProcess(cmd)
	procx.NoWindow(cmd)
	if err := cmd.Start(); err != nil {
		return nil, info, Classify(ExitHarness, fmt.Errorf("start harness %s: %w", hs.Command, err))
	}
	wait := func() (int, error) {
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
	return wait, info, nil
}

// runHostHarness is runAgent for a host-run harness: the same event
// collection, budgets and classification, with the process in place of
// the container.
func runHostHarness(ctx context.Context, p Params, rd *runDirs, token string, rep *strings.Replacer, task []byte) agentOutcome {
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
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		collector.consume(pr)
	}()
	var stdin io.Reader
	if p.Plan.Harness.Host.StdinTask {
		stdin = bytes.NewReader(task)
	}
	stderrPath := filepath.Join(rd.receiptsDir, "agent-stderr.log")
	stderrFile, err := os.Create(stderrPath) //nolint:gosec // our receipts dir
	if err != nil {
		_ = pw.Close()
		<-done
		return agentOutcome{err: Classify(ExitCLI, fmt.Errorf("create stderr receipt: %w", err))}
	}
	defer func() { _ = stderrFile.Close() }()
	wait, info, err := startHostProcess(agentCtx, p, rd, rep, token, stdin, pw, stderrFile)
	if err != nil {
		_ = pw.Close()
		<-done
		return agentOutcome{err: err, hostInfo: info}
	}
	exit, runErr := wait()
	_ = pw.Close()
	<-done
	// The harness wrote its stderr straight into the receipt; mask it now,
	// before the tail below quotes it.
	_ = stderrFile.Close()
	rd.redact.File(stderrPath)
	out := agentOutcome{lastText: collector.lastText, usage: collector.usage(), hostInfo: info}
	switch {
	case context.Cause(agentCtx) == errBudget:
		out.err = Classify(ExitBudget, fmt.Errorf("turn budget exceeded (%d tool calls)", p.Budgets.MaxTurns))
	case context.Cause(agentCtx) == errCostBudget:
		out.err = Classify(ExitBudget, fmt.Errorf("cost budget exceeded (harness reported $%.4f, budget $%.2f)", collector.costUSD, p.Budgets.MaxCostUSD))
	case errors.Is(context.Cause(agentCtx), context.DeadlineExceeded):
		out.err = Classify(ExitTimeout, fmt.Errorf("run timed out after %ds", p.Budgets.TimeoutSeconds))
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
