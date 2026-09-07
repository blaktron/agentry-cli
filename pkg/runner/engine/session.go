package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
		CapabilitiesEnforced: p.Plan.Enforcement,
		Harness: receipt.Harness{
			Adapter: p.Plan.Harness.Adapter,
			Image:   p.Plan.Harness.Agent.Image,
			Model:   p.Plan.Harness.Model,
			Mode:    p.Plan.Mode,
		},
		Result: receipt.TextResult(""),
		OutDir: p.OutDir,
	}
	planHash, err := p.Plan.Hash()
	if err != nil {
		return nil, Classify(ExitCLI, err)
	}
	receiptsDir := filepath.Join(p.OutDir, "receipts")
	env.Receipts = receipt.Receipts{Dir: receiptsDir, RunPlanSHA256: planHash}

	td := &teardown{d: d, stderr: p.Stderr, runID: runID}
	s := &liveSession{
		d: d, p: p, env: env, td: td, start: start, runID: runID,
		events:  make(chan session.Event, 1024),
		done:    make(chan struct{}),
		budgets: p.Plan.Harness.Session.Budgets,
	}
	fail := func(err error) (session.Session, error) {
		td.run()
		_, cerr := fail(env, err)
		return nil, cerr
	}

	rd, err := prepareRunDirs(p, runID, td)
	if err != nil {
		return fail(err)
	}
	s.rd = rd
	// The harness image runs unprivileged (user docker-agent) and must
	// write its session store into this host-owned directory; a per-run
	// named volume is the follow-up (spike, agentry-cli#33).
	if err := os.Chmod(rd.session, 0o777); err != nil { //nolint:gosec // the harness user must write here; the dir holds only its SQLite store
		return fail(Classify(ExitCLI, fmt.Errorf("session dir: %w", err)))
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
	s.redact = newRedactor(token, secrets, hostSecretValues(p.Plan.Harness.Agent.Env))

	if err := startInfra(ctx, d, p, rd, td, runID, token, rep); err != nil {
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

	events chan session.Event
	done   chan struct{} // closed once (ended)

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
	costUSD        float64
	lastText       string
	harnessErr     string
	lastActivity   time.Time
	endOnce        sync.Once
	envelope       *receipt.Envelope
	envelopeErr    error
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
	}
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
	go func() {
		code, rerr := s.d.RunForeground(agentCtx, runSpec, stdinR, stdoutW, stderrFile)
		_ = stdoutW.Close()
		_ = stderrFile.Close()
		s.mu.Lock()
		s.exitCode = code
		if rerr != nil {
			s.harnessErr = rerr.Error()
		}
		s.mu.Unlock()
		close(s.exited)
		s.endWith(session.EndHarness)
	}()
	drv, err := p.Adapter.Session(agentCtx, rwPipe{Reader: stdoutR, WriteCloser: stdinW}, adapter.SessionOptions{GatewayTools: p.Plan.Tools})
	if err != nil {
		cancel(err)
		return Classify(ExitHarness, fmt.Errorf("interactive session: %w", err))
	}
	s.drv = drv
	handshake, cancelHS := context.WithTimeout(agentCtx, 120*time.Second)
	defer cancelHS()
	if err := drv.Start(handshake); err != nil {
		cancel(err)
		return Classify(ExitHarness, fmt.Errorf("interactive session: %w%s", err, stderrTail(s.stderrPath)))
	}
	s.lastActivity = time.Now()
	return nil
}

// pump turns adapter events into session events and applies the tool and
// cost budgets exactly as the autonomous collector does.
func (s *liveSession) pump() {
	for ev := range s.drv.Events() {
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
			if ev.CostUSD > s.costUSD {
				s.costUSD = ev.CostUSD
			}
			over := s.p.Budgets.MaxCostUSD > 0 && s.costUSD > s.p.Budgets.MaxCostUSD
			s.mu.Unlock()
			s.emit(session.Event{Kind: session.KindUsage, InputTokens: ev.InputTokens, OutputTokens: ev.OutputTokens, CostUSD: ev.CostUSD, Raw: ev.Raw})
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
}

// watch ends the session on silence (plan §6.7); the wall clock is the
// agent context's deadline.
func (s *liveSession) watch() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
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
	}
	close(s.events)
	if s.eventsFile != nil {
		_ = s.eventsFile.Close()
	}
	if logs, lerr := s.d.Logs(ctx, containerName(s.runID, "gateway"), 2000); lerr == nil && logs != "" {
		_ = os.WriteFile(filepath.Join(s.rd.receiptsDir, "gateway.log"), []byte(s.redact.String(logs)), 0o644)
	}
	writeMeta(s.rd.receiptsDir, s.runID, s.p, s.start)

	s.mu.Lock()
	reason := s.reason
	log := append([]session.Event(nil), s.log...)
	lastText := s.lastText
	env := s.env
	env.Usage = receipt.Usage{InputTokens: s.inTok, OutputTokens: s.outTok, CostUSD: s.costUSD, Turns: s.toolCalls, UserTurns: s.userTurns}
	harnessErr := s.harnessErr
	s.mu.Unlock()
	env.Usage.WallMS = time.Since(s.start).Milliseconds()
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
	if outErr != nil {
		env.Result = receipt.TextResult(lastText)
		env, outErr = fail(env, outErr)
	} else {
		env.Status = "ok"
		env.ExitCode = ExitOK
		if err := env.Write(s.p.OutDir); err != nil {
			_, _ = fmt.Fprintf(s.p.Stderr, "warning: %v\n", err)
		}
	}
	s.td.run()
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

// redactor masks the values that must never reach a receipt: the run's
// gateway token, every resolved secret value and the model API key
// (§R11 invariant 7).
type redactor struct{ values []string }

func newRedactor(token string, secrets map[string]string, extra []string) *redactor {
	var vals []string
	add := func(v string) {
		if len(v) >= 4 {
			vals = append(vals, v)
		}
	}
	add(token)
	for _, v := range secrets {
		add(v)
	}
	for _, v := range extra {
		add(v)
	}
	// Longest first so a value that contains another is masked whole.
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	return &redactor{values: vals}
}

// String masks every known value in s.
func (r *redactor) String(s string) string {
	if r == nil {
		return s
	}
	for _, v := range r.values {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	return s
}

// hostSecretValues collects the host-environment values the plan injects
// into the agent container (the model API key) for redaction.
func hostSecretValues(vars []spec.EnvVar) []string {
	var out []string
	for _, v := range vars {
		if v.FromHostEnv != "" {
			if val := os.Getenv(v.FromHostEnv); val != "" {
				out = append(out, val)
			}
		}
	}
	return out
}
