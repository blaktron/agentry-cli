package engine

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/blaktron/agentry/pkg/manifest"

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
	if p.Plan == nil || !p.Plan.Conversational() || p.Plan.Harness.Session == nil {
		return nil, Classify(ExitCompile, errors.New("not a conversational plan (compile with Mode interactive, or persistent for a persistent agent)"))
	}
	// A persistent plan runs only as a segment of its agent, whose volumes
	// keep the conversation; a segment is only ever a persistent plan's.
	if (p.Plan.Mode == string(manifest.ModePersistent)) != (p.Persistent != nil) {
		return nil, Classify(ExitCompile, errors.New("a persistent plan runs only as a persistent agent's segment, and a segment only from a persistent plan"))
	}
	if p.Persistent != nil && (p.Persistent.Agent == "" || p.Persistent.SessionVolume == "" || p.Persistent.DataVolume == "") {
		return nil, Classify(ExitCLI, errors.New("a persistent agent's segment needs its agent's name and volumes"))
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
	if ps := p.Persistent; ps != nil {
		env.Persistent = &receipt.Persistent{
			Agent: ps.Agent, Segment: ps.Segment, ResumeReason: notStartedReason,
			Volumes: receipt.PersistentVolumes{Session: ps.SessionVolume, Data: ps.DataVolume, Memory: ps.Memory.VolumeName()},
			Share:   ps.Share, Roster: ps.Roster,
		}
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
	sweepAbandonedRun(ctx, d, p.RunRoot, p.RunnerHome, p.Stderr)
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
	ReportStage(p.OnStage, Stage{Name: receipt.StagePrepare})
	rd, err := prepareRunDirs(p, runID, td)
	if err != nil {
		return fail(err)
	}
	if rd.appLink != "" {
		env.CapabilitiesEnforced["persistent.appLink.transport"] = appLinkTransportLine(rd)
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
	if p.Persistent != nil {
		// A persistent agent's store is its own volume, made at its first
		// segment and kept: it carries no run label, so neither this
		// segment's teardown nor the sweep of a dead run removes it.
		rd.session = p.Persistent.SessionVolume
		if err := ensurePersistentVolumes(ctx, d, p); err != nil {
			return fail(Classify(ExitCLI, err))
		}
	} else {
		if err := d.VolumeCreate(ctx, rd.session, runLabels(runID)); err != nil {
			return fail(Classify(ExitCLI, fmt.Errorf("create session volume: %w", err)))
		}
		td.add(func(c context.Context) { _ = d.VolumeRemove(c, rd.session) })
	}
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
	if err := ensureImageServers(ctx, d, p); err != nil {
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
	env.CapabilitiesEnforced = withWithheldNote(env.CapabilitiesEnforced, withheldSecrets(p))
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
	ReportStage(p.OnStage, Stage{Name: receipt.StageInfra})
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
	s.granted, s.served = permitted(s.granted, served), served
	// The tools server runs: the Coding agent's ports are published into it.
	if err := startPortRelays(ctx, d, p, rd, td, runID); err != nil {
		return fail(err)
	}
	// Harness files after the infrastructure, as in Run: a host-run harness
	// needs the published addresses.
	rep = rd.replacer()
	if err := renderPlanFiles(p.Plan, p.PlanDir, rd, nativePath, "harness"); err != nil {
		return fail(err)
	}
	ReportStage(p.OnStage, Stage{Name: receipt.StageAgent, Subject: adapter.HarnessTitle(p.Plan.Harness.Adapter)})
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
	// served are the tools the gateway serves, as its probe listed them
	// once the infrastructure was up.
	served []string
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
	// conversationArgs are what the agent's argv gains at each start of a
	// persistent agent's segment whose harness names its conversation on
	// its command line (spec.SessionSpec.ConversationArgs).
	conversationArgs []string
	eventsFile       *os.File
	stderrPath       string

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
	// because one was in flight when the input arrived, and what the share
	// outbox's copy after a turn did (copyShareOut).
	pendingNotices []string
	// outboxMu holds one copy of the share outbox at a time: after a turn,
	// and at the end.
	outboxMu      sync.Mutex
	userTurns     int
	toolCalls     int
	inTok, outTok int64
	totTok        int64
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

	// shares is a persistent agent's check of its share (share.go), made
	// at the first turn that has something to check.
	shares *shareChecker
	// conversation, resumed and resumeReason are a persistent agent's
	// segment's (Conversation), set once its driver has started.
	conversation string
	resumed      bool
	resumeReason string
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

	restart := p.Plan.Harness.Session.RestartOnRenewal
	opts := sessionOptions(p, s.rd, token, s.granted)
	opts.Resumable = restart
	notContinued := ""
	if p.Persistent != nil {
		opts.Persistent, opts.ContinueID = true, p.Persistent.ContinueID
		// An id that goes on the harness's command line must be one the
		// engine chose, a UUID: anything else could be read as a flag.
		if ca := p.Plan.Harness.Session.ConversationArgs; ca != nil && opts.ContinueID != "" && uuid.Validate(opts.ContinueID) != nil {
			opts.ContinueID, notContinued = "", "the stored conversation's id is not one this harness names a conversation by"
		}
		s.nameConversation(&opts)
	}
	// A harness told on its command line to continue a conversation it
	// cannot load ends at once: its exit is the engine's to answer with a
	// new conversation (relaunch), not the session's end.
	relaunchable := opts.ContinueID != "" && s.conversationArgs != nil
	if relaunchable {
		s.setRestarting(true)
	}
	rw, err := s.launch(false)
	if err != nil {
		s.setRestarting(false)
		cancel(err)
		return err
	}
	// A failure from here on has a harness process running and writing
	// its stderr receipt; the goroutine launch started masks that receipt
	// once the process has exited, so the failure waits for it (bounded —
	// the cancel kills the process) before the receipt is handed over.
	awaitExit := func() {
		_, exited := s.harness()
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
	drv, err := p.Adapter.Session(agentCtx, rw, opts)
	if err != nil {
		if relaunchable {
			s.harnessDone()
		}
		cancel(err)
		awaitExit()
		return Classify(ExitHarness, fmt.Errorf("interactive session: %w", err))
	}
	s.setDriver(drv)
	// The handshake is where the harness loads the stored conversation,
	// which can take a while for a long one.
	if opts.ContinueID != "" {
		ReportStage(p.OnStage, Stage{Name: receipt.StageAgent, Detail: StageDetailResume, Subject: adapter.HarnessTitle(p.Plan.Harness.Adapter)})
	}
	handshake, cancelHS := context.WithTimeout(agentCtx, 120*time.Second)
	defer cancelHS()
	err = drv.Start(handshake)
	var nc *adapter.NotContinuedError
	if relaunchable && errors.As(err, &nc) {
		notContinued = nc.Reason
		drv, err = s.relaunchAnew(handshake, drv, &opts)
		if drv != nil {
			s.setDriver(drv)
		}
	} else if relaunchable {
		s.harnessDone()
	}
	if err != nil {
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
	if p.Persistent != nil {
		// The reason can quote the harness's own error: it is masked like
		// every receipt.
		id, resumed, reason := conversationOf(drv, p.Plan.Harness.Adapter, p.Persistent.ContinueID)
		if notContinued != "" {
			resumed, reason = false, notContinued
		}
		s.conversation, s.resumed, s.resumeReason = id, resumed, s.redact.String(reason)
		s.env.Persistent.Resumed, s.env.Persistent.ResumeReason = s.resumed, s.resumeReason
	}
	return nil
}

// nameConversation sets the flags that tell a harness started on its command
// line which conversation a persistent agent's segment is in: the stored
// one to continue, else a new one under an id chosen here, which the driver
// is given (adapter.SessionOptions.ConversationID).
func (s *liveSession) nameConversation(opts *adapter.SessionOptions) {
	ca := s.p.Plan.Harness.Session.ConversationArgs
	if ca == nil {
		return
	}
	if opts.ContinueID != "" {
		s.conversationArgs = []string{ca.Continue, opts.ContinueID}
		return
	}
	opts.ConversationID = uuid.NewString()
	s.conversationArgs = []string{ca.New, opts.ConversationID}
}

func (s *liveSession) setRestarting(on bool) {
	s.mu.Lock()
	s.restarting = on
	s.mu.Unlock()
}

// harnessDone ends the hold a relaunchable start put on the harness's exit:
// an exit that came while it held is the session's end after all. The exit
// goroutine reads the hold and closes exited under mu, so one of the two
// ends the session.
func (s *liveSession) harnessDone() {
	s.mu.Lock()
	s.restarting = false
	exited := s.exited
	s.mu.Unlock()
	select {
	case <-exited:
		s.endWith(session.EndHarness)
	default:
	}
}

// relaunchAnew starts the harness again on a new conversation after it
// could not load the stored one and ended (adapter.NotContinuedError): the
// old process's exit is awaited and its driver closed, and the new one is
// started with the id of a new conversation.
func (s *liveSession) relaunchAnew(ctx context.Context, old adapter.SessionDriver, opts *adapter.SessionOptions) (adapter.SessionDriver, error) {
	_, exited := s.harness()
	select {
	case <-exited:
	case <-ctx.Done():
		s.harnessDone()
		return nil, fmt.Errorf("the harness that could not continue the conversation did not end: %w", ctx.Err())
	}
	_ = old.Close()
	opts.ContinueID = ""
	s.nameConversation(opts)
	s.setRestarting(false)
	rw, err := s.launch(true)
	if err != nil {
		return nil, err
	}
	drv, err := s.p.Adapter.Session(s.agentCtx, rw, *opts)
	if err != nil {
		return nil, err
	}
	// The new harness gets a handshake's full time of its own.
	handshake, cancel := context.WithTimeout(s.agentCtx, 120*time.Second)
	defer cancel()
	return drv, drv.Start(handshake)
}

// noStoredConversation is the resume reason of a segment that was given no
// stored conversation to continue.
const noStoredConversation = "no stored conversation to continue"

// notStartedReason is a segment's resume reason until its harness has
// started: a segment that fails before then continued nothing.
const notStartedReason = "the segment ended before its harness started"

// conversationOf is the conversation a persistent agent's segment is in once
// its driver has started: the harness's id for it, whether it is the one
// continueID named, and why not when it is not.
func conversationOf(drv adapter.SessionDriver, adapterName, continueID string) (id string, resumed bool, reason string) {
	c, ok := drv.(adapter.Continuing)
	if !ok {
		return "", false, "the " + adapterName + " adapter does not continue a stored conversation in this version"
	}
	if continueID == "" {
		return c.ConversationID(), false, noStoredConversation
	}
	resumed, reason = c.Continued()
	return c.ConversationID(), resumed, reason
}

// ServedTools are the tools the session's gateway serves, by name: its own
// tools server's and every MCP server's in the plan, as the gateway listed
// them before the harness started.
func (s *liveSession) ServedTools() []string { return slices.Clone(s.served) }

// Conversation implements PersistentSession.
func (s *liveSession) Conversation() (id string, resumed bool, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conversation, s.resumed, s.resumeReason
}

// SetResumeReason implements PersistentSession.
func (s *liveSession) SetResumeReason(reason string) {
	reason = s.redact.String(reason)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resumeReason = reason
	if s.env.Persistent != nil {
		s.env.Persistent.ResumeReason = reason
	}
}

// PersistentSession is the session a persistent agent's segment returns
// (StartSession with Params.Persistent).
type PersistentSession interface {
	session.Session
	// Conversation is the harness's id for the conversation the segment
	// is in, whether it continued the one Params.Persistent.ContinueID
	// named, and why not when it did not: what the supervisor keeps for the
	// next segment, and what the envelope's persistent block records.
	Conversation() (id string, resumed bool, reason string)
	// SetResumeReason replaces the envelope's account of a segment that did
	// not continue its conversation, with what the supervisor did instead.
	SetResumeReason(reason string)
	// SendCarrying is Send with earlier context ahead of the person's text:
	// the harness is sent both, the user turn records the person's text
	// alone, and the events receipt records what was carried.
	SendCarrying(ctx context.Context, carried, text string) error
}

// PersistentLabel keys a persistent agent's volumes by the agent's name, and
// PersistentHomeLabel by the runner home they belong to. Neither is the run
// label: the volumes outlive every segment.
const (
	PersistentLabel     = "agentry.persistent"
	PersistentHomeLabel = "agentry.persistent.home"
)

// PersistentVolumeLabels are the labels a persistent agent's volumes carry:
// the agent's name and the runner home's identity.
func PersistentVolumeLabels(agent, home string) map[string]string {
	return map[string]string{PersistentLabel: agent, PersistentHomeLabel: home}
}

// ensurePersistentVolumes makes the agent's volumes that do not exist yet.
// An existing one is left as it is: it holds the agent's conversation and
// files. Listed first because an engine may refuse to create a volume that
// exists (podman does). A volume that does not exist, whose legacy volume
// does, is made as a copy of it. A memory volume, which has no legacy
// volume, is seeded when it is made, or when no start has seeded it yet
// (seedMemoryVolume), after the data volume it may copy a folder from.
func ensurePersistentVolumes(ctx context.Context, d dockerx.Client, p Params) error {
	ps := p.Persistent
	have, err := d.VolumesByLabel(ctx, PersistentHomeLabel+"="+ps.Home)
	if err != nil {
		return fmt.Errorf("list the volumes of the persistent agent %s: %w", ps.Agent, err)
	}
	legacy, err := d.VolumesByLabel(ctx, PersistentLabel+"="+ps.Agent)
	if err != nil {
		return fmt.Errorf("list the volumes of the persistent agent %s: %w", ps.Agent, err)
	}
	for _, v := range []struct{ name, old string }{{ps.SessionVolume, ps.LegacySessionVolume}, {ps.DataVolume, ps.LegacyDataVolume}} {
		if slices.Contains(have, v.name) {
			continue
		}
		if err := d.VolumeCreate(ctx, v.name, PersistentVolumeLabels(ps.Agent, ps.Home)); err != nil {
			return fmt.Errorf("create the volume %s of the persistent agent %s: %w", v.name, ps.Agent, err)
		}
		if v.old == "" || !slices.Contains(legacy, v.old) {
			continue
		}
		if err := copyPersistentVolume(ctx, d, p, v.old, v.name); err != nil {
			_ = d.VolumeRemove(context.WithoutCancel(ctx), v.name)
			return fmt.Errorf("carry the volume %s of the persistent agent %s over to %s: %w", v.old, ps.Agent, v.name, err)
		}
	}
	for _, v := range ps.ServerVolumes {
		if slices.Contains(have, v) {
			continue
		}
		if err := d.VolumeCreate(ctx, v, PersistentVolumeLabels(ps.Agent, ps.Home)); err != nil {
			return fmt.Errorf("create the volume %s of the persistent agent %s: %w", v, ps.Agent, err)
		}
	}
	m := ps.Memory
	if m == nil {
		return nil
	}
	made := !slices.Contains(have, m.Volume)
	if !made && m.Seeded {
		return nil
	}
	if made {
		if err := d.VolumeCreate(ctx, m.Volume, PersistentVolumeLabels(ps.Agent, ps.Home)); err != nil {
			return fmt.Errorf("create the volume %s of the persistent agent %s: %w", m.Volume, ps.Agent, err)
		}
	}
	if err := seedMemoryVolume(ctx, d, p); err != nil {
		if made {
			// Removed, so the next start makes and seeds it again.
			_ = d.VolumeRemove(context.WithoutCancel(ctx), m.Volume)
		}
		return fmt.Errorf("seed the memory volume %s of the persistent agent %s: %w", m.Volume, ps.Agent, err)
	}
	return nil
}

// VolumeName is the memory's volume; empty for none.
func (m *PersistentMemory) VolumeName() string {
	if m == nil {
		return ""
	}
	return m.Volume
}

// memorySeedScript fills a memory volume, at /memory, from the archive on
// its stdin, then, when the data volume's folder $1 is a folder that holds
// anything, copies it to $2 beneath the memory without following a link
// and keeps nothing but its files and folders, and appends each file of the
// carried folder ($3) to the memory's file of the same path. The carried
// folder is removed either way. A memory that holds SCHEMA.md is seeded
// already and is left as it is, its archive read and dropped.
const memorySeedScript = `set -eu
cd /memory
if [ -e SCHEMA.md ]; then
  cat >/dev/null
  exit 0
fi
tar -x -f -
if [ -n "$1" ] && [ -d "/data/$1" ] && [ ! -L "/data/$1" ] && [ -n "$(ls -A "/data/$1")" ]; then
  mkdir -p "$2"
  cp -RP "/data/$1/." "$2/"
  find "$2" ! -type f ! -type d -exec rm -f {} +
  if [ -d "$3" ]; then
    (cd "$3" && find . -type f) | while IFS= read -r f; do
      mkdir -p "$(dirname "$f")"
      cat "$3/$f" >> "$f"
    done
  fi
fi
rm -rf "$3"
`

// seedMemoryVolume fills the agent's memory volume with its seed
// (PersistentMemory.Seed), and the copy of its data volume's folder
// (PersistentMemory.Carry), in a one-shot of the runner image: no network, the data
// volume read-only, the archive on stdin, and DAC_READ_SEARCH alone, so
// the copy reads every file of the agent's own and owns what it writes.
func seedMemoryVolume(ctx context.Context, d dockerx.Client, p Params) error {
	ps, m := p.Persistent, p.Persistent.Memory
	archive, err := memorySeedArchive(m.Seed)
	if err != nil {
		return err
	}
	name := containerName(p.RunID, "memory-seed")
	s := hardened(dockerx.RunSpec{
		Name:       name,
		Image:      p.Plan.Proxy.Image,
		Entrypoint: "sh",
		Argv:       []string{"-c", memorySeedScript, "sh", m.Carry, m.CarryTo, MemoryCarriedDir},
		User:       "0:0",
		Network:    "none",
		Mounts: []dockerx.Mount{
			{Source: ps.DataVolume, Target: "/data", ReadOnly: true},
			{Source: m.Volume, Target: "/memory"},
		},
		Labels:     runLabels(p.RunID),
		AutoRemove: true,
		Runtime:    p.Plan.Runtime(),
	}, p.limits())
	s.Interactive = true
	s.CapAdd = []string{"DAC_READ_SEARCH"}
	defer func() { _ = d.RemoveForce(context.WithoutCancel(ctx), name) }()
	var stderr bytes.Buffer
	code, err := dockerx.RunForegroundUnder(ctx, d, s, "memory volume's seed", bytes.NewReader(archive), io.Discard, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("the seed exited %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// memorySeedArchive is the seed as a tar archive, in path order: each
// file or folder with its folders before it, owned by root, the files 0644 and the
// folders 0755. A path that is not a clean relative one is refused.
func memorySeedArchive(seed map[string][]byte) ([]byte, error) {
	paths := slices.Sorted(maps.Keys(seed))
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	made := map[string]bool{}
	for _, p := range paths {
		folder := strings.HasSuffix(p, "/")
		if p = strings.TrimSuffix(p, "/"); !fs.ValidPath(p) || p == "." {
			return nil, fmt.Errorf("the memory's seed has a path %q that is not a clean relative one", p)
		}
		var dirs []string
		if folder {
			dirs = append(dirs, p)
		}
		for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
			dirs = append(dirs, dir)
		}
		slices.Reverse(dirs)
		for _, dir := range dirs {
			if made[dir] {
				continue
			}
			made[dir] = true
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir + "/", Mode: 0o755}); err != nil {
				return nil, err
			}
		}
		if folder {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: p, Mode: 0o644, Size: int64(len(seed[p]))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(seed[p]); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// copyPersistentVolume copies the files of the volume from into the volume
// to, which is empty, in a one-shot of the harness image: no network, a
// read-only root, the old volume read-only, and the capabilities cp -a needs
// to keep owners, modes and times. The old volume is not changed.
func copyPersistentVolume(ctx context.Context, d dockerx.Client, p Params, from, to string) error {
	name := containerName(p.RunID, "volume-copy")
	s := hardened(dockerx.RunSpec{
		Name:       name,
		Image:      p.Plan.Harness.Agent.Image,
		Entrypoint: "sh",
		Argv:       []string{"-c", `cp -a /from/. /to/`},
		User:       "0:0",
		Network:    "none",
		Mounts: []dockerx.Mount{
			{Source: from, Target: "/from", ReadOnly: true},
			{Source: to, Target: "/to"},
		},
		Labels:     runLabels(p.RunID),
		AutoRemove: true,
		Runtime:    p.Plan.Runtime(),
	}, p.limits())
	s.CapAdd = []string{"CHOWN", "DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER"}
	defer func() { _ = d.RemoveForce(context.WithoutCancel(ctx), name) }()
	var stderr bytes.Buffer
	code, err := dockerx.RunForegroundUnder(ctx, d, s, "persistent volume's copy", nil, io.Discard, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("the copy exited %d: %s", code, strings.TrimSpace(stderr.String()))
	}
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
			Argv:        append(substituteAll(p.Plan.Harness.Agent.Argv, rep), s.conversationArgs...),
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
		close(exited)
		s.mu.Unlock()
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
	case adapter.EventToolResult:
		// A session's transcript keeps the result's raw line; its text
		// says only what the event is, never what the tool returned.
		s.emit(session.Event{Kind: session.KindOther, Text: string(ev.Kind), Raw: ev.Raw})
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

// setDriver sets the session's driver under mu: a harness that exits while
// the session starts ends it (endWith) on the goroutine that watched it.
func (s *liveSession) setDriver(drv adapter.SessionDriver) {
	s.mu.Lock()
	s.drv = drv
	s.mu.Unlock()
}

// endWith marks the session over for reason (first reason wins), stops
// the harness, and closes the event stream after the session_end event.
func (s *liveSession) endWith(reason session.EndReason) {
	s.endOnce.Do(func() {
		s.mu.Lock()
		s.ended, s.reason = true, reason
		started := s.drv != nil
		s.mu.Unlock()
		s.emit(session.Event{Kind: session.KindSessionEnd, Reason: reason})
		close(s.done)
		stdin, exited := s.harness()
		if started {
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

// SendCarrying implements PersistentSession.
func (s *liveSession) SendCarrying(ctx context.Context, carried, text string) error {
	return s.turnCarrying(ctx, carried, text, false)
}

func (s *liveSession) turn(ctx context.Context, text string, synthetic bool) error {
	return s.turnCarrying(ctx, "", text, synthetic)
}

// turnCarrying is one turn whose prompt is carried, then text; the user turn
// records text, and a carried context is recorded as an event of its own.
func (s *liveSession) turnCarrying(ctx context.Context, carried, text string, synthetic bool) error {
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
	if carried != "" {
		s.emit(session.Event{Kind: session.KindOther, Text: "carried into this turn: " + carried})
	}
	s.emit(session.Event{Kind: session.KindUserTurn, Text: text, Synthetic: synthetic})
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// A persistent agent's share is checked for what was added to it since
	// the last turn, before the harness can read it.
	s.checkShare(turnCtx)
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
	s.mu.Lock()
	callsBefore := s.toolCalls
	s.mu.Unlock()
	res, err := promptAtCapacity(turnCtx, s.drv, carried+text, func(n int) bool {
		// The failed attempt's events go ahead of the notice.
		s.syncPump()
		s.mu.Lock()
		called := s.toolCalls != callsBefore
		s.mu.Unlock()
		// The session's own wall and idle limits bound the wait as the
		// run's timeout bounds an autonomous run's.
		idle := s.budgets.IdleTimeoutSeconds > 0 && time.Duration(s.budgets.IdleTimeoutSeconds)*time.Second <= capacityWait
		if called || idle || !canWaitAtCapacity(s.agentCtx) {
			return false
		}
		s.emit(session.Event{Kind: session.KindNotice, Text: capacityNotice(n)})
		// Waiting out the provider is the session at work, not idle.
		s.mu.Lock()
		s.lastActivity = time.Now()
		s.mu.Unlock()
		return true
	})
	s.syncPump()
	// What the servers that see the share saved during the turn reaches
	// the share before the turn is over.
	s.copyShareOut(context.WithoutCancel(ctx))
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
	// The harness is gone, so the servers save nothing more.
	s.copyShareOut(context.WithoutCancel(ctx))
	writeMeta(s.rd.receiptsDir, s.runID, s.p, s.start, "")
	daemon := daemonLine(s.rd)
	if s.rd.pdfLedger != "" {
		s.rd.redact.File(s.rd.pdfLedger)
	}
	if s.rd.fetchLedger != "" {
		s.rd.redact.File(s.rd.fetchLedger)
	}
	if s.rd.deliverablesLedger != "" {
		s.rd.redact.File(s.rd.deliverablesLedger)
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
	if outErr == nil && env.Status == "ok" {
		// A session's last message is a turn of a chat, never a file the
		// runner writes down: only marked files and the runner's pick of
		// reports/ are listed, once teardown has stopped the containers.
		finishDeliverables(ctx, s.d, env, s.rd, s.p, "")
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
	mode := cmp.Or(env.Harness.Mode, "interactive")
	fmt.Fprintf(&b, "# Transcript: %s\n\nrun %s · %s · %s\n\n", env.Ref, env.RunID, mode, env.Harness.Model)
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
