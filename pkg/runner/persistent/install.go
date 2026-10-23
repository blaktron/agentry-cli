package persistent

// A server the person adds to a running agent, or that a start finds the
// agent's store does not hold, is installed in the supervisor's background
// while the agent runs without it. Each install holds the server's store
// lock, so it is the store's one writer for that server; the segment's
// gateway reads the other servers' directories meanwhile. When the install
// finishes the server is ready, which changes the agent's grants, so the
// next turn starts the segment that has it, as any roster change does, and
// that turn tells the agent it joined and names its tools. A failed install
// is recorded on the server's entry and told once; the agent keeps running.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/osvdb"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// InstallServer installs one package server into a persistent agent's store,
// storeDir, writing its progress to logw (localrun.InstallServer).
type InstallServer func(ctx context.Context, srv manifest.MCPServer, storeDir string, logw io.Writer) error

// installWait bounds how long a stop waits for the background installs it
// cancelled to remove their containers: longer than an install's own
// teardown, whose engine calls are bounded.
const installWait = 90 * time.Second

// The bounds of what a failure records and what the agent is told of it.
const (
	maxFailureReason = 600
	maxToldReason    = 200
	tailLines        = 6
	maxTailBytes     = 8 << 10
	maxToldTools     = 40
)

// bgInstall is one background install under way.
type bgInstall struct {
	cancel context.CancelFunc
}

// startInstalls cancels the installs of the servers the agent no longer has
// on, then starts one for each server it has on, agreed to as it reads,
// that the store does not hold, whose install has not failed and is not
// already under way. The installs run until they finish, their server is
// switched off or removed, or the agent stops.
func (s *Supervisor) startInstalls() {
	a, home := s.opts.Agent, s.opts.Home
	cfg, err := LoadConfig(home, a.Name)
	if err != nil {
		return
	}
	r := BuildRoster(a, cfg, nil)
	// Only what the person agreed to is installed: an entry turned on some
	// other way is neither installed nor given to a segment.
	unagreed := map[string]bool{}
	for _, e := range Unconsented(r, cfg) {
		unagreed[e.ID] = true
	}
	storeDir := serversDir(home, a.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Under mu, as cancelInstalls cancels: once the stop has cancelled the
	// installs, none is added for it to miss.
	if s.stopped || s.bgCtx.Err() != nil {
		return
	}
	for id, in := range s.installs {
		if e, ok := r.Entry(id); !ok || !e.On {
			in.cancel()
			delete(s.installs, id)
		}
	}
	// A server switched off or removed after its install finished has not
	// joined: switched on again, it is a switch like any other.
	for id := range s.joined {
		if e, ok := r.Entry(id); !ok || !e.On {
			delete(s.joined, id)
		}
	}
	for _, as := range agentServers(a, cfg) {
		srv, id := as.Server, as.ID()
		e, ok := r.Entry(id)
		_, failed := cfg.ServerFailures[id]
		if !ok || !e.On || unagreed[id] || failed || srv.IsRemote() || s.installs[id] != nil || mcpinstall.Installed(storeDir, srv) {
			continue
		}
		ctx, cancel := context.WithCancel(s.bgCtx)
		in := &bgInstall{cancel: cancel}
		s.installs[id] = in
		s.installWG.Add(1)
		go s.install(ctx, in, srv, storeDir)
	}
}

// install installs srv into the agent's store under its store lock, and
// records what came of it: the server joins at the next turn, or its entry
// is failed with the reason. A cancelled install records nothing.
func (s *Supervisor) install(ctx context.Context, in *bgInstall, srv manifest.MCPServer, storeDir string) {
	id, name := ServerEntryID(srv), serverName(srv)
	defer func() {
		in.cancel()
		s.mu.Lock()
		if s.installs[id] == in {
			delete(s.installs, id)
		}
		s.mu.Unlock()
		s.installWG.Done()
	}()
	logw := s.opts.Stderr
	if logw == nil {
		logw = io.Discard
	}
	engine.ReportStage(s.opts.OnStage, Stage{Name: receipt.StageMCPServers, Subject: mcpinstall.PackageOf(srv)})
	_, _ = fmt.Fprintf(logw, "installing the MCP server %s in the background; the agent runs without it until it is ready\n", name)
	s.notice("Installing the MCP server " + name + " in the background: the agent keeps running, and the server joins at your first message after it is ready")
	unlock, err := mcpinstall.LockServer(ctx, storeDir, mcpinstall.IDFor(srv))
	if err == nil {
		tail := &lineTail{}
		do := s.opts.InstallServer
		if do == nil {
			do = func(ctx context.Context, srv manifest.MCPServer, storeDir string, logw io.Writer) error {
				return localrun.InstallServer(ctx, s.opts.Docker, s.opts.Policy, storeDir, srv, logw)
			}
		}
		err = do(ctx, srv, storeDir, io.MultiWriter(logw, tail))
		if err == nil && ctx.Err() == nil {
			// Joined before the lock is let go, which is when the server
			// becomes ready: a turn that restarts for it tells it joined.
			s.mu.Lock()
			s.joined[id] = true
			s.mu.Unlock()
		}
		unlock()
		if err != nil {
			err = withTail(err, tail)
		}
	}
	if ctx.Err() != nil {
		_, _ = fmt.Fprintf(logw, "the install of the MCP server %s was cancelled\n", name)
		return
	}
	if err != nil {
		reason := failureReason(err.Error())
		_, _ = fmt.Fprintf(logw, "the MCP server %s could not be installed: %s\n", name, reason)
		if s.recordFailure(id, reason) {
			s.notice("The MCP server " + name + " could not be installed, and the agent keeps running without it: " + reason + ". Switch it off and on to try again.")
		}
		return
	}
	_, _ = fmt.Fprintf(logw, "the MCP server %s is installed; it joins at the next turn\n", name)
	s.notice("The MCP server " + name + " is installed: it joins at your next message")
}

// recordFailure records that the server id could not be installed or
// started, when the agent still has it on; it answers whether it did.
func (s *Supervisor) recordFailure(id, reason string) bool {
	recorded := false
	_ = s.updateConfig(func(c *Config) error {
		if e, ok := BuildRoster(s.opts.Agent, *c, nil).Entry(id); !ok || !e.On {
			return nil
		}
		if c.ServerFailures == nil {
			c.ServerFailures = map[string]ServerFailure{}
		}
		c.ServerFailures[id] = ServerFailure{Reason: reason, At: time.Now().UTC()}
		recorded = true
		return nil
	})
	if recorded {
		s.mu.Lock()
		delete(s.joined, id)
		s.mu.Unlock()
	}
	return recorded
}

// notice tells every chat attached something about the agent that is not
// the agent's own: a session notice, never sent to the harness.
func (s *Supervisor) notice(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.broadcast(session.Event{Kind: session.KindNotice, Time: time.Now().UTC(), Text: text})
	}
}

// cancelInstalls cancels every background install, and no more start.
func (s *Supervisor) cancelInstalls() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bgCancel()
}

// waitInstalls waits, a bounded time, for the cancelled installs to take
// down what they started.
func (s *Supervisor) waitInstalls() {
	done := make(chan struct{})
	go func() {
		s.installWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(installWait):
	}
}

// installing are the ids of the servers being installed now.
func (s *Supervisor) installing() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.installs))
	for id := range s.installs {
		out[id] = true
	}
	return out
}

// servedTools are the tools a segment's gateway serves, when its session
// says.
func servedTools(sess engine.PersistentSession) []string {
	if st, ok := sess.(interface{ ServedTools() []string }); ok {
		return st.ServedTools()
	}
	return nil
}

// joinFrame is what the first turn of a segment that servers joined starts
// with: the servers, by their roster names, and the tools the segment's
// gateway serves that the one before did not, each quoted. restarted adds
// what a restart did that the change's own frame has not said. "" when no
// server joined.
func joinFrame(roster Roster, joined map[string]bool, tools []string, restarted string) string {
	var names []string
	for _, e := range roster {
		if joined[e.ID] && e.State == StateOn {
			names = append(names, strconv.Quote(e.Name))
		}
	}
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	if len(names) == 1 {
		b.WriteString("(An MCP server the person added finished installing and is yours from this turn: " + names[0] + ".")
	} else {
		b.WriteString("(MCP servers the person added finished installing and are yours from this turn: " + strings.Join(names, ", ") + ".")
	}
	switch {
	case len(tools) == 0:
		b.WriteString(" Its tools are among yours now.")
	default:
		quoted := make([]string, 0, min(len(tools), maxToldTools))
		for _, t := range tools[:min(len(tools), maxToldTools)] {
			quoted = append(quoted, strconv.Quote(t))
		}
		b.WriteString(" The tools it brings: " + strings.Join(quoted, ", "))
		if len(tools) > maxToldTools {
			fmt.Fprintf(&b, ", and %d more", len(tools)-maxToldTools)
		}
		b.WriteString(".")
	}
	if restarted != "" {
		b.WriteString(" " + restarted)
	}
	b.WriteString(")\n\n")
	return b.String()
}

// failFrame is what a turn starts with when servers the agent has on could
// not be installed, or could not start and it has not been told: each by its roster
// name and the first line of why, quoted. "" when there is none.
func failFrame(roster Roster, failures map[string]ServerFailure) string {
	var names, whys []string
	for _, e := range roster {
		f, ok := failures[e.ID]
		if !ok || f.Told || e.State != StateFailed {
			continue
		}
		names = append(names, strconv.Quote(e.Name))
		whys = append(whys, strconv.Quote(cut(f.Reason, maxToldReason)))
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return "(Agentry could not give you the MCP server " + names[0] + " the person added, so you do not have it. Why: " + whys[0] +
			". The person sees this in Agentry and can try again.)\n\n"
	}
	for i := range whys {
		whys[i] = names[i] + ": " + whys[i]
	}
	return "(Agentry could not give you the MCP servers " + strings.Join(names, ", ") + " the person added, so you do not have them. Why: " +
		strings.Join(whys, "; ") + ". The person sees this in Agentry and can try again.)\n\n"
}

// newTools are the names in now that were not in before.
func newTools(before, now []string) []string {
	var out []string
	for _, t := range now {
		if !slices.Contains(before, t) && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// lineTail keeps the last lines written to it.
type lineTail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *lineTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - maxTailBytes; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

// lines are the last n lines that are not blank.
func (t *lineTail) lines(n int) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for _, l := range bytes.Split(t.buf, []byte("\n")) {
		if l = bytes.TrimSpace(l); len(l) > 0 {
			out = append(out, string(l))
		}
	}
	return out[max(len(out)-n, 0):]
}

// withTail adds the builder's last lines to a failed install's error, for
// an install that ran its builder.
func withTail(err error, tail *lineTail) error {
	if r := receipt.ReasonOf(err); r == nil || r.Detail != receipt.InstallBuilder {
		return err
	}
	if lines := tail.lines(tailLines); len(lines) > 0 {
		return fmt.Errorf("%w; the builder's last lines: %s", err, strings.Join(lines, " | "))
	}
	return err
}

// failureReason is an install's or a start's error as a failure records it:
// one line of printable text, cut. It comes in part from a package's
// builder and a registry, so every control and format character (a
// terminal's escape, a bidirectional override) is replaced before a
// terminal, a chat or the agent is shown it.
func failureReason(s string) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
	return cut(strings.Join(strings.Fields(clean), " "), maxFailureReason)
}

// hadOn says the roster's entry id was given: on, in a segment.
func hadOn(r Roster, id string) bool {
	e, ok := r.Entry(id)
	return ok && e.State == StateOn
}

// cut is s at most n bytes long, cut at a rune and marked.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// refreshDependencyDB keeps OSV's databases up to date in the background
// while the agent runs: it asks at once and then every hour, and an archive
// is downloaded again once a day has passed (osvdb.Ensure). One refresher
// at a time, ended by the stop as an install is; the scan reads what is
// there, each archive replaced only once a new one is whole.
func (s *Supervisor) refreshDependencyDB() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.osvRefreshing || s.stopped || s.bgCtx.Err() != nil {
		return
	}
	s.osvRefreshing = true
	s.installWG.Add(1)
	go func() {
		defer s.installWG.Done()
		logw := s.opts.Stderr
		if logw == nil {
			logw = io.Discard
		}
		defer func() {
			s.mu.Lock()
			s.osvRefreshing = false
			s.mu.Unlock()
		}()
		tick := time.NewTicker(dependencyDBCheck)
		defer tick.Stop()
		for {
			osvdb.Ensure(s.bgCtx, s.opts.Home, logw)
			select {
			case <-s.bgCtx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}

// dependencyDBCheck is how often a running agent asks whether OSV's
// databases are due.
var dependencyDBCheck = time.Hour
