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
// attached stdio and returns the session. The plan must be an interactive
// plan (Mode interactive, Harness.Session set): it is byte-identical to
// the autonomous plan except the harness argv and that block, so the
// sandbox — network, proxy, gateway, mounts, secrets — is exactly the
// autonomous one. The task text, if any, is the opening user turn.
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
			// A session's receipt names the pin and the credential kind as a
			// run's does.
			ImagePin:       p.Plan.Harness.ImagePin.String(),
			CredentialKind: credentialKind(p.Plan),
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

	// The engine, and where the run's docker filter runs on it, before
	// anything touches the daemon.
	place, err := placementFor(ctx, d)
	if err != nil {
		_, cerr := fail(env, nil, err)
		return nil, cerr
	}
	p.place = &place
	env.CapabilitiesEnforced["engine"] = place.line()
	if p.Plan.Runtime() != "" {
		env.CapabilitiesEnforced["runtime"] = runtimeLine(p)
	}

	// What a run whose process died left behind goes first, before this
	// run adds its own.
	sweepAbandonedRun(ctx, d, p.RunRoot, p.Stderr)
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
		// deleted until the process exits. A harness that already exited
		// has written its session_end (awaitExit waits for it); emit
		// writes under s.mu and skips a nil file, so a later write is
		// dropped, never made to a closed file.
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
	// Nor the review grant, a built-in's autonomous review.
	if p.Plan.Capabilities.ReviewTools || p.Review != nil {
		return fail(Classify(ExitPolicy, errors.New("the review grant does not run in an interactive session")))
	}
	// Nor end_blocked: a session's person sees the agent stop, and ends it.
	if p.EndBlocked {
		return fail(Classify(ExitPolicy, errors.New("end_blocked does not run in an interactive session")))
	}
	if err := checkVoice(p); err != nil {
		return fail(err)
	}
	if err := checkModelSignIn(p); err != nil {
		return fail(err)
	}
	if err := checkBrowser(ctx, p); err != nil {
		return fail(err)
	}
	if err := checkTargetHosts(p); err != nil {
		return fail(err)
	}
	env.CapabilitiesEnforced = withSignInRenewal(env.CapabilitiesEnforced, p)
	rd, err := prepareRunDirs(p, runID, td)
	if err != nil {
		return fail(err)
	}
	// The System Prompt tells the model it is in a session (spec.PHRunMode).
	rd.interactive = true
	s.rd = rd
	// What the driver may permit is read now, before any harness starts:
	// a host-run harness runs as the person and could rewrite the file.
	if s.granted, err = grantedTools(filepath.Join(rd.cfg, "gateway")); err != nil {
		return fail(Classify(ExitCLI, fmt.Errorf("the run's tool grant: %w", err)))
	}
	// The inputs are decoded before anything starts, and a file added
	// later before it is announced.
	srcRuns, err := fetchSources(ctx, d, p, rd, td, runID)
	if err != nil {
		return fail(err)
	}
	rd.sources = sourceIDs(srcRuns)
	if s.dec, err = decodeInputs(ctx, d, p, rd); err != nil {
		return fail(err)
	}
	s.dec.lines(env.CapabilitiesEnforced)
	sourceLines(env.CapabilitiesEnforced, srcRuns, s.dec)
	// The session store: a per-run named volume, labelled like every run
	// object and removed with it, not a host directory the harness
	// could write to world-wide. A fresh volume is root-owned
	// wherever it is mounted and the harness runs unprivileged, so the
	// volume is given to the harness image's own user first — as root,
	// in that image, on no network, once.
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
		code, err := dockerx.RunForegroundUnder(ctx, d, initSpec, "session store's one-shot", nil, io.Discard, p.Stderr)
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
	if err := writeConnectionFiles(p, rd, secrets); err != nil {
		return fail(err)
	}
	if err := prepareBrowser(ctx, d, p, rd, td, env, secrets); err != nil {
		return fail(err)
	}
	if err := prepareTargetHosts(ctx, d, p, rd, td, env); err != nil {
		return fail(err)
	}
	// The optional secrets it goes without, before any replacer is built:
	// the System Prompt names them (spec.PHSecretsAbsent), and so does the
	// receipt.
	rd.secretsAbsent = absentSecrets(p.Plan, secrets)
	env.CapabilitiesEnforced = withAbsentNote(env.CapabilitiesEnforced, rd.secretsAbsent)
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
	// A session's connections are renewed for as long as it lives;
	// teardown stops the renewer before it deletes their folders.
	stopRenewal := renewConnections(ctx, p, rd)
	td.add(func(context.Context) { stopRenewal() })

	var served []string
	infraErr := startInfra(ctx, d, p, rd, td, runID, token, rep, &served)
	if len(rd.modelHosts) > 0 {
		env.CapabilitiesEnforced["llm-proxy.hosts"] = modelHostsLine(rd.modelHosts)
	}
	if infraErr != nil {
		env.CapabilitiesEnforced["daemon"] = daemonLine(rd)
		return fail(withRefusals(infraErr, rd))
	}
	// The driver also permits what the gateway serves beyond the run's own
	// grant: the declared servers' tools. Still before any harness starts.
	s.granted = permitted(s.granted, served)
	// Harness files after the infrastructure, as in Run: a host-run harness
	// needs the published addresses.
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
// machine does.
var pumpEventHook func()

// liveSession is the engine's session.Session.
type liveSession struct {
	d   dockerx.Client
	p   Params
	env *receipt.Envelope
	td  *teardown
	rd  *runDirs
	// granted is the run's own tool grant, read from its rendered tools
	// spec after the run-time grants: what the driver permits.
	granted []string
	// dec decodes the inputs, a mid-run file before it is announced; nil
	// when the caller decoded them or staged its own (every built-in).
	dec    *inputsDecoder
	start  time.Time
	runID  string
	redact *redactor

	budgets spec.SessionBudgets

	drv      adapter.SessionDriver
	agentCtx context.Context
	cancel   context.CancelCauseFunc
	// stdinW and exited are the running harness's (harness()); a restart
	// replaces them, under mu.
	stdinW   io.Closer
	exited   chan struct{} // closed when the harness has exited
	exitCode int
	token    string
	rep      *strings.Replacer
	// signInSum is the model sign-in's token file's digest when the
	// harness started; restarting is set while the restart route ends the
	// harness on purpose, and restarts counts them (the receipt says so).
	signInSum  string
	restarting bool
	restarts   int
	eventsFile *os.File
	stderrPath string

	events chan session.Event
	// eventsMu orders emit's send against End's close(events);
	// eventsClosed is set under it when the channel closes.
	eventsMu     sync.RWMutex
	eventsClosed bool
	done         chan struct{} // closed once (ended)
	pumpDone     chan struct{} // closed when pump has drained the driver's events
	// pumpSync asks the pump to record everything the driver has queued so
	// far, then close the channel it was sent.
	pumpSync chan chan struct{}

	turnMu sync.Mutex // one prompt at a time
	mu     sync.Mutex // everything below
	ended  bool
	reason session.EndReason
	log    []session.Event
	// inTurn is true while a turn has the harness, from after its checks
	// until just before its last event. AddInput reads it rather than
	// turnMu: a turn emits assistant_turn (or error) while it still holds
	// turnMu, so a client that adds a file the moment it sees that event
	// would otherwise find the session busy and its notice would wait for
	// the next user turn.
	inTurn bool
	// ending is set once End has begun: an input added after it starts no
	// turn, so nothing can run between End's final-result turn and the end
	// and replace the result it asked for.
	ending bool
	// pendingNotices are input notices to prepend to the next user turn
	// because one was in flight when the input arrived.
	pendingNotices []string
	userTurns      int
	toolCalls      int
	inTok, outTok  int64
	totTok         int64
	// usageSeen says the harness reported usage at least once; without it
	// the receipt says the session's 0 tokens were never reported.
	usageSeen  bool
	costUSD    float64
	lastText   string
	harnessErr string
	// planLimit is the harness's first plan-limit event (EndPlanLimit).
	planLimit *adapter.Event
	// orgPolicy is the harness's first organization-policy event
	// (EndOrgPolicy).
	orgPolicy *adapter.Event
	// outOfCredits is the provider's first out-of-credits answer
	// (EndOutOfCredits).
	outOfCredits *adapter.Event
	// panicErr is set when the pump or the watcher recovered a panic. It is
	// the session's error whatever reason it then ended with, because that
	// reason is a side effect of the recovery.
	panicErr     error
	lastActivity time.Time
	endOnce      sync.Once
	// endMu serialises End: a second caller waits for the first and gets
	// its envelope. Without it two concurrent callers both passed the
	// envelope check and both closed events (a desktop quit mid-run is
	// such a caller).
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
	eventsFile, err := os.Create(filepath.Join(s.rd.receiptsDir, "events.ndjson")) //nolint:gosec // our receipts dir
	if err != nil {
		return Classify(ExitCLI, fmt.Errorf("create events receipt: %w", err))
	}
	s.eventsFile = eventsFile
	s.stderrPath = filepath.Join(s.rd.receiptsDir, "agent-stderr.log")

	agentCtx, cancel := context.WithCancelCause(ctx)
	if s.budgets.MaxWallSeconds > 0 {
		var cancelWall context.CancelFunc
		agentCtx, cancelWall = context.WithTimeout(agentCtx, time.Duration(s.budgets.MaxWallSeconds)*time.Second)
		s.td.add(func(context.Context) { cancelWall() })
	}
	s.agentCtx, s.cancel = agentCtx, cancel
	s.token, s.rep = token, rep
	if p.Plan.Harness.Host == nil {
		name := containerName(s.runID, "agent")
		s.td.add(func(c context.Context) { _ = s.d.RemoveForce(c, name) })
	}

	rw, err := s.launch(false)
	if err != nil {
		cancel(err)
		return err
	}
	_, exited := s.harness()
	// A failure from here on has a harness process running and writing
	// its stderr receipt; the goroutine launch started masks that receipt
	// once the process has exited, so the failure waits for it (bounded —
	// the cancel kills the process) before the receipt is handed over.
	awaitExit := func() {
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			return
		}
		// The goroutine ends the session right after the exit: wait for
		// its session_end to reach the events receipt, which the failure
		// then closes.
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
		}
	}
	restart := p.Plan.Harness.Session.RestartOnRenewal
	opts := sessionOptions(p, s.rd, token, s.granted)
	opts.Resumable = restart
	drv, err := p.Adapter.Session(agentCtx, rw, opts)
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
		var refused *adapter.OrgPolicyRefusal
		if errors.As(err, &refused) {
			return orgPolicyError(&adapter.Event{Policy: refused.Policy, Text: refused.Text})
		}
		return Classify(ExitHarness, fmt.Errorf("interactive session: %w%s", err, stderrTail(s.stderrPath)))
	}
	s.signInSum = s.signInTokenSum()
	s.lastActivity = time.Now()
	return nil
}

// harness is the running harness's stdin and the channel closed when it
// exits; a restart replaces both.
func (s *liveSession) harness() (io.Closer, chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stdinW, s.exited
}

// launch starts the harness — the agent container, or the person's own
// installation — on fresh stdio pipes, and the goroutine that records its
// exit and ends the session with it, unless the exit is a restart's. It
// returns the harness's stdout and stdin for the driver. again is a
// restart: the stderr receipt is appended to, and a container of the same
// name is removed first (docker removes an exited --rm container on its
// own time).
func (s *liveSession) launch(again bool) (io.ReadWriteCloser, error) {
	p, rep, token := s.p, s.rep, s.token
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if again {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	stderrFile, err := os.OpenFile(s.stderrPath, flags, 0o644) //nolint:gosec // our receipts dir
	if err != nil {
		return nil, Classify(ExitCLI, fmt.Errorf("create stderr receipt: %w", err))
	}
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	var run func() (int, error)
	if p.Plan.Harness.Host != nil {
		wait, started, herr := startHostProcess(s.agentCtx, p, s.rd, rep, token, stdinR, stdoutW, stderrFile)
		if herr != nil {
			// No harness process, so no goroutine to close the stderr
			// receipt.
			_ = stderrFile.Close()
			return nil, herr
		}
		s.env.Harness.Host = started.info
		if len(started.inherited) > 0 {
			s.env.CapabilitiesEnforced["harness.env"] = hostEnvLine(started.inherited)
		}
		run = wait
	} else {
		agentEnv, err := resolveEnv(p.Plan.Harness.Agent.Env, rep, token)
		if err != nil {
			_ = stderrFile.Close()
			return nil, err
		}
		mounts := resolveMounts(p.Plan.Harness.Agent.Mounts, rep)
		mounts = append(mounts, resolveMounts(p.Plan.Harness.Session.Mounts, rep)...)
		name := containerName(s.runID, "agent")
		hosts, err := sandboxHosts(s.agentCtx, s.d, p.Plan, s.runID)
		if err != nil {
			_ = stderrFile.Close()
			return nil, Classify(ExitGateway, err)
		}
		runSpec := hardenedAgent(dockerx.RunSpec{
			Name:        name,
			Image:       p.Plan.Harness.Agent.Image,
			Entrypoint:  p.Plan.Harness.Agent.Entrypoint,
			Argv:        substituteAll(p.Plan.Harness.Agent.Argv, rep),
			Env:         agentEnv,
			Mounts:      mounts,
			Network:     networkName(s.runID),
			Labels:      runLabels(s.runID),
			Interactive: true,
			AutoRemove:  true,
			Runtime:     p.Plan.Runtime(),
			ExtraHosts:  hosts,
		}, p)
		if again {
			_ = s.d.RemoveForce(s.agentCtx, name)
		}
		run = func() (int, error) {
			return dockerx.RunForegroundUnder(s.agentCtx, s.d, runSpec, "agent container", stdinR, stdoutW, stderrFile)
		}
	}
	exited := make(chan struct{})
	s.mu.Lock()
	s.stdinW, s.exited = stdinW, exited
	s.mu.Unlock()
	go func() {
		code, rerr := run()
		_ = stdoutW.Close()
		_ = stdinR.Close()
		_ = stderrFile.Close()
		s.redact.File(s.stderrPath)
		s.mu.Lock()
		s.exitCode = code
		if rerr != nil {
			s.harnessErr = rerr.Error()
		}
		restarting := s.restarting
		s.mu.Unlock()
		close(exited)
		if !restarting {
			s.endWith(session.EndHarness)
		}
	}()
	return rwPipe{Reader: stdoutR, WriteCloser: stdinW}, nil
}

// signInTokenSum is a digest of the model sign-in's token file as it is
// now, "" when the run has none: how the restart route knows the runner
// renewed the token since the harness started (the renewer rewrites the
// file, connections.go).
func (s *liveSession) signInTokenSum() string {
	if s.p.Plan.Harness.ModelSignIn == nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(s.rd.modelSignInDir(), spec.ModelSignInTokenFile)) //nolint:gosec // the run's own folder
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// restartTimeout bounds a restart: the old harness's exit, the new one's
// start and the driver's resume.
var restartTimeout = 120 * time.Second

// restartOnRenewal starts the harness again when the plan reads its token
// once at start (spec.SessionSpec.RestartOnRenewal) and the runner has
// renewed it since: the old harness is ended (stdin closed), a new one is
// launched on the renewed token, and the driver resumes the conversation
// (adapter.Resumer). Called before a turn, under turnMu.
func (s *liveSession) restartOnRenewal(ctx context.Context) error {
	if !s.p.Plan.Harness.Session.RestartOnRenewal {
		return nil
	}
	now := s.signInTokenSum()
	if now == "" || now == s.signInSum {
		return nil
	}
	rs, ok := s.drv.(adapter.Resumer)
	if !ok {
		return fmt.Errorf("the %s adapter cannot resume a conversation, so its renewed token cannot reach it", s.p.Plan.Harness.Adapter)
	}
	rctx, cancel := context.WithTimeout(ctx, restartTimeout)
	defer cancel()
	s.mu.Lock()
	s.restarting = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.restarting = false
		s.mu.Unlock()
	}()
	stdin, exited := s.harness()
	_ = stdin.Close()
	select {
	case <-exited:
	case <-rctx.Done():
		return fmt.Errorf("restart on the renewed token: the harness did not exit: %w", rctx.Err())
	}
	if ended, _ := s.Ended(); ended {
		return session.ErrEnded
	}
	rw, err := s.launch(true)
	if err != nil {
		return fmt.Errorf("restart on the renewed token: %w", err)
	}
	if err := rs.Resume(rctx, rw); err != nil {
		return fmt.Errorf("restart on the renewed token: %w%s", err, stderrTail(s.stderrPath))
	}
	s.signInSum = now
	s.mu.Lock()
	s.restarts++
	s.mu.Unlock()
	return nil
}

// pump turns adapter events into session events and applies the tool and
// cost budgets exactly as the autonomous collector does.
func (s *liveSession) pump() {
	defer close(s.pumpDone)
	// A panic here would take the process down, and the session's teardown
	// runs only from End — so the sandbox, the gateway holding the docker
	// socket, the run network and secrets.env would outlive it, bounded by
	// nothing, since the budgets are applied by the goroutine that died.
	// Recover, name it, and end the session so End runs.
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
// of an End that would otherwise make emit discard them.
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
		s.usageSeen = true
		// A plan run reports tokens, never dollars.
		if ev.CostUSD > s.costUSD && s.p.Plan.Harness.ModelSignIn == nil {
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
	case adapter.EventPlanLimit:
		s.mu.Lock()
		first := s.planLimit == nil
		if first {
			s.planLimit = &ev
		}
		s.mu.Unlock()
		s.emit(session.Event{Kind: session.KindError, Text: "plan limit reached: " + ev.Text, Raw: ev.Raw})
		if first {
			s.endWith(session.EndPlanLimit)
		}
	case adapter.EventOutOfCredits:
		s.mu.Lock()
		first := s.outOfCredits == nil
		if first {
			s.outOfCredits = &ev
		}
		s.harnessErr = ev.Text
		s.mu.Unlock()
		s.emit(session.Event{Kind: session.KindError, Text: "out of credits: " + ev.Text, Raw: ev.Raw})
		if first {
			s.endWith(session.EndOutOfCredits)
		}
	case adapter.EventOrgPolicy:
		s.mu.Lock()
		first := s.orgPolicy == nil
		if first {
			s.orgPolicy = &ev
		}
		s.mu.Unlock()
		s.emit(session.Event{Kind: session.KindError, Text: "refused by an organization's policy: " + ev.Text, Raw: ev.Raw})
		if first {
			s.endWith(session.EndOrgPolicy)
		}
	default:
		s.emit(session.Event{Kind: session.KindOther, Text: ev.Text, Raw: ev.Raw})
	}
}

// recordPanic stores a recovered panic as the session's error and ends the
// session, so End runs the teardown and names the panic instead of the
// process dying with the sandbox still up. The first panic is the one kept:
// whatever follows is its fallout.
func (s *liveSession) recordPanic(err error) {
	s.mu.Lock()
	if s.panicErr == nil {
		s.panicErr = err
	}
	s.mu.Unlock()
	s.endWith(session.EndCancelled)
}

// watch ends the session on silence; the wall clock is the agent
// context's deadline.
func (s *liveSession) watch() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	// As the pump's: a panic here would leave the session's containers up
	// with nothing left to apply the idle or wall budgets.
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
	// Ordered against End's close of the channel: a send that loses the
	// race is dropped, never made on a closed channel. A blocked send
	// leaves when done closes, which End does before it closes events.
	s.eventsMu.RLock()
	defer s.eventsMu.RUnlock()
	if s.eventsClosed {
		return
	}
	select {
	case s.events <- ev:
	case <-s.done:
	}
}

// closeEvents closes the consumer's event channel once; an emit after it
// is dropped.
func (s *liveSession) closeEvents() {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	if !s.eventsClosed {
		s.eventsClosed = true
		close(s.events)
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
		stdin, exited := s.harness()
		if s.drv != nil {
			// stdin EOF: the harness exits by itself; the context kill is
			// the backstop.
			_ = stdin.Close()
		}
		go func() {
			select {
			case <-exited:
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
	s.inTurn = true
	s.mu.Unlock()
	// Every return clears it before the last event; this covers a panic
	// in the driver, so no later add waits on a turn that is gone.
	defer func() {
		s.mu.Lock()
		s.inTurn = false
		s.mu.Unlock()
	}()
	if len(notices) > 0 {
		text = strings.Join(notices, "\n") + "\n\n" + text
	}
	s.emit(session.Event{Kind: session.KindUserTurn, Text: text, Synthetic: synthetic})
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := s.restartOnRenewal(turnCtx); err != nil {
		if ended, _ := s.Ended(); ended {
			return session.ErrEnded
		}
		s.emit(session.Event{Kind: session.KindError, Text: err.Error()})
		s.endWith(session.EndHarness)
		return err
	}
	go func() {
		select {
		case <-s.done:
			cancel()
		case <-turnCtx.Done():
		}
	}()
	res, err := s.drv.Prompt(turnCtx, text)
	s.syncPump()
	// The turn is over before its last event is seen.
	s.mu.Lock()
	s.inTurn = false
	s.mu.Unlock()
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
		// receipt.
		s.lastText = s.redact.String(res.Text)
	}
	s.lastActivity = time.Now()
	s.mu.Unlock()
	s.emit(session.Event{Kind: session.KindAssistantTurn, Text: res.Text, StopReason: res.StopReason})
	return nil
}

// AddInput implements session.Session: only a regular host
// file, only under the staged inputs directory, agent-side mount
// unchanged (:ro). The agent hears about it as a user turn.
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
	unread, unpacked := "", false
	if s.dec != nil {
		// Decoded, then staged, then announced: the agent never hears of
		// a file whose text is not there yet.
		e, err := s.dec.add(ctx, base, data)
		if err != nil {
			return fmt.Errorf("decode input %s: %w", hostPath, err)
		}
		unread, unpacked = e.Unread, e.Unpacked
	} else if err := os.WriteFile(target, data, 0o644); err != nil { //nolint:gosec // inputs are agent-readable by design
		return fmt.Errorf("stage input %s: %w", hostPath, err)
	}
	sum := sha256.Sum256(data)
	agentPath := spec.InputsMount + "/" + base
	digest := "sha256:" + hex.EncodeToString(sum[:])
	s.emit(session.Event{Kind: session.KindInputAdded, Path: agentPath, Digest: digest, Text: base})
	notice := fmt.Sprintf("A new input file is available at %s (%s); read it with read_input.", agentPath, digest)
	switch {
	case unread != "":
		notice = fmt.Sprintf("A new input file was added at %s (%s), but it could not be read: %s.", agentPath, digest, unread)
	case unpacked:
		notice = fmt.Sprintf("A new archive is available at %s (%s) and was unpacked; list_inputs with path %q lists its files, and read_input reads each by its path.", agentPath, digest, base)
	}
	s.mu.Lock()
	ending, busy := s.ending, s.inTurn
	if busy && !ending {
		s.pendingNotices = append(s.pendingNotices, notice)
	}
	s.mu.Unlock()
	switch {
	case ending:
		return session.ErrEnded
	case busy:
		return nil
	}
	return s.turn(ctx, notice, true)
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
	s.ending = true
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
	_, exited := s.harness()
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
		s.cancel(errors.New("harness did not exit"))
		<-exited
	}
	if s.drv != nil {
		_ = s.drv.Close()
		// The driver's Close ends its event stream; wait for the pump to
		// process what was still buffered (the last turn's usage, a late
		// tool call) before the counters are read into the envelope.
		// Bounded: a wedged pump must not hang End.
		select {
		case <-s.pumpDone:
		case <-time.After(10 * time.Second):
		}
	}
	s.closeEvents()
	if s.eventsFile != nil {
		_ = s.eventsFile.Close()
	}
	if logs, lerr := s.d.Logs(ctx, containerName(s.runID, "gateway"), 2000); lerr == nil && logs != "" {
		_ = os.WriteFile(filepath.Join(s.rd.receiptsDir, "gateway.log"), []byte(s.redact.String(logs)), 0o644)
	}
	if logs, lerr := s.d.Logs(ctx, containerName(s.runID, "proxy"), 2000); lerr == nil && logs != "" {
		_ = os.WriteFile(filepath.Join(s.rd.receiptsDir, "proxy.log"), []byte(s.redact.String(logs)), 0o644)
	}
	copyHostLogs(s.rd, s.env)
	collectBrowseProxyLog(ctx, s.d, s.rd, s.env)
	copyBrowserOut(s.rd, s.p.Stderr)
	writeMeta(s.rd.receiptsDir, s.runID, s.p, s.start, "")
	daemon := daemonLine(s.rd)
	if s.rd.pdfLedger != "" {
		s.rd.redact.File(s.rd.pdfLedger)
	}
	if s.rd.fetchLedger != "" {
		s.rd.redact.File(s.rd.fetchLedger)
	}
	pdfs := pdfRecord(s.rd)
	fetchSaves := fetchSavesRecord(s.rd)

	s.mu.Lock()
	reason := s.reason
	log := append([]session.Event(nil), s.log...)
	lastText := s.lastText
	env := s.env
	env.Usage = receipt.Usage{InputTokens: s.inTok, OutputTokens: s.outTok, TotalTokens: s.totTok, CostUSD: s.costUSD, Turns: s.toolCalls, UserTurns: s.userTurns}
	tokensUnreported := !s.usageSeen && s.userTurns > 0
	harnessErr := s.harnessErr
	panicErr := s.panicErr
	planLimit := s.planLimit
	orgPolicy := s.orgPolicy
	outOfCredits := s.outOfCredits
	restarts := s.restarts
	s.mu.Unlock()
	env.Usage.WallMS = time.Since(s.start).Milliseconds()
	if restarts > 0 {
		env.CapabilitiesEnforced["model.signIn"] += fmt.Sprintf("; on the restart token route the harness was started again %d time(s) on a renewed token, each resuming the conversation", restarts)
	}
	env.CapabilitiesEnforced = withCostBudget(env.CapabilitiesEnforced, s.p.Budgets.MaxCostUSD, env.Usage.CostUSD)
	if tokensUnreported {
		env.CapabilitiesEnforced["usage.tokens"] = tokensUnreportedLine
	}
	env.CapabilitiesEnforced["daemon"] = daemon
	env.PDFs = pdfs
	env.FetchSaves = fetchSaves
	// Every decode the session made, the files added mid-run included.
	s.dec.lines(env.CapabilitiesEnforced)
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
	case session.EndPlanLimit:
		outErr = planLimitError(planLimit.Text, planLimit.ResetsAt)
	case session.EndOrgPolicy:
		outErr = orgPolicyError(orgPolicy)
	case session.EndOutOfCredits:
		outErr = outOfCreditsError(outOfCredits.Text)
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
		// went wrong.
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
// redacted) event log.
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

// tokensUnreportedLine is the receipt's line for a session whose harness
// reported no usage at all. docker-agent 1.150.0 sends its ACP usage_update
// only for a model whose context window it knows (its models.dev catalogue,
// or a provider's context_size), so a session on any other model, an
// operator's own endpoint among them, reports no tokens.
const tokensUnreportedLine = "the harness reported no token usage on its session stream, so usage.totalTokens is 0 because nothing was reported, not because the session used none"

// withCostBudget adds the receipt's line about the session's cost budget.
// A cost of 0 is ambiguous: the harness may have reported none, which is
// not the same as a session that cost nothing. And a ceiling asked for
// against such a harness could never have fired, so the line says that
// rather than leaving the control looking enforced. docker-agent sends
// `used` and no cost on its ACP usage_update (1.128.0 and 1.150.0 alike),
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
