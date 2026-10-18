package acpagent_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/blaktron/agentry-cli/pkg/runner/acpagent"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// The conformance tests drive the agent side with the ACP Go SDK's own client,
// the way an editor would.

type recorder struct {
	mu      sync.Mutex
	updates []acp.SessionUpdate
	asked   []acp.RequestPermissionRequest
	answer  func(acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)
	fsCalls int
}

func (r *recorder) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, n.Update)
	return nil
}

func (r *recorder) RequestPermission(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	r.mu.Lock()
	r.asked = append(r.asked, p)
	ans := r.answer
	r.mu.Unlock()
	if ans == nil {
		return acp.RequestPermissionResponse{}, errors.New("no one to ask")
	}
	return ans(p)
}

func choose(id string) func(acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return func(acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(acp.PermissionOptionId(id))}, nil
	}
}

func (r *recorder) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	r.mu.Lock()
	r.fsCalls++
	r.mu.Unlock()
	return acp.ReadTextFileResponse{}, errors.New("not served")
}
func (r *recorder) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	r.mu.Lock()
	r.fsCalls++
	r.mu.Unlock()
	return acp.WriteTextFileResponse{}, errors.New("not served")
}
func (r *recorder) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	r.mu.Lock()
	r.fsCalls++
	r.mu.Unlock()
	return acp.CreateTerminalResponse{}, errors.New("not served")
}
func (r *recorder) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, nil
}
func (r *recorder) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, nil
}
func (r *recorder) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, nil
}
func (r *recorder) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}

type fnBackend func(ctx context.Context, h *acpagent.Host, first string) error

func (f fnBackend) Run(ctx context.Context, h *acpagent.Host, first string) error {
	return f(ctx, h, first)
}

// connect starts the agent with b and an SDK client on the other end.
func connect(t *testing.T, b acpagent.Backend, rec *recorder) (*acp.ClientSideConnection, func()) {
	t.Helper()
	cr, aw := io.Pipe() // agent -> client
	ar, cw := io.Pipe() // client -> agent
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = acpagent.Serve(context.Background(), ar, aw, acpagent.Info{Name: "agentry", Version: "test"}, b)
		_ = aw.Close()
	}()
	conn := acp.NewClientSideConnection(rec, cw, cr)
	stop := func() {
		_ = cw.Close()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("the agent did not stop when the client closed")
		}
	}
	t.Cleanup(stop)
	return conn, stop
}

func start(t *testing.T, conn *acp.ClientSideConnection) acp.SessionId {
	t.Helper()
	ctx := context.Background()
	init, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		// An editor that offers its file system and a terminal: the agent
		// must not ask for either.
		ClientCapabilities: acp.ClientCapabilities{Fs: acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true}, Terminal: true},
	})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if init.ProtocolVersion != acp.ProtocolVersionNumber || init.AgentInfo == nil || init.AgentInfo.Name != "agentry" {
		t.Fatalf("initialize answered %+v", init)
	}
	caps := init.AgentCapabilities
	if caps.LoadSession || caps.PromptCapabilities.Image || caps.PromptCapabilities.Audio || caps.PromptCapabilities.EmbeddedContext {
		t.Fatalf("capabilities offered more than the agent serves: %+v", caps)
	}
	if caps.SessionCapabilities.Close == nil {
		t.Fatal("session/close is served and not advertised")
	}
	ns, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: "/work", McpServers: []acp.McpServer{}})
	if err != nil || ns.SessionId == "" {
		t.Fatalf("session/new: %v", err)
	}
	return ns.SessionId
}

// reportsFolder is a run's folder with its reports and an envelope listing them.
func reportsFolder(t *testing.T, files map[string]string) (string, *receipt.Deliverables) {
	t.Helper()
	dir := t.TempDir()
	ds := &receipt.Deliverables{}
	for _, rel := range []string{"reports/summary.md", "reports/data.pdf"} {
		c, ok := files[rel]
		if !ok {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(c))
		ds.Files = append(ds.Files, receipt.Deliverable{Path: rel, Title: strings.TrimPrefix(filepath.Ext(rel), ".") + " report", SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(c)), Type: strings.TrimPrefix(filepath.Ext(rel), "."), Marked: true})
	}
	return dir, ds
}

func TestAutonomousRunIsOneTurnWithItsDeliverablesInTheLastUpdates(t *testing.T) {
	dir, ds := reportsFolder(t, map[string]string{"reports/summary.md": "# Weekly\nAll quiet.\n", "reports/data.pdf": "%PDF-1.7 fake"})
	var ran bool
	backend := fnBackend(func(ctx context.Context, h *acpagent.Host, first string) error {
		if first != "summarise the week" {
			t.Errorf("the task is the first prompt's text, got %q", first)
		}
		if err := h.Consent(ctx, acpagent.Consent{Title: "Run acme/weekly?", Text: "acme/weekly@1.2.0\ncapabilities: no network"}); err != nil {
			return err
		}
		ran = true
		h.Event(adapter.Event{Kind: adapter.EventToolCall, Tool: "read_input", CallID: "c1"})
		h.Event(adapter.Event{Kind: adapter.EventToolResult, Tool: "read_input", CallID: "c1", Text: "ten lines"})
		env := &receipt.Envelope{RunID: "r1", Status: "ok", ExitClass: "ok", OutDir: dir, Result: []byte(`"Done: the week was quiet."`), Deliverables: ds}
		h.Finish(env, nil)
		return nil
	})
	rec := &recorder{answer: choose("allow")}
	conn, _ := connect(t, backend, rec)
	sid := start(t, conn)
	resp, err := conn.Prompt(context.Background(), acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock("summarise the week")}})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason %q", resp.StopReason)
	}
	if !ran {
		t.Fatal("the run never started")
	}
	if len(rec.asked) != 1 {
		t.Fatalf("the client was asked %d times", len(rec.asked))
	}
	q := rec.asked[0]
	if q.SessionId != sid || q.ToolCall.Title == nil || *q.ToolCall.Title != "Run acme/weekly?" {
		t.Fatalf("consent request %+v", q)
	}
	var optionIDs []string
	for _, o := range q.Options {
		optionIDs = append(optionIDs, string(o.OptionId))
	}
	if strings.Join(optionIDs, ",") != "allow,reject" {
		t.Fatalf("options %v", optionIDs)
	}
	if rec.fsCalls != 0 {
		t.Fatalf("the agent used the client's file system or terminal %d times", rec.fsCalls)
	}

	var kinds, texts []string
	var inline *acp.EmbeddedResourceResource
	var link *acp.ContentBlockResourceLink
	for _, u := range rec.updates {
		switch {
		case u.ToolCall != nil:
			kinds = append(kinds, "tool_call")
		case u.ToolCallUpdate != nil:
			kinds = append(kinds, "tool_call_update")
		case u.AgentMessageChunk != nil:
			c := u.AgentMessageChunk.Content
			switch {
			case c.Text != nil:
				texts = append(texts, c.Text.Text)
				kinds = append(kinds, "text")
			case c.Resource != nil:
				r := c.Resource.Resource
				inline = &r
				kinds = append(kinds, "resource")
			case c.ResourceLink != nil:
				link = c.ResourceLink
				kinds = append(kinds, "resource_link")
			}
		}
	}
	if got := strings.Join(kinds, ","); got != "tool_call,tool_call_update,text,text,resource,resource_link" {
		t.Fatalf("updates were %s", got)
	}
	if texts[0] != "Done: the week was quiet." || texts[1] != "Deliverables (2):" {
		t.Fatalf("texts %q", texts)
	}
	if inline == nil || inline.TextResourceContents == nil || inline.TextResourceContents.Text != "# Weekly\nAll quiet.\n" || !strings.HasPrefix(inline.TextResourceContents.Uri, "file://") {
		t.Fatalf("the text deliverable was not embedded: %+v", inline)
	}
	if link == nil || !strings.HasSuffix(link.Uri, "/reports/data.pdf") || link.MimeType == nil || *link.MimeType != "application/pdf" {
		t.Fatalf("the pdf was not linked: %+v", link)
	}
	meta, _ := resp.Meta["agentry"].(map[string]any)
	if meta["runId"] != "r1" || meta["status"] != "ok" {
		t.Fatalf("the answer's _meta says %v", resp.Meta)
	}
}

func TestAConsentTheClientDoesNotGiveIsARefusal(t *testing.T) {
	cases := map[string]struct {
		answer func(acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)
		wait   time.Duration
	}{
		"declined": {answer: choose("reject")},
		"dismissed": {answer: func(acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
		}},
		"an error": {answer: func(acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
			return acp.RequestPermissionResponse{}, errors.New("cannot show it")
		}},
		"no handler": {answer: nil},
		"another id": {answer: choose("allow-always")},
		"no answer":  {answer: func(acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) { select {} }},
		"empty outcome": {answer: func(acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
			return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{}}, nil
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			old := acpagent.ConsentTimeout
			acpagent.ConsentTimeout = 300 * time.Millisecond
			defer func() { acpagent.ConsentTimeout = old }()
			var ran bool
			backend := fnBackend(func(ctx context.Context, h *acpagent.Host, _ string) error {
				if err := h.Consent(ctx, acpagent.Consent{Title: "Run it?", Text: "x"}); err != nil {
					return err
				}
				ran = true
				return nil
			})
			rec := &recorder{answer: c.answer}
			conn, _ := connect(t, backend, rec)
			sid := start(t, conn)
			resp, err := conn.Prompt(context.Background(), acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock("go")}})
			if err != nil {
				t.Fatalf("prompt: %v", err)
			}
			if resp.StopReason != acp.StopReasonRefusal {
				t.Fatalf("stop reason %q", resp.StopReason)
			}
			if ran {
				t.Fatal("the run started without consent")
			}
		})
	}
}

// fakeSession is an interactive session whose turns answer with their prompt.
type fakeSession struct {
	events chan session.Event
	mu     sync.Mutex
	sent   []string
	ended  bool
	reason session.EndReason
}

func newFakeSession() *fakeSession { return &fakeSession{events: make(chan session.Event, 64)} }

func (f *fakeSession) Events() <-chan session.Event { return f.events }
func (f *fakeSession) Send(_ context.Context, text string) error {
	f.mu.Lock()
	f.sent = append(f.sent, text)
	f.mu.Unlock()
	f.events <- session.Event{Kind: session.KindUserTurn, Text: text}
	f.events <- session.Event{Kind: session.KindToolCall, Text: "list_inputs"}
	f.events <- session.Event{Kind: session.KindAssistantText, Text: "echo: " + text}
	f.events <- session.Event{Kind: session.KindAssistantTurn, Text: "echo: " + text}
	return nil
}
func (f *fakeSession) AddInput(context.Context, string) error { return nil }
func (f *fakeSession) End(context.Context, bool) (*receipt.Envelope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ended {
		f.ended, f.reason = true, session.EndQuit
		close(f.events)
	}
	return &receipt.Envelope{}, nil
}
func (f *fakeSession) Ended() (bool, session.EndReason) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ended, f.reason
}

func TestInteractiveSessionIsOneSessionAndEachPromptATurn(t *testing.T) {
	dir, ds := reportsFolder(t, map[string]string{"reports/summary.md": "# Notes\n"})
	fs := newFakeSession()
	backend := fnBackend(func(ctx context.Context, h *acpagent.Host, first string) error {
		if err := h.Consent(ctx, acpagent.Consent{Title: "Run?", Text: "x"}); err != nil {
			return err
		}
		_ = h.Converse(ctx, fs)
		env, err := fs.End(context.Background(), false)
		env.OutDir, env.Deliverables = dir, ds
		h.Finish(env, err)
		return err
	})
	rec := &recorder{answer: choose("allow")}
	conn, _ := connect(t, backend, rec)
	sid := start(t, conn)
	ctx := context.Background()
	for _, say := range []string{"hello", "and again"} {
		resp, err := conn.Prompt(ctx, acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock(say)}})
		if err != nil || resp.StopReason != acp.StopReasonEndTurn {
			t.Fatalf("prompt %q: %v %v", say, resp.StopReason, err)
		}
	}
	if len(rec.asked) != 1 {
		t.Fatalf("consent is asked once for the session, got %d", len(rec.asked))
	}
	if strings.Join(fs.sent, "|") != "hello|and again" {
		t.Fatalf("turns were %q", fs.sent)
	}
	before := len(rec.updates)
	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: sid}); err != nil {
		t.Fatalf("session/close: %v", err)
	}
	var sawResource bool
	rec.mu.Lock()
	for _, u := range rec.updates[before:] {
		if u.AgentMessageChunk != nil && u.AgentMessageChunk.Content.Resource != nil {
			sawResource = true
		}
	}
	rec.mu.Unlock()
	if !sawResource {
		t.Fatal("the deliverables the teardown listed did not come back before the close was answered")
	}
	if _, err := conn.Prompt(ctx, acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock("more")}}); err == nil {
		t.Fatal("a prompt after the close was taken")
	}
}

func TestACancelEndsTheRunAndIsAnsweredAsCancelled(t *testing.T) {
	started := make(chan struct{})
	backend := fnBackend(func(ctx context.Context, h *acpagent.Host, _ string) error {
		if err := h.Consent(ctx, acpagent.Consent{Title: "Run?", Text: "x"}); err != nil {
			return err
		}
		close(started)
		<-ctx.Done() // the run goes on until it is stopped
		h.Finish(&receipt.Envelope{Status: "cancelled"}, context.Cause(ctx))
		return context.Cause(ctx)
	})
	rec := &recorder{answer: choose("allow")}
	conn, _ := connect(t, backend, rec)
	sid := start(t, conn)
	type res struct {
		resp acp.PromptResponse
		err  error
	}
	got := make(chan res, 1)
	go func() {
		r, err := conn.Prompt(context.Background(), acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock("long job")}})
		got <- res{r, err}
	}()
	<-started
	if err := conn.Cancel(context.Background(), acp.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-got:
		if r.err != nil || r.resp.StopReason != acp.StopReasonCancelled {
			t.Fatalf("prompt answered %+v, %v", r.resp, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the prompt was not answered after the cancel")
	}
}

func TestARunThatFailsIsAnErrorAnswer(t *testing.T) {
	backend := fnBackend(func(ctx context.Context, h *acpagent.Host, _ string) error {
		if err := h.Consent(ctx, acpagent.Consent{Title: "Run?", Text: "x"}); err != nil {
			return err
		}
		err := errors.New("the harness exited 1")
		h.Finish(&receipt.Envelope{Status: "failed"}, err)
		return err
	})
	rec := &recorder{answer: choose("allow")}
	conn, _ := connect(t, backend, rec)
	sid := start(t, conn)
	_, err := conn.Prompt(context.Background(), acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock("go")}})
	var re *acp.RequestError
	if !errors.As(err, &re) || !strings.Contains(re.Message, "the harness exited 1") {
		t.Fatalf("want the run's error, got %v", err)
	}
}
