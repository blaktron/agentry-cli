package persistent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// wordServer is a package server, which the agent's store must hold before
// a segment is given it.
var wordServer = manifest.MCPServer{PyPI: "office-word-mcp-server", Version: "1.1.11"}

// record writes the store's record of srv, as a finished install leaves it.
func record(t *testing.T, storeDir string, srv manifest.MCPServer) {
	t.Helper()
	id := mcpinstall.IDFor(srv)
	if err := os.MkdirAll(filepath.Join(storeDir, id), 0o700); err != nil {
		t.Error(err)
		return
	}
	rec := fmt.Sprintf(`{"source":"pypi","package":%q,"version":%q,"id":%q,"binName":"word_mcp_server"}`, mcpinstall.PackageOf(srv), srv.Version, id)
	if err := os.WriteFile(filepath.Join(storeDir, id+".json"), []byte(rec), 0o600); err != nil {
		t.Error(err)
	}
}

// installer is a background install a test drives: each call waits for a
// release (or its context), then installs, or fails with fail.
type installer struct {
	t       *testing.T
	mu      sync.Mutex
	calls   int
	release chan struct{}
	fail    error
	// cancelled counts the calls whose context ended first.
	cancelled int
	// lines are written to the install's log, as a builder's are.
	lines string
}

func newInstaller(t *testing.T) *installer {
	return &installer{t: t, release: make(chan struct{}, 8)}
}

func (in *installer) install(ctx context.Context, srv manifest.MCPServer, storeDir string, logw io.Writer) error {
	in.mu.Lock()
	in.calls++
	lines, fail := in.lines, in.fail
	in.mu.Unlock()
	select {
	case <-in.release:
	case <-ctx.Done():
		in.mu.Lock()
		in.cancelled++
		in.mu.Unlock()
		return ctx.Err()
	}
	_, _ = io.WriteString(logw, lines)
	if fail != nil {
		return fail
	}
	record(in.t, storeDir, srv)
	return nil
}

func (in *installer) count() (calls, cancelled int) {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.calls, in.cancelled
}

// startWith starts the Assistant with st's segments and in's installs; the
// segments' gateways serve tools, each given its servers' tools as
// "<server>_tool".
func startWith(t *testing.T, home string, st *starter, in *installer) *Supervisor {
	t.Helper()
	sv, err := Start(context.Background(), Options{
		Home: home, Agent: assistantAgent(t), Client: ClientCLI, Policy: policy.Default(),
		Docker: &dockerx.Fake{}, StartSegment: st.start, InstallServer: in.install,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _, _ = sv.Stop(context.Background()) })
	return sv
}

// waitFor waits for the condition, failing the test after a few seconds.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func entryState(sv *Supervisor, id string) EntryState {
	e, _ := sv.Roster().Entry(id)
	return e.State
}

// addWord adds the Word server to a stopped agent, agreed to.
func addWord(t *testing.T, home string) {
	t.Helper()
	c, err := LoadConfig(home, "assistant")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyChange(home, assistantAgent(t), &c, Change{AddServer: &wordServer, Consent: true}); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(home, "assistant", c); err != nil {
		t.Fatal(err)
	}
}

// TestStartSkipsAServerBeingInstalled: a start whose server's store lock
// another holder has leaves the server out of the segment, its envelope's
// roster saying it is installing; the agent's own install waits for the lock
// and, the store's one writer, installs once the lock is let go.
func TestStartSkipsAServerBeingInstalled(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	addWord(t, home)
	store := serversDir(home, "assistant")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := mcpinstall.LockServer(t.Context(), store, mcpinstall.IDFor(wordServer))
	if err != nil {
		t.Fatal(err)
	}
	st, in := &starter{resumed: true}, newInstaller(t)
	in.release <- struct{}{}
	sv := startWith(t, home, st, in)
	first := st.segments[0]
	if len(first.Grants.Servers) != 0 {
		t.Fatalf("the segment was given %v while its install was under way", first.Grants.Servers)
	}
	if !slices.ContainsFunc(first.Persistent.Roster, func(e receipt.RosterEntry) bool {
		return e.ID == ServerEntryID(wordServer) && e.On && e.State == string(StateInstalling)
	}) {
		t.Errorf("the envelope's roster %+v does not say the server is installing", first.Persistent.Roster)
	}
	if got := entryState(sv, ServerEntryID(wordServer)); got != StateInstalling {
		t.Errorf("the roster says %s, want installing", got)
	}
	time.Sleep(300 * time.Millisecond)
	if calls, _ := in.count(); calls != 0 {
		t.Fatal("the agent's install wrote the store while another holder had its lock")
	}
	unlock()
	waitFor(t, "the install", func() bool { c, _ := in.count(); return c == 1 })
	waitFor(t, "the server to be ready", func() bool { return entryState(sv, ServerEntryID(wordServer)) == StatePending })
	if _, err := sv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Installed, and its lock held again (another writer at it): the next
	// start leaves it out all the same, and installs nothing.
	unlock, err = mcpinstall.LockServer(t.Context(), store, mcpinstall.IDFor(wordServer))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	st2, in2 := &starter{resumed: true}, newInstaller(t)
	startWith(t, home, st2, in2)
	if len(st2.segments[0].Grants.Servers) != 0 {
		t.Errorf("an installed server whose lock is held was given: %v", st2.segments[0].Grants.Servers)
	}
	time.Sleep(100 * time.Millisecond)
	if calls, _ := in2.count(); calls != 0 {
		t.Errorf("an installed server was installed again (%d)", calls)
	}
}

// TestOnlyAnAgreedServerIsInstalled: a server turned on in config.yaml some
// other way than a change the person agreed to is never installed, while
// the agent runs or at a start.
func TestOnlyAnAgreedServerIsInstalled(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st, in := &starter{}, newInstaller(t)
	sv := startWith(t, home, st, in)
	c, _ := LoadConfig(home, "assistant")
	c.Servers = append(c.Servers, wordServer)
	if err := SaveConfig(home, "assistant", c); err != nil {
		t.Fatal(err)
	}
	name := "Sandra"
	if _, err := sv.Change(Change{Name: &name}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if calls, _ := in.count(); calls != 0 {
		t.Errorf("an unagreed server was installed (%d)", calls)
	}
	var ce *ConsentError
	if _, err := sv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(context.Background(), Options{Home: home, Agent: assistantAgent(t), Client: ClientCLI, Policy: policy.Default(),
		StartSegment: st.start, InstallServer: in.install}); !errors.As(err, &ce) {
		t.Errorf("a start with an unagreed server: %v", err)
	}
}

// TestAFailureReasonIsOnePrintableLine: what a builder or a registry wrote
// reaches a terminal, a chat and the agent as one line, with no escape and
// no bidirectional override.
func TestAFailureReasonIsOnePrintableLine(t *testing.T) {
	got := failureReason("builder exited 1\n\x1b[31mERROR\x1b[0m: evil\u202Etxt.exe\r\nend")
	if strings.ContainsAny(got, "\x1b\n\r\u202E") || !strings.Contains(got, "ERROR") || !strings.HasSuffix(got, "end") {
		t.Errorf("reason %q", got)
	}
	if long := failureReason(strings.Repeat("é", 1000)); len(long) > maxFailureReason+len("…") || !strings.HasSuffix(long, "…") {
		t.Errorf("a long reason is %d bytes", len(long))
	}
}

// TestAFinishedInstallJoinsAtTheNextTurn: a server added to a running agent
// installs in the background while the agent answers turns in the same
// segment; once it is installed the next turn, and that turn alone, starts
// the segment that has it, and tells the agent it joined with the tools its
// gateway serves now.
func TestAFinishedInstallJoinsAtTheNextTurn(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st, in := &starter{resumed: true}, newInstaller(t)
	st.served = func(seg localrun.Segment) []string {
		tools := []string{"fs_read", "fs_write"}
		for _, srv := range seg.Grants.Servers {
			tools = append(tools, mcpinstall.IDFor(srv)+"_create_document")
		}
		return tools
	}
	sv := startWith(t, home, st, in)
	events, detach := sv.Subscribe()
	defer detach()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r, err := sv.Change(Change{AddServer: &wordServer, Consent: true})
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := r.Entry(ServerEntryID(wordServer)); e.State != StateInstalling || sv.Status().Pending {
		t.Fatalf("the added server is %s, pending %v: want installing, and nothing to restart for", e.State, sv.Status().Pending)
	}
	if err := sv.Send(ctx, "while it installs"); err != nil {
		t.Fatal(err)
	}
	if len(st.segments) != 1 {
		t.Fatalf("a turn during the install started %d segments, want the one", len(st.segments))
	}
	if got := st.last.sentTurns(); len(got) != 1 || got[0] != "while it installs" {
		t.Errorf("the turn during the install was sent %q", got)
	}

	in.release <- struct{}{}
	waitFor(t, "the install to finish", func() bool { return entryState(sv, ServerEntryID(wordServer)) == StatePending })
	if !sv.Status().Pending {
		t.Error("a finished install leaves nothing pending")
	}
	if err := sv.Send(ctx, "now"); err != nil {
		t.Fatal(err)
	}
	if len(st.segments) != 2 || len(st.segments[1].Grants.Servers) != 1 {
		t.Fatalf("segments %d; the second given %v", len(st.segments), st.segments[len(st.segments)-1].Grants.Servers)
	}
	turn := st.last.sentTurns()
	if len(turn) != 1 || !strings.HasPrefix(turn[0], "(An MCP server the person added finished installing") ||
		!strings.Contains(turn[0], `"pypi:office-word-mcp-server@1.1.11"`) || !strings.Contains(turn[0], `"pypi-office-word-mcp-server_create_document"`) ||
		strings.Contains(turn[0], `"fs_read"`) || strings.Contains(turn[0], "switched on") || !strings.HasSuffix(turn[0], "now") {
		t.Errorf("the joining turn was sent %q", turn)
	}
	if err := sv.Send(ctx, "again"); err != nil {
		t.Fatal(err)
	}
	if len(st.segments) != 2 {
		t.Errorf("the join restarted the agent %d times", len(st.segments)-1)
	}
	if got := st.last.sentTurns(); got[len(got)-1] != "again" {
		t.Errorf("the join was told twice: %q", got[len(got)-1])
	}
	if got := entryState(sv, ServerEntryID(wordServer)); got != StateOn {
		t.Errorf("the joined server is %s", got)
	}
	var notices []string
	for len(events) > 0 {
		if ev := <-events; ev.Kind == session.KindNotice {
			notices = append(notices, ev.Text)
		}
	}
	if !slices.ContainsFunc(notices, func(n string) bool { return strings.HasPrefix(n, "Installing the MCP server") }) ||
		!slices.ContainsFunc(notices, func(n string) bool { return strings.Contains(n, "is installed: it joins at your next message") }) {
		t.Errorf("the chat was shown %q", notices)
	}
}

// TestAFailedInstallLeavesTheAgentRunning: a server whose install fails is
// failed on the roster with the builder's last lines, the agent is told
// once at its next turn and keeps running in the same segment; switching it
// off and on installs it again, and it joins.
func TestAFailedInstallLeavesTheAgentRunning(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st, in := &starter{resumed: true}, newInstaller(t)
	in.fail = receipt.WithReason(errors.New("install pypi-office-word-mcp-server: builder exited 1"), receipt.ReasonMCPInstallFailed, receipt.InstallBuilder, "pypi:office-word-mcp-server")
	in.lines = "Collecting office-word-mcp-server==1.1.11\nERROR: No matching distribution found for office-word-mcp-server==1.1.11\n"
	sv := startWith(t, home, st, in)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := sv.Change(Change{AddServer: &wordServer, Consent: true}); err != nil {
		t.Fatal(err)
	}
	in.release <- struct{}{}
	waitFor(t, "the install to fail", func() bool { return entryState(sv, ServerEntryID(wordServer)) == StateFailed })
	e, _ := sv.Roster().Entry(ServerEntryID(wordServer))
	if !strings.Contains(e.Reason, "builder exited 1") || !strings.Contains(e.Reason, "No matching distribution found") {
		t.Errorf("the reason is %q, want the error and the builder's last lines", e.Reason)
	}
	if err := sv.Send(ctx, "still there?"); err != nil {
		t.Fatal(err)
	}
	if err := sv.Send(ctx, "and now?"); err != nil {
		t.Fatal(err)
	}
	turns := st.last.sentTurns()
	if len(st.segments) != 1 || sv.Status().State != StateRunning {
		t.Fatalf("a failed install restarted or stopped the agent: segments %d, %+v", len(st.segments), sv.Status())
	}
	if len(turns) != 2 || !strings.HasPrefix(turns[0], "(Agentry could not give you the MCP server \"pypi:office-word-mcp-server@1.1.11\" the person added") || turns[1] != "and now?" {
		t.Errorf("the turns were sent %q: want the failure told once", turns)
	}

	in.mu.Lock()
	in.fail = nil
	in.mu.Unlock()
	if _, err := sv.Change(Change{Switch: &Switch{ID: ServerEntryID(wordServer), On: false}}); err != nil {
		t.Fatal(err)
	}
	if got := entryState(sv, ServerEntryID(wordServer)); got != StateOff {
		t.Errorf("switched off, the server is %s", got)
	}
	if _, err := sv.Change(Change{Switch: &Switch{ID: ServerEntryID(wordServer), On: true}}); err != nil {
		t.Fatal(err)
	}
	in.release <- struct{}{}
	waitFor(t, "the retried install", func() bool { return entryState(sv, ServerEntryID(wordServer)) == StatePending })
	if calls, _ := in.count(); calls != 2 {
		t.Errorf("installs %d, want the failed one and the retry", calls)
	}
	if err := sv.Send(ctx, "with it"); err != nil {
		t.Fatal(err)
	}
	if len(st.segments) != 2 || len(st.segments[1].Grants.Servers) != 1 {
		t.Errorf("the retried server did not join: segments %d", len(st.segments))
	}
}

// TestAFailedInstallStaysFailedAcrossAStart: a stopped agent's roster says
// the server failed and why, and a start leaves it out and installs nothing
// until the person switches it.
func TestAFailedInstallStaysFailedAcrossAStart(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	addWord(t, home)
	c, _ := LoadConfig(home, "assistant")
	c.ServerFailures = map[string]ServerFailure{ServerEntryID(wordServer): {Reason: "resolve pypi:office-word-mcp-server@1.1.11: not found", At: time.Now(), Told: true}}
	if err := SaveConfig(home, "assistant", c); err != nil {
		t.Fatal(err)
	}
	if e, _ := BuildRoster(assistantAgent(t), c, nil).Entry(ServerEntryID(wordServer)); e.State != StateFailed || !strings.Contains(e.Reason, "not found") {
		t.Fatalf("the stopped roster has %+v", e)
	}
	st, in := &starter{}, newInstaller(t)
	startWith(t, home, st, in)
	time.Sleep(100 * time.Millisecond)
	if calls, _ := in.count(); calls != 0 || len(st.segments[0].Grants.Servers) != 0 {
		t.Errorf("a start installed a failed server (%d) or gave it (%v)", calls, st.segments[0].Grants.Servers)
	}
}

// TestStopCancelsABackgroundInstall: stopping the agent cancels an install
// under way and waits for it; nothing is recorded of it.
func TestStopCancelsABackgroundInstall(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	addWord(t, home)
	st, in := &starter{}, newInstaller(t)
	sv := startWith(t, home, st, in)
	waitFor(t, "the install to start", func() bool { c, _ := in.count(); return c == 1 })
	if _, err := sv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, cancelled := in.count(); cancelled != 1 {
		t.Errorf("the stop left the install running (cancelled %d)", cancelled)
	}
	c, _ := LoadConfig(home, "assistant")
	if len(c.ServerFailures) != 0 || mcpinstall.Installed(serversDir(home, "assistant"), wordServer) {
		t.Errorf("a cancelled install recorded %v", c.ServerFailures)
	}
}

// TestAJoinedServerThatCannotStartIsLeftOut: when the segment a finished
// install starts cannot start with the server, the server is failed with
// that reason and the agent starts again without it, and keeps answering.
func TestAJoinedServerThatCannotStartIsLeftOut(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st, in := &starter{resumed: true}, newInstaller(t)
	refusing := func(ctx context.Context, seg localrun.Segment) (engine.PersistentSession, error) {
		if len(seg.Grants.Servers) > 0 {
			return nil, errors.New("Can't start mcp-pypi-office-word-mcp-server")
		}
		return st.start(ctx, seg)
	}
	sv, err := Start(context.Background(), Options{
		Home: home, Agent: assistantAgent(t), Client: ClientCLI, Policy: policy.Default(),
		Docker: &dockerx.Fake{}, StartSegment: refusing, InstallServer: in.install,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = sv.Stop(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := sv.Change(Change{AddServer: &wordServer, Consent: true}); err != nil {
		t.Fatal(err)
	}
	in.release <- struct{}{}
	waitFor(t, "the install", func() bool { return entryState(sv, ServerEntryID(wordServer)) == StatePending })
	if err := sv.Send(ctx, "go on"); err != nil {
		t.Fatalf("the turn after a server that cannot start: %v", err)
	}
	e, _ := sv.Roster().Entry(ServerEntryID(wordServer))
	if e.State != StateFailed || !strings.Contains(e.Reason, "Can't start") || sv.Status().State != StateRunning {
		t.Fatalf("the server is %+v, the agent %+v", e, sv.Status())
	}
	if turn := st.last.sentTurns(); len(turn) != 1 || !strings.HasPrefix(turn[0], "(Agentry could not give you the MCP server") || strings.Contains(turn[0], "finished installing") {
		t.Errorf("the turn was sent %q", turn)
	}

	// Switched off and on to try again: installed already, it is given to
	// the next segment at once, which cannot start with it either; the
	// agent runs on without it, failed again.
	for _, on := range []bool{false, true} {
		if _, err := sv.Change(Change{Switch: &Switch{ID: ServerEntryID(wordServer), On: on}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sv.Send(ctx, "and again"); err != nil {
		t.Fatalf("the turn after the retry: %v", err)
	}
	if e, _ := sv.Roster().Entry(ServerEntryID(wordServer)); e.State != StateFailed || sv.Status().State != StateRunning {
		t.Errorf("after the retry the server is %+v, the agent %+v", e, sv.Status())
	}
	if calls, _ := in.count(); calls != 1 {
		t.Errorf("an installed server was installed again (%d)", calls)
	}
}

// TestAnAddedServerChangesNoGrantsUntilInstalled: the grants a segment is
// given count a package server once the store holds its pin, so adding one
// restarts nothing and its install does.
func TestAnAddedServerChangesNoGrantsUntilInstalled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	a := assistantAgent(t)
	c := Config{Enabled: true}
	before := grantsKey(home, a, c)
	restart, err := ApplyChange(home, a, &c, Change{AddServer: &wordServer, Consent: true})
	if err != nil || restart || grantsKey(home, a, c) != before {
		t.Fatalf("adding an uninstalled server: restart %v, err %v", restart, err)
	}
	record(t, serversDir(home, "assistant"), wordServer)
	if grantsKey(home, a, c) == before {
		t.Error("the finished install changed nothing a segment is given")
	}
	unlock, err := mcpinstall.LockServer(t.Context(), serversDir(home, "assistant"), mcpinstall.IDFor(wordServer))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if grantsKey(home, a, c) != before {
		t.Error("a server whose store lock is held counts")
	}
}
