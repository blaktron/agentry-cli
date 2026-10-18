package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter/signintest"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// acpClient is an editor's side of the protocol for the command-level tests.
type acpClient struct {
	mu      sync.Mutex
	asked   []acp.RequestPermissionRequest
	text    []string
	answer  string // the option chosen; "" answers with an error
	badLine string // the first line of the agent's stdout that is not a JSON-RPC frame
}

func (c *acpClient) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m := n.Update.AgentMessageChunk; m != nil && m.Content.Text != nil {
		c.text = append(c.text, m.Content.Text.Text)
	}
	return nil
}

func (c *acpClient) RequestPermission(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.asked = append(c.asked, p)
	ans := c.answer
	c.mu.Unlock()
	if ans == "" {
		return acp.RequestPermissionResponse{}, errors.New("this client cannot show a permission request")
	}
	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(acp.PermissionOptionId(ans))}, nil
}

func (c *acpClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errors.New("no")
}
func (c *acpClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errors.New("no")
}
func (c *acpClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errors.New("no")
}
func (c *acpClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, nil
}
func (c *acpClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, nil
}
func (c *acpClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, nil
}
func (c *acpClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}

// startACP runs `agentry acp args…` through the real command tree with the SDK
// client on the other end of its stdin and stdout. Every line the command
// writes to stdout is checked to be a JSON-RPC frame.
func startACP(t *testing.T, cl *acpClient, args ...string) (*acp.ClientSideConnection, *bytes.Buffer, func()) {
	t.Helper()
	cr, aw := io.Pipe()
	ar, cw := io.Pipe()
	tapR, tapW := io.Pipe()
	go func() { // the tap: stdout of the command, line by line
		defer func() { _ = tapW.Close() }()
		sc := bufio.NewScanner(cr)
		sc.Buffer(make([]byte, 1<<20), 32<<20)
		for sc.Scan() {
			var f struct {
				JSONRPC string `json:"jsonrpc"`
			}
			if json.Unmarshal(sc.Bytes(), &f) != nil || f.JSONRPC != "2.0" {
				cl.mu.Lock()
				if cl.badLine == "" {
					cl.badLine = sc.Text()
				}
				cl.mu.Unlock()
			}
			_, _ = tapW.Write(append(sc.Bytes(), '\n'))
		}
	}()
	root := newRootCmd()
	var errOut bytes.Buffer
	root.SetOut(aw)
	root.SetErr(&errOut)
	root.SetIn(ar)
	root.SetArgs(append([]string{"acp"}, args...))
	done := make(chan error, 1)
	go func() {
		err := root.ExecuteContext(context.Background())
		_ = aw.Close()
		done <- err
	}()
	conn := acp.NewClientSideConnection(cl, cw, tapR)
	stop := func() {
		_ = cw.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("agentry acp ended with %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("agentry acp did not stop when its stdin closed")
		}
		cl.mu.Lock()
		bad := cl.badLine
		cl.mu.Unlock()
		if bad != "" {
			t.Errorf("stdout carried something that is not a JSON-RPC frame: %q", bad)
		}
	}
	return conn, &errOut, stop
}

func acpHandshake(t *testing.T, conn *acp.ClientSideConnection) acp.SessionId {
	t.Helper()
	if _, err := conn.Initialize(context.Background(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatal(err)
	}
	ns, err := conn.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/work", McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	return ns.SessionId
}

func daemonOps(f *dockerx.Fake) string { return strings.Join(f.Ops(), " ") }

func startedAContainer(f *dockerx.Fake) bool {
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "Pull:") || strings.HasPrefix(op, "RunForeground") || strings.HasPrefix(op, "RunDetached") || strings.HasPrefix(op, "Build") {
			return true
		}
	}
	return false
}

func acpFixture(t *testing.T) (policyPath string, f *dockerx.Fake) {
	t.Helper()
	isolateEnv(t)
	_, policyPath = isolateRunnerHome(t)
	signintest.Register()
	f = &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
		"-probe": {},
		"-agent": {Stdout: signintest.Line(signintest.Event{Type: "text", Text: "done"}) + "\n"},
	}}
	prev := newDockerClient
	newDockerClient = func() dockerx.Client { return f }
	t.Cleanup(func() { newDockerClient = prev })
	if _, _, err := policy.EnsureDefault(policyPath); err != nil {
		t.Fatal(err)
	}
	return policyPath, f
}

// A client that declines, cannot show the request or sends the wrong option
// leaves the daemon untouched: the run is refused before anything is pulled.
func TestAgentryACPDoesNotStartARunTheClientDidNotConsentTo(t *testing.T) {
	for name, answer := range map[string]string{"declined": "reject", "cannot show it": "", "an option it was not offered": "allow-always"} {
		t.Run(name, func(t *testing.T) {
			policyPath, f := acpFixture(t)
			cl := &acpClient{answer: answer}
			conn, _, stop := startACP(t, cl, "--local", repoPath(t, "testdata/runner/pure-text"), "--allow-unsigned", "--policy", policyPath, "--adapter", signintest.TokenAdapter)
			sid := acpHandshake(t, conn)
			resp, err := conn.Prompt(context.Background(), acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock("do the thing")}})
			if err != nil || resp.StopReason != acp.StopReasonRefusal {
				t.Fatalf("prompt: %q, %v", resp.StopReason, err)
			}
			stop()
			if len(cl.asked) != 1 {
				t.Fatalf("the client was asked %d times", len(cl.asked))
			}
			if startedAContainer(f) {
				t.Fatalf("the daemon was reached without consent: %s", daemonOps(f))
			}
		})
	}
}

// The consent the client is shown names what the run may do, even for a
// release that declares nothing a terminal would ask about.
func TestAgentryACPAsksForConsentAndShowsThePlan(t *testing.T) {
	policyPath, _ := acpFixture(t)
	cl := &acpClient{answer: "reject"}
	conn, _, stop := startACP(t, cl, "--local", repoPath(t, "testdata/runner/hello-chat"), "--allow-unsigned", "--policy", policyPath, "--deliver-to", "/tmp/out-acp")
	sid := acpHandshake(t, conn)
	if _, err := conn.Prompt(context.Background(), acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock("hi")}}); err != nil {
		t.Fatal(err)
	}
	stop()
	if len(cl.asked) != 1 {
		t.Fatalf("asked %d times", len(cl.asked))
	}
	q := cl.asked[0]
	var shown string
	for _, c := range q.ToolCall.Content {
		if c.Content != nil && c.Content.Content.Text != nil {
			shown += c.Content.Content.Text.Text
		}
	}
	for _, want := range []string{"fixtures/hello-chat", "(interactive)", "capabilities:", "network.egress", "none", "attestation", "deliverables are copied to /tmp/out-acp", "none of this client's files"} {
		if !strings.Contains(shown, want) {
			t.Errorf("the consent text lacks %q:\n%s", want, shown)
		}
	}
	if q.ToolCall.Title == nil || !strings.HasPrefix(*q.ToolCall.Title, "Run fixtures/hello-chat") {
		t.Errorf("title %v", q.ToolCall.Title)
	}
}

// --yes is the person's own consent on the command line: the client is not
// asked, and still told what runs.
func TestAgentryACPYesAsksNoOneButStillShowsThePlan(t *testing.T) {
	policyPath, f := acpFixture(t)
	cl := &acpClient{answer: ""}
	conn, errOut, stop := startACP(t, cl, "--local", repoPath(t, "testdata/runner/pure-text"), "--allow-unsigned", "--yes", "--policy", policyPath, "--adapter", signintest.TokenAdapter, "--out", t.TempDir())
	sid := acpHandshake(t, conn)
	_, err := conn.Prompt(context.Background(), acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock("do the thing")}})
	stop()
	if len(cl.asked) != 0 {
		t.Fatalf("a --yes run asked the client")
	}
	if !startedAContainer(f) {
		t.Fatalf("the run did not start: %s\n%s", daemonOps(f), errOut.String())
	}
	var re *acp.RequestError
	// The fake daemon runs no tools container, so the release's one-shot run
	// ends blocked: the client is told so, as an error answer.
	if !errors.As(err, &re) {
		t.Fatalf("want the run's failure as an error answer, got %v", err)
	}
	if len(cl.text) == 0 || !strings.Contains(cl.text[0], "fixtures/pure-text") {
		t.Fatalf("the client was not shown the plan: %q", cl.text)
	}
}

func TestAgentryACPRefusesBadFlagsBeforeServing(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"--local", "x", "acme/x"},
		{"acme/x", "--mode", "chatty"},
		{"acme/x", "--no-destination", "--destination", "d"},
	} {
		_, _, err := runCLI(t, strings.NewReader(""), append([]string{"acp"}, args...)...)
		wantClassified(t, err, engine.ExitUsage)
	}
}

// An interactive release served over ACP: one session, each prompt a turn, the
// session ended when the client closes it, its sandbox torn down.
func TestAgentryACPServesAnInteractiveSession(t *testing.T) {
	policyPath, _ := acpFixture(t)
	rec := &chatSession{events: make(chan session.Event, 16)}
	oldStart := startSession
	startSession = func(_ context.Context, _ dockerx.Client, p engine.Params) (session.Session, error) {
		if p.Task != nil {
			t.Error("an interactive session sends the first prompt itself; the engine is given no task")
		}
		return rec, nil
	}
	t.Cleanup(func() { startSession = oldStart })
	cl := &acpClient{answer: "allow"}
	conn, _, stop := startACP(t, cl, "--local", repoPath(t, "testdata/runner/hello-chat"), "--allow-unsigned", "--policy", policyPath, "--out", t.TempDir())
	sid := acpHandshake(t, conn)
	for _, say := range []string{"what is in the workspace?", "thanks"} {
		resp, err := conn.Prompt(context.Background(), acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock(say)}})
		if err != nil || resp.StopReason != acp.StopReasonEndTurn {
			t.Fatalf("prompt %q: %v %v", say, resp.StopReason, err)
		}
	}
	if _, err := conn.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	stop()
	if len(cl.asked) != 1 {
		t.Fatalf("consent was asked %d times", len(cl.asked))
	}
	if got := strings.Join(rec.sent, "|"); got != "what is in the workspace?|thanks" {
		t.Fatalf("turns %q", got)
	}
	if rec.ends == 0 {
		t.Fatal("the session was never ended")
	}
	if !strings.Contains(strings.Join(cl.text, ""), "you said: thanks") {
		t.Fatalf("the replies did not reach the client: %q", cl.text)
	}
}

// chatSession answers each turn with what it was told.
type chatSession struct {
	events chan session.Event
	mu     sync.Mutex
	sent   []string
	ends   int
	closed bool
}

func (c *chatSession) Events() <-chan session.Event { return c.events }
func (c *chatSession) Send(_ context.Context, text string) error {
	c.mu.Lock()
	c.sent = append(c.sent, text)
	c.mu.Unlock()
	c.events <- session.Event{Kind: session.KindAssistantText, Text: "you said: " + text}
	c.events <- session.Event{Kind: session.KindAssistantTurn, Text: "you said: " + text}
	return nil
}
func (c *chatSession) AddInput(context.Context, string) error { return nil }
func (c *chatSession) End(context.Context, bool) (*receipt.Envelope, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ends++
	if !c.closed {
		c.closed = true
		close(c.events)
	}
	return &receipt.Envelope{Status: "ok", ExitClass: "ok", OutDir: "/nowhere"}, nil
}
func (c *chatSession) Ended() (bool, session.EndReason) { return false, "" }
