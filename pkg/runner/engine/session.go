package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// StartSession does everything Run does up to and including the sandbox,
// then starts the harness's interactive server on the agent container's
// attached stdio and returns the session (DR-15, plan §6.3). The plan
// must be an interactive plan (Mode interactive, Harness.Session set):
// it is byte-identical to the autonomous plan except the harness argv and
// that block, so the sandbox — network, proxy, gateway, mounts, secrets —
// is exactly the autonomous one. The task text, if any, is the opening
// user turn.
func StartSession(ctx context.Context, d dockerx.Client, p Params) (session.Session, error) {
	if p.Plan == nil || p.Plan.Mode != "interactive" || p.Plan.Harness.Session == nil {
		return nil, Classify(ExitCompile, errors.New("not an interactive plan (compile with Mode interactive)"))
	}
	if p.Adapter == nil {
		return nil, Classify(ExitCLI, errors.New("no adapter"))
	}
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
		Status:               "error",
		RunID:                runID,
		Ref:                  p.Plan.Ref,
		Digest:               p.Plan.Digest,
		Attestation:          p.Attestation,
		CapabilitiesEnforced: enforcedAtStart(p),
		Harness: receipt.Harness{
			Adapter: p.Plan.Harness.Adapter,
			Image:   p.Plan.Harness.Agent.Image,
			Model:   p.Plan.Harness.Model,
			Mode:    p.Plan.Mode,
		},
		Policy: policyRecord(p),
		Result: receipt.TextResult(""),
		OutDir: p.OutDir,
	}
	planHash, err := p.Plan.Hash()
	if err != nil {
		return nil, Classify(ExitCLI, err)
	}
	receiptsDir := filepath.Join(p.OutDir, "receipts")
	env.Receipts = receipt.Receipts{Dir: receiptsDir, RunPlanSHA256: planHash}

	// What a run whose process died left behind goes first, before this
	// run adds its own (agentry-cli#161).
	SweepAbandoned(ctx, d, p.Stderr)
	td := &teardown{d: d, stderr: p.Stderr, runID: runID}
	s := &liveSession{
		d: d, p: p, env: env, td: td, start: start, runID: runID,
		events:   make(chan session.Event, 1024),
		done:     make(chan struct{}),
		pumpDone: make(chan struct{}),
		pumpSync: make(chan chan struct{}),
		budgets:  p.Plan.Harness.Session.Budgets,
	}
	fail := func(err error) (session.Session, error) {
		// Only End closes the events receipt; a start that fails never
		// reaches it, and on Windows an open receipt cannot be moved or
		// deleted until the process exits (agentry-cli#459). A harness
		// that already exited has written its session_end (awaitExit
		// waits for it); emit writes under s.mu and skips a nil file, so
		// a later write is dropped, never made to a closed file.
		s.mu.Lock()
		if s.eventsFile != nil {
			_ = s.eventsFile.Close()
			s.eventsFile = nil
		}
		s.mu.Unlock()
		td.run()
		_, cerr := fail(env, s.redact, err)
		_ = td.reportLeftFiles(env) // the session already failed; the report amends it
		return nil, cerr
	}

	// The web tools are an autonomous draft's; a session never runs them.
	if p.Plan.Capabilities.WebTools || p.Web != nil {
		return fail(Classify(ExitPolicy, errors.New("the web tools do not run in an interactive session")))
	}
	rd, err := prepareRunDirs(p, runID, td)
	if err != nil {
		return fail(err)
	}
	s.rd = rd
	// The session store: a per-run named volume, labelled like every run
	// object and removed with it (agentry-cli#90; DR-15 called the
	// world-writable host directory it replaces "for now"). A fresh
	// volume is root-owned wherever it is mounted and the harness runs
	// unprivileged, so the volume is given to the harness image's own
	// user first — as root, in that image, on no network, once.
	if err := d.VolumeCreate(ctx, rd.session, runLabels(runID)); err != nil {
		return fail(Classify(ExitCLI, fmt.Errorf("create session volume: %w", err)))
	}
	td.add(func(c context.Context) { _ = d.VolumeRemove(c, rd.session) })
	if owner := p.Plan.Harness.Session.StoreOwner; owner != "" {
		initName := containerName(runID, "session-init")
		td.add(func(c context.Context) { _ = d.RemoveForce(c, initName) })
		// The one-shot that hands the store to the harness user keeps
		// CHOWN, the one capability its chown needs, and nothing else.
		initSpec := hardened(dockerx.RunSpec{
			Name:       initName,
			Image:      p.Plan.Harness.Agent.Image,
			Entrypoint: "sh",
			Argv:       []string{"-c", `chown -- "$1" "$2"`, "sh", owner, spec.SessionMount},
			User:       "0:0",
			Network:    "none",
			Mounts:     []dockerx.Mount{{Source: rd.session, Target: spec.SessionMount}},
			Labels:     runLabels(runID),
			AutoRemove: true,
			Runtime:    p.Plan.Runtime(),
		}, p.limits())
		initSpec.CapAdd = []string{"CHOWN"}
		code, err := d.RunForeground(ctx, initSpec, nil, io.Discard, p.Stderr)
		if err != nil || code != 0 {
			if err == nil {
				err = fmt.Errorf("exit %d", code)
			}
			return fail(Classify(ExitCLI, fmt.Errorf("give the session store to the harness user %q: %w", owner, err)))
		}
	}
	secrets, err := secretValues(p)
	if err != nil {
		return fail(err)
	}
	if err := writeSecretsFile(p, rd.secretsPath); err != nil {
		return fail(err)
	}
	task, err := readTask(p.Task, rd.receiptsDir)
	if err != nil {
		return fail(err)
	}
	token, err := newToken()
	if err != nil {
		return fail(Classify(ExitCLI, err))
	}
	rep := rd.replacer()
	s.redact = rd.newRedactor(p, token, secrets)
	env.CapabilitiesEnforced = withRedactionNote(env.CapabilitiesEnforced, p, secrets)

	if err := startInfra(ctx, d, p, rd, td, runID, token, rep); err != nil {
		env.CapabilitiesEnforced["daemon"] = daemonLine(rd)
		return fail(withRefusals(err, rd))
	}
	// Harness files after the infrastructure, as in Run: a host-run harness
	// needs the published addresses (plan runner-m-r4-adapters §5).
	rep = rd.replacer()
	if err := renderPlanFiles(p.Plan, p.PlanDir, rd, nativePath, "harness"); err != nil {
		return fail(err)
	}
	if err := s.startHarness(ctx, token, rep); err != nil {
		return fail(err)
	}
	if !p.Intake.Empty() {
		if raw, err := json.Marshal(p.Intake.Receipt(spec.InputsMount)); err == nil {
			s.emit(session.Event{Kind: session.KindIntake, Raw: raw})
		}
	}
	go s.pump()
	go s.watch()
	if len(strings.TrimSpace(string(task))) > 0 {
		// The opening context, exactly as in autonomous: the first turn.
		go func() { _ = s.Send(context.Background(), string(task)) }()
	}
	return s, nil
}

// pumpEventHook runs before the pump handles each driver event; nil
// outside tests, which set it to make the pump lag the driver as a loaded
// machine does (agentry-cli#204).
var pumpEventHook func()

// liveSession is the engine's session.Session.
type liveSession struct {
	d      dockerx.Client
	p      Params
	env    *receipt.Envelope
	td     *teardown
	rd     *runDirs
	start  time.Time
	runID  string
	redact *redactor

	budgets spec.SessionBudgets

	drv        adapter.SessionDriver
	agentCtx   context.Context
	cancel     context.CancelCauseFunc
	stdinW     io.Closer
	exited     chan struct{} // closed when the harness container has exited
	exitCode   int
	eventsFile *os.File
	stderrPath string

	events   chan session.Event
	done     chan struct{} // closed once (ended)
	pumpDone chan struct{} // closed when pump has drained the driver's events
	// pumpSync asks the pump to record everything the driver has queued so
	// far, then close the channel it was sent (agentry-cli#204).
	pumpSync chan chan struct{}

	turnMu sync.Mutex // one prompt at a time
	mu     sync.Mutex // everything below
	ended  bool
	reason session.EndReason
	log    []session.Event
	// pendingNotices are input notices to prepend to the next user turn
	// because one was in flight when the input arrived.
	pendingNotices []string
	userTurns      int
	toolCalls      int
	inTok, outTok  int64
	totTok         int64
	costUSD        float64
	lastText       string
	harnessErr     string
	// panicErr is set when the pump or the watcher recovered a panic. It is
	// the session's error whatever reason it then ended with, because that
	// reason is a side effect of the recovery (agentry-cli#161).
	panicErr     error
	lastActivity time.Time
	endOnce      sync.Once
	// endMu serialises End: a second caller waits for the first and gets
	// its envelope. Without it two concurrent callers both passed the
	// envelope check and both closed events (agentry-cli#161; the
	// desktop's quit mid-run, agentry-desktop#125, is such a caller).
	endMu       sync.Mutex
	envelope    *receipt.Envelope
	envelopeErr error
}

// rwPipe joins the harness's stdout (read) and stdin (write) for the adapter.
type rwPipe struct {
	io.Reader
	io.WriteCloser
}

func (s *liveSession) startHarness(ctx context.Context, token string, rep *strings.Replacer) error {
	p := s.p
	agentEnv, err := resolveEnv(p.Plan.Harness.Agent.Env, rep, token)
	if err != nil {
		return err
	}
	mounts := resolveMounts(p.Plan.Harness.Agent.Mounts, rep)
	mounts = append(mounts, resolveMounts(p.Plan.Harness.Session.Mounts, rep)...)
	name := containerName(s.runID, "agent")
	hosts, err := sandboxHosts(ctx, s.d, p.Plan, s.runID)
	if err != nil {
		return Classify(ExitGateway, err)
	}
	runSpec := dockerx.RunSpec{
		Name:        name,
		Image:       p.Plan.Harness.Agent.Image,
		Argv:        substituteAll(p.Plan.Harness.Agent.Argv, rep),
		Env:         agentEnv,
		Mounts:      mounts,
		Network:     networkName(s.runID),
		Labels:      runLabels(s.runID),
		Interactive: true,
		AutoRemove:  true,
		Runtime:     p.Plan.Runtime(),
		ExtraHosts:  hosts,
	}
	runSpec = hardenedAgent(runSpec, p)
	s.td.add(func(c context.Context) { _ = s.d.RemoveForce(c, name) })

	eventsFile, err := os.Create(filepath.Join(s.rd.receiptsDir, "events.ndjson")) //nolint:gosec // our receipts dir
	if err != nil {
		return Classify(ExitCLI, fmt.Errorf("create events receipt: %w", err))
	}
	s.eventsFile = eventsFile
	s.stderrPath = filepath.Join(s.rd.receiptsDir, "agent-stderr.log")
	stderrFile, err := os.Create(s.stderrPath) //nolint:gosec // our receipts dir
	if err != nil {
		return Classify(ExitCLI, fmt.Errorf("create stderr receipt: %w", err))
	}

	agentCtx, cancel := context.WithCancelCause(ctx)
	if s.budgets.MaxWallSeconds > 0 {
		var cancelWall context.CancelFunc
		agentCtx, cancelWall = context.WithTimeout(agentCtx, time.Duration(s.budgets.MaxWallSeconds)*time.Second)
		s.td.add(func(context.Context) { cancelWall() })
	}
	s.agentCtx, s.cancel = agentCtx, cancel

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	s.stdinW = stdinW
	s.exited = make(chan struct{})
	run := func() (int, error) { return s.d.RunForeground(agentCtx, runSpec, stdinR, stdoutW, stderrFile) }
	if p.Plan.Harness.Host != nil {
		wait, started, herr := startHostProcess(agentCtx, p, s.rd, rep, token, stdinR, stdoutW, stderrFile)
		info := started.info
		if herr != nil {
			// No harness process, so no goroutine to close the stderr
			// receipt (agentry-cli#459).
			_ = stderrFile.Close()
			cancel(herr)
			return herr
		}
		s.env.Harness.Host = info
		if len(started.inherited) > 0 {
			s.env.CapabilitiesEnforced["harness.env"] = hostEnvLine(started.inherited)
		}
		run = wait
	}
	go func() {
		code, rerr := run()
		_ = stdoutW.Close()
		_ = stderrFile.Close()
		s.redact.File(s.stderrPath)
		s.mu.Lock()
		s.exitCode = code
		if rerr != nil {
			s.harnessErr = rerr.Error()
		}
		s.mu.Unlock()
		close(s.exited)
		s.endWith(session.EndHarness)
	}()
	// A failure from here on has a harness process running and writing
	// its stderr receipt; the goroutine above masks that receipt once the
	// process has exited, so the failure waits for it (bounded — the
	// cancel kills the process) before the receipt is handed over.
	awaitExit := func() {
		select {
		case <-s.exited:
		case <-time.After(15 * time.Second):
			return
		}
		// The goroutine ends the session right after the exit: wait for
		// its session_end to reach the events receipt, which the failure
		// then closes (agentry-cli#459).
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
		}
	}
	drv, err := p.Adapter.Session(agentCtx, rwPipe{Reader: stdoutR, WriteCloser: stdinW}, adapter.SessionOptions{GatewayTools: p.Plan.Tools})
	if err != nil {
		cancel(err)
		awaitExit()
		return Classify(ExitHarness, fmt.Errorf("interactive session: %w", err))
	}
	s.drv = drv
	handshake, cancelHS := context.WithTimeout(agentCtx, 120*time.Second)
	defer cancelHS()
	if err := drv.Start(handshake); err != nil {
		cancel(err)
		awaitExit()
		return Classify(ExitHarness, fmt.Errorf("interactive session: %w%s", err, stderrTail(s.stderrPath)))
	}
	s.lastActivity = time.Now()
	return nil
}

// pump turns adapter events into session events and applies the tool and
// cost budgets exactly as the autonomous collector does.
func (s *liveSession) pump() {
	defer close(s.pumpDone)
	// A panic here would take the process down, and the session's teardown
	// runs only from End — so the sandbox, the gateway holding the docker
	// socket, the run network and secrets.env would outlive it, bounded by
	// nothing, since the budgets are applied by the goroutine that died
	// (agentry-cli#161). Recover, name it, and end the session so End runs.
	defer func() {
		if r := recover(); r != nil {
			s.recordPanic(fmt.Errorf("session event pump panicked: %v", r))
		}
	}()
	events := s.drv.Events()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			s.handle(ev)
		case ack := <-s.pumpSync:
			// Everything the driver queued before the caller asked is in the
			// channel already; take it without waiting for more.
			for drained := false; !drained; {
				select {
				case ev, ok := <-events:
					if !ok {
						close(ack)
						return
					}
					s.handle(ev)
				default:
					drained = true
				}
			}
			close(ack)
		}
	}
}

// syncPump returns once the pump has recorded every event the driver had
// queued when it was called. SessionDriver queues all of a turn's events
// before Prompt returns, so after a turn this puts the turn's tool calls
// and permission decisions in the log ahead of assistant_turn, and ahead
// of an End that would otherwise make emit discard them (agentry-cli#204).
func (s *liveSession) syncPump() {
	ack := make(chan struct{})
	select {
	case s.pumpSync <- ack:
	case <-s.pumpDone:
		return
	}
	select {
	case <-ack:
	case <-s.pumpDone:
	}
}

// handle records one driver event (the pump's body).
func (s *liveSession) handle(ev adapter.Event) {
	if pumpEventHook != nil {
		pumpEventHook()
	}
	switch ev.Kind {
	case adapter.EventAssistantText:
		s.emit(session.Event{Kind: session.KindAssistantText, Text: ev.Text, Raw: ev.Raw})
	case adapter.EventToolCall:
		s.emit(session.Event{Kind: session.KindToolCall, Text: ev.Text, Raw: ev.Raw})
		s.mu.Lock()
		s.toolCalls++
		over := s.p.Budgets.MaxTurns > 0 && s.toolCalls > s.p.Budgets.MaxTurns
		s.mu.Unlock()
		if over {
			s.endWith(session.EndToolBudget)
		}
	case adapter.EventUsage:
		s.mu.Lock()
		s.inTok += ev.InputTokens
		s.outTok += ev.OutputTokens
		s.totTok += ev.TotalTokens
		if ev.CostUSD > s.costUSD {
			s.costUSD = ev.CostUSD
		}
		over := s.p.Budgets.MaxCostUSD > 0 && s.costUSD > s.p.Budgets.MaxCostUSD
		s.mu.Unlock()
		s.emit(session.Event{Kind: session.KindUsage, InputTokens: ev.InputTokens, OutputTokens: ev.OutputTokens, TotalTokens: ev.TotalTokens, CostUSD: ev.CostUSD, Raw: ev.Raw})
		if over {
			s.endWith(session.EndCostBudget)
		}
	case adapter.EventPermissionGranted:
		s.emit(session.Event{Kind: session.KindPermissionGranted, Text: ev.Text, Raw: ev.Raw})
	case adapter.EventPermissionDenied:
		s.emit(session.Event{Kind: session.KindPermissionDenied, Text: ev.Text, Raw: ev.Raw})
	case adapter.EventError:
		s.mu.Lock()
		s.harnessErr = ev.Text
		s.mu.Unlock()
		s.emit(session.Event{Kind: session.KindError, Text: ev.Text, Raw: ev.Raw})
	default:
		s.emit(session.Event{Kind: session.KindOther, Text: ev.Text, Raw: ev.Raw})
	}
}

// recordPanic stores a recovered panic as the session's error and ends the
// session, so End runs the teardown and names the panic instead of the
// process dying with the sandbox still up (agentry-cli#161). The first panic
// is the one kept: whatever follows is its fallout.
func (s *liveSession) recordPanic(err error) {
	s.mu.Lock()
	if s.panicErr == nil {
		s.panicErr = err
	}
	s.mu.Unlock()
	s.endWith(session.EndCancelled)
}

// watch ends the session on silence (plan §6.7); the wall clock is the
// agent context's deadline.
func (s *liveSession) watch() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	// As the pump's: a panic here would leave the session's containers up
	// with nothing left to apply the idle or wall budgets (agentry-cli#161).
	defer func() {
		if r := recover(); r != nil {
			s.recordPanic(fmt.Errorf("session watcher panicked: %v", r))
		}
	}()
	for {
		select {
		case <-s.done:
			return
		case <-s.agentCtx.Done():
			if errors.Is(context.Cause(s.agentCtx), context.DeadlineExceeded) {
				s.endWith(session.EndWall)
			} else if s.agentCtx.Err() != nil {
				s.endWith(session.EndCancelled)
			}
			return
		case <-tick.C:
			if s.budgets.IdleTimeoutSeconds <= 0 {
				continue
			}
			s.mu.Lock()
			idle := time.Since(s.lastActivity)
			s.mu.Unlock()
			if idle > time.Duration(s.budgets.IdleTimeoutSeconds)*time.Second {
				s.endWith(session.EndIdle)
				return
			}
		}
	}
}

// emit records an event (redacted) and delivers it to the consumer.
func (s *liveSession) emit(ev session.Event) {
	ev.Time = time.Now().UTC()
	ev.Text = s.redact.String(ev.Text)
	if len(ev.Raw) > 0 {
		ev.Raw = json.RawMessage(s.redact.String(string(ev.Raw)))
	}
	s.mu.Lock()
	if s.ended && ev.Kind != session.KindSessionEnd {
		s.mu.Unlock()
		return
	}
	s.log = append(s.log, ev)
	if ev.Kind == session.KindAssistantText || ev.Kind == session.KindToolCall || ev.Kind == session.KindUserTurn {
		s.lastActivity = time.Now()
	}
	if s.eventsFile != nil {
		if b, err := json.Marshal(ev); err == nil {
			_, _ = s.eventsFile.Write(append(b, '\n'))
		}
	}
	s.mu.Unlock()
	select {
	case s.events <- ev:
	case <-s.done:
	}
}

// endWith marks the session over for reason (first reason wins), stops
// the harness, and closes the event stream after the session_end event.
func (s *liveSession) endWith(reason session.EndReason) {
	s.endOnce.Do(func() {
		s.mu.Lock()
		s.ended, s.reason = true, reason
		s.mu.Unlock()
		s.emit(session.Event{Kind: session.KindSessionEnd, Reason: reason})
		close(s.done)
		if s.drv != nil {
			// stdin EOF: the harness exits by itself; the context kill is
			// the backstop.
			_ = s.stdinW.Close()
		}
		go func() {
			select {
			case <-s.exited:
			case <-time.After(15 * time.Second):
				s.cancel(errors.New("session ended: " + string(reason)))
			}
		}()
	})
}

// Events implements session.Session.
func (s *liveSession) Events() <-chan session.Event { return s.events }

// Ended implements session.Session.
func (s *liveSession) Ended() (bool, session.EndReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended, s.reason
}

// Send implements session.Session.
func (s *liveSession) Send(ctx context.Context, text string) error {
	return s.turn(ctx, text, false)
}

func (s *liveSession) turn(ctx context.Context, text string, synthetic bool) error {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return session.ErrEnded
	}
	if !synthetic {
		s.userTurns++
		if s.budgets.MaxUserTurns > 0 && s.userTurns > s.budgets.MaxUserTurns {
			s.mu.Unlock()
			s.endWith(session.EndUserTurns)
			return session.ErrEnded
		}
	}
	notices := s.pendingNotices
	s.pendingNotices = nil
	s.mu.Unlock()
	if len(notices) > 0 {
		text = strings.Join(notices, "\n") + "\n\n" + text
	}
	s.emit(session.Event{Kind: session.KindUserTurn, Text: text, Synthetic: synthetic})
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.done:
			cancel()
		case <-turnCtx.Done():
		}
	}()
	res, err := s.drv.Prompt(turnCtx, text)
	s.syncPump()
	if err != nil {
		if ended, _ := s.Ended(); ended {
			return session.ErrEnded
		}
		s.emit(session.Event{Kind: session.KindError, Text: err.Error()})
		return err
	}
	s.mu.Lock()
	if res.Text != "" {
		// The final result is built from this; it is redacted like every
		// receipt (§R11 invariant 7).
		s.lastText = s.redact.String(res.Text)
	}
	s.lastActivity = time.Now()
	s.mu.Unlock()
	s.emit(session.Event{Kind: session.KindAssistantTurn, Text: res.Text, StopReason: res.StopReason})
	return nil
}

// AddInput implements session.Session (plan §6.4, §6.10 line 3): only a
// regular host file, only under the staged inputs directory, agent-side
// mount unchanged (:ro). The agent hears about it as a user turn.
func (s *liveSession) AddInput(ctx context.Context, hostPath string) error {
	if ended, _ := s.Ended(); ended {
		return session.ErrEnded
	}
	abs, err := filepath.Abs(hostPath)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", hostPath, err)
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return fmt.Errorf("input file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("input %s is not a regular file (directories and links are not accepted mid-run)", hostPath)
	}
	base := filepath.Base(abs)
	target := filepath.Join(s.rd.inputs, base)
	if rel, err := filepath.Rel(s.rd.inputs, target); err != nil || strings.HasPrefix(rel, "..") || base == "." || base == "" {
		return fmt.Errorf("input name %q is not allowed", base)
	}
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("an input named %q is already staged — rename the file (inputs are basename-rooted under %s)", base, spec.InputsMount)
	}
	data, err := os.ReadFile(abs) //nolint:gosec // user-named input file
	if err != nil {
		return fmt.Errorf("read input %s: %w", hostPath, err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil { //nolint:gosec // inputs are agent-readable by design
		return fmt.Errorf("stage input %s: %w", hostPath, err)
	}
	sum := sha256.Sum256(data)
	agentPath := spec.InputsMount + "/" + base
	digest := "sha256:" + hex.EncodeToString(sum[:])
	s.emit(session.Event{Kind: session.KindInputAdded, Path: agentPath, Digest: digest, Text: base})
	notice := fmt.Sprintf("A new input file is available at %s (%s); read it with read_input.", agentPath, digest)
	if s.turnMu.TryLock() {
		s.turnMu.Unlock()
		return s.turn(ctx, notice, true)
	}
	s.mu.Lock()
	s.pendingNotices = append(s.pendingNotices, notice)
	s.mu.Unlock()
	return nil
}

// End implements session.Session.
func (s *liveSession) End(ctx context.Context, finish bool) (*receipt.Envelope, error) {
	s.endMu.Lock()
	defer s.endMu.Unlock()
	s.mu.Lock()
	if s.envelope != nil {
		env, err := s.envelope, s.envelopeErr
		s.mu.Unlock()
		return env, err
	}
	s.mu.Unlock()

	if ended, _ := s.Ended(); !ended {
		if finish && s.p.Plan.Harness.ResultKind == "json" {
			_ = s.turn(ctx, "Reply now with your final result as JSON matching the declared output schema, and nothing else.", true)
		}
		if finish {
			s.endWith(session.EndFinished)
		} else {
			s.endWith(session.EndQuit)
		}
	}
	select {
	case <-s.exited:
	case <-time.After(20 * time.Second):
		s.cancel(errors.New("harness did not exit"))
		<-s.exited
	}
	if s.drv != nil {
		_ = s.drv.Close()
		// The driver's Close ends its event stream; wait for the pump to
		// process what was still buffered (the last turn's usage, a late
		// tool call) before the counters are read into the envelope
		// (agentry-cli#69). Bounded: a wedged pump must not hang End.
		select {
		case <-s.pumpDone:
		case <-time.After(10 * time.Second):
		}
	}
	close(s.events)
	if s.eventsFile != nil {
		_ = s.eventsFile.Close()
	}
	if logs, lerr := s.d.Logs(ctx, containerName(s.runID, "gateway"), 2000); lerr == nil && logs != "" {
		_ = os.WriteFile(filepath.Join(s.rd.receiptsDir, "gateway.log"), []byte(s.redact.String(logs)), 0o644)
	}
	if logs, lerr := s.d.Logs(ctx, containerName(s.runID, "proxy"), 2000); lerr == nil && logs != "" {
		_ = os.WriteFile(filepath.Join(s.rd.receiptsDir, "proxy.log"), []byte(s.redact.String(logs)), 0o644)
	}
	writeMeta(s.rd.receiptsDir, s.runID, s.p, s.start, "")
	daemon := daemonLine(s.rd)

	s.mu.Lock()
	reason := s.reason
	log := append([]session.Event(nil), s.log...)
	lastText := s.lastText
	env := s.env
	env.Usage = receipt.Usage{InputTokens: s.inTok, OutputTokens: s.outTok, TotalTokens: s.totTok, CostUSD: s.costUSD, Turns: s.toolCalls, UserTurns: s.userTurns}
	harnessErr := s.harnessErr
	panicErr := s.panicErr
	s.mu.Unlock()
	env.Usage.WallMS = time.Since(s.start).Milliseconds()
	env.CapabilitiesEnforced = withCostBudget(env.CapabilitiesEnforced, s.p.Budgets.MaxCostUSD, env.Usage.CostUSD)
	env.CapabilitiesEnforced["daemon"] = daemon
	_ = os.WriteFile(filepath.Join(s.rd.receiptsDir, "transcript.md"), []byte(transcript(env, log)), 0o644)

	var outErr error
	switch reason {
	case session.EndFinished, session.EndQuit:
		schema, serr := outputSchema(s.p)
		if serr != nil {
			outErr = serr
		} else if reason == session.EndFinished || s.p.Plan.Harness.ResultKind != "json" {
			result, rerr := finalResult(s.p.Plan.Harness.ResultKind, lastText, schema)
			if rerr != nil {
				env.Result = receipt.TextResult(lastText)
				outErr = rerr
			} else {
				env.Result = result
			}
		} else {
			env.Result = receipt.TextResult(lastText)
		}
	case session.EndToolBudget:
		outErr = Classify(ExitBudget, fmt.Errorf("turn budget exceeded (%d tool calls)", s.p.Budgets.MaxTurns))
	case session.EndCostBudget:
		outErr = Classify(ExitBudget, fmt.Errorf("cost budget exceeded (harness reported $%.4f, budget $%.2f)", env.Usage.CostUSD, s.p.Budgets.MaxCostUSD))
	case session.EndIdle:
		outErr = Classify(ExitTimeout, fmt.Errorf("session idle for more than %ds", s.budgets.IdleTimeoutSeconds))
	case session.EndWall:
		outErr = Classify(ExitTimeout, fmt.Errorf("session exceeded %ds", s.budgets.MaxWallSeconds))
	case session.EndUserTurns:
		outErr = Classify(ExitBudget, fmt.Errorf("user-turn budget exceeded (%d turns)", s.budgets.MaxUserTurns))
	case session.EndHarness:
		detail := stderrTail(s.stderrPath)
		if harnessErr != "" {
			detail = "\nharness error: " + harnessErr + detail
		}
		outErr = Classify(ExitHarness, fmt.Errorf("agent exited before the session ended%s", detail))
	default:
		outErr = Classify(ExitCLI, fmt.Errorf("session %s", reason))
	}
	if panicErr != nil {
		// The panic is the error, whatever reason the recovery ended the
		// session with: that reason is the recovery's own doing, not what
		// went wrong (agentry-cli#161).
		outErr = Classify(ExitCLI, panicErr)
	}
	if outErr != nil {
		env.Result = receipt.TextResult(lastText)
		env, outErr = fail(env, s.redact, outErr)
	} else {
		env.Status = "ok"
		env.ExitCode = ExitOK
		if err := env.Write(s.p.OutDir); err != nil {
			_, _ = fmt.Fprintf(s.p.Stderr, "warning: %v\n", err)
		}
	}
	s.td.run()
	if err := s.td.reportLeftFiles(env); err != nil && outErr == nil {
		outErr = err
	}
	s.mu.Lock()
	s.envelope, s.envelopeErr = env, outErr
	s.mu.Unlock()
	return env, outErr
}

// transcript renders the readable conversation from the (already
// redacted) event log (plan §6.5).
func transcript(env *receipt.Envelope, log []session.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Transcript: %s\n\nrun %s · interactive · %s\n\n", env.Ref, env.RunID, env.Harness.Model)
	for _, ev := range log {
		switch ev.Kind {
		case session.KindUserTurn:
			who := "User"
			if ev.Synthetic {
				who = "Runner"
			}
			fmt.Fprintf(&b, "## %s\n\n%s\n\n", who, strings.TrimSpace(ev.Text))
		case session.KindAssistantTurn:
			fmt.Fprintf(&b, "## Assistant\n\n%s\n\n", strings.TrimSpace(ev.Text))
		case session.KindToolCall:
			fmt.Fprintf(&b, "- tool call: %s\n", ev.Text)
		case session.KindPermissionGranted:
			fmt.Fprintf(&b, "- permission granted: %s\n", ev.Text)
		case session.KindPermissionDenied:
			fmt.Fprintf(&b, "- permission request denied: %s\n", ev.Text)
		case session.KindInputAdded:
			fmt.Fprintf(&b, "- input added: %s (%s)\n", ev.Path, ev.Digest)
		case session.KindError:
			fmt.Fprintf(&b, "- error: %s\n", ev.Text)
		case session.KindSessionEnd:
			fmt.Fprintf(&b, "\n_Session ended: %s._\n", ev.Reason)
		}
	}
	return b.String()
}

// hostSecretValues collects the host-environment values the plan injects
// into the agent container (the model API key) for redaction.
func hostSecretValues(vars []spec.EnvVar) []string {
	var out []string
	for _, v := range vars {
		if v.FromHostEnv != "" {
			// The same resolution the run uses (environment, else the
			// keychain), so a key read from the keychain is redacted like
			// one read from the environment; a lookup that fails here is
			// the run's refusal to make, not the redactor's.
			if val, err := hostSecret(v); err == nil && val != "" {
				out = append(out, val)
			}
		}
	}
	return out
}

// withCostBudget adds the receipt's line about the session's cost budget.
// A cost of 0 is ambiguous: the harness may have reported none, which is
// not the same as a session that cost nothing. And a ceiling asked for
// against such a harness could never have fired, so the line says that
// rather than leaving the control looking enforced (agentry-cli#222).
// docker-agent 1.128.0 sends `used` and no cost on its ACP usage_update,
// which makes this the default adapter's normal case, not an edge.
//
// A copy: enforcedWithPackages hands back the plan's own map when the run
// resolved no exec packages, and the plan's map is the plan's.
func withCostBudget(enforced map[string]string, ceiling, reported float64) map[string]string {
	const unreported = "the harness reported no cost on its event stream, so usage.costUsd is 0 because nothing was reported, not because the session cost nothing"
	var line string
	switch {
	case reported > 0 && ceiling > 0:
		line = fmt.Sprintf("cost ceiling $%.2f enforced on the harness's reported running cost ($%.4f)", ceiling, reported)
	case reported > 0:
		line = fmt.Sprintf("no cost ceiling requested; the harness reported a running cost of $%.4f", reported)
	case ceiling > 0:
		line = fmt.Sprintf("cost ceiling $%.2f requested but not enforceable: %s", ceiling, unreported)
	default:
		line = unreported
	}
	out := maps.Clone(enforced)
	if out == nil {
		out = make(map[string]string, 1)
	}
	out["budgets.cost"] = line
	return out
}
