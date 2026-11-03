package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter/signintest"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// acpClient is an editor's side of the protocol for the command-level tests.
type acpClient struct {
	mu     sync.Mutex
	asked  []acp.RequestPermissionRequest
	text   []string
	tools  []string // the titles of the tool calls the agent made
	answer string   // the option chosen; "" answers with an error
	// onAsk runs when the client is asked, before it answers: what a person
	// could do while they think about it.
	onAsk   func()
	badLine string // the first line of the agent's stdout that is not a JSON-RPC frame
}

func (c *acpClient) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m := n.Update.AgentMessageChunk; m != nil && m.Content.Text != nil {
		c.text = append(c.text, m.Content.Text.Text)
	}
	if tc := n.Update.ToolCall; tc != nil {
		c.tools = append(c.tools, tc.Title)
	}
	return nil
}

func (c *acpClient) RequestPermission(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.asked = append(c.asked, p)
	ans, hook := c.answer, c.onAsk
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
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
	init, err := conn.Initialize(context.Background(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	if err != nil {
		t.Fatal(err)
	}
	if init.AgentInfo == nil || init.AgentInfo.Name != "agentry" {
		t.Errorf("the agent announced itself as %+v", init.AgentInfo)
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
		"-agent": {Stdout: signintest.Line(signintest.Event{Type: "tool_call", Text: "fetch"}) + "\n" + signintest.Line(signintest.Event{Type: "text", Text: "done"}) + "\n"},
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
	conn, _, stop := startACP(t, cl, "--local", repoPath(t, "testdata/runner/hello-chat"), "--allow-unsigned", "--policy", policyPath, "--deliver-to", "/tmp/out-acp", "--destination", "archive", "--destination", "n8n")
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
	for _, want := range []string{"fixtures/hello-chat", "(interactive)", "capabilities:", "network.egress", "none", "attestation", "deliverables are copied to /tmp/out-acp", "none of this client's files",
		"fixtures/hello-chat@1.0.0 (interactive)\nReads the workspace and answers about it — the E2E smoke agent for both run modes\n",
		", attestation: local\n", "  filesystem.read: workspace\n",
		"  interactive: you talk to this agent in this session; you cannot give it more access than listed above\n",
		"result and deliverables are sent to: archive, n8n\n"} {
		if !strings.Contains(shown, want) {
			t.Errorf("the consent text lacks %q:\n%s", want, shown)
		}
	}
	if q.ToolCall.Title == nil || !strings.HasPrefix(*q.ToolCall.Title, "Run fixtures/hello-chat") {
		t.Errorf("title %v", q.ToolCall.Title)
	}
	if q.ToolCall.Title != nil && *q.ToolCall.Title != "Run fixtures/hello-chat@1.0.0?" {
		t.Errorf("title %q", *q.ToolCall.Title)
	}
	if !regexp.MustCompile(`\nrelease sha256:[0-9a-f]{64}, attestation: local\n`).MatchString(shown) {
		t.Errorf("the consent text does not name the release's digest:\n%s", shown)
	}
	// A release the person never accepted before has nothing to compare, and
	// one with no egress or exec has no first-run question to ask.
	for _, absent := range []string{"last accepted at", "first run:"} {
		if strings.Contains(shown, absent) {
			t.Errorf("the consent text carries %q:\n%s", absent, shown)
		}
	}
}

// A client that allows the request has consented: a release that declares
// egress and exec is then run without a second question on the terminal the
// command does not have.
func TestAgentryACPRunsWhatTheClientConsentedTo(t *testing.T) {
	policyPath, f := acpFixture(t)
	cl := &acpClient{answer: "allow"}
	conn, errOut, stop := startACP(t, cl, "--local", repoPath(t, "testdata/runner/sandbox-egress"), "--allow-unsigned", "--policy", policyPath, "--adapter", signintest.TokenAdapter, "--out", t.TempDir())
	sid := acpHandshake(t, conn)
	_, err := conn.Prompt(context.Background(), acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock("do the thing")}})
	stop()
	if len(cl.asked) != 1 {
		t.Fatalf("the client was asked %d times", len(cl.asked))
	}
	var shown string
	for _, c := range cl.asked[0].ToolCall.Content {
		if c.Content != nil && c.Content.Content.Text != nil {
			shown += c.Content.Content.Text.Text
		}
	}
	for _, want := range []string{"network.egress: example.com\n", "exec: commands: cat\n", "\nfirst run: a first run of a QuickAgent that declares "} {
		if !strings.Contains(shown, want) {
			t.Errorf("the consent text lacks %q:\n%s", want, shown)
		}
	}
	if err != nil && strings.Contains(err.Error(), "--yes") {
		t.Errorf("the client's consent was asked for again on the terminal: %v\n%s", err, errOut.String())
	}
	if !startedAContainer(f) {
		t.Errorf("the run did not start after the client allowed it: %s\n%s", daemonOps(f), errOut.String())
	}
}

// A release installed from a registry is held to its record again after the
// client was asked and before anything is compiled or pulled for it: what
// changed on disk while the person considered it is refused.
func TestAgentryACPRefusesAReleaseChangedWhileTheClientConsidered(t *testing.T) {
	policyPath, f := acpFixture(t)
	home, err := runnerHome()
	if err != nil {
		t.Fatal(err)
	}
	fr := newFakeRegistry(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	tampered := filepath.Join(installdir.ReleaseDir(home, "acme", "demo", "1.0.0", fv.digest), "workspace", "SKILL.md")
	cl := &acpClient{answer: "allow", onAsk: func() {
		if err := os.WriteFile(tampered, []byte("changed while the client was asked\n"), 0o644); err != nil {
			t.Error(err)
		}
	}}
	conn, errOut, stop := startACP(t, cl, "acme/demo@1.0.0", "--registry", fr.url(), "--allow-unsigned", "--policy", policyPath, "--adapter", signintest.TokenAdapter, "--out", t.TempDir())
	sid := acpHandshake(t, conn)
	_, err = conn.Prompt(context.Background(), acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock("do the thing")}})
	stop()
	var re *acp.RequestError
	if !errors.As(err, &re) || !strings.Contains(re.Message, "acme/demo@1.0.0") || !strings.Contains(re.Message, "SKILL.md") {
		t.Fatalf("a release changed while the client considered it: %v\n%s", err, errOut.String())
	}
	if startedAContainer(f) {
		t.Errorf("an image was pulled for a release that changed: %s", daemonOps(f))
	}
}

// The consent text compares the release with what the person accepted before:
// a first run says why it asks, a later one says what it widens or that
// nothing did, and a release with no recorded attestation says it was not
// checked.
func TestAgentryACPConsentTextComparesWithWhatWasAcceptedBefore(t *testing.T) {
	isolateEnv(t)
	home, _ := isolateRunnerHome(t)
	wide, err := attackManifest(t, `"s"`, "capabilities:\n  network:\n    egress: [\"api.example.com\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	narrow, err := attackManifest(t, `"s"`, "")
	if err != nil {
		t.Fatal(err)
	}
	meta := &installdir.Meta{Ref: "acme/capdemo@1.0.0", Namespace: "acme", Name: "capdemo", Version: "1.0.0", Digest: "sha256:abc",
		Attestation: receipt.Attestation{State: "verified"}}
	text := func(m *manifest.Manifest, meta *installdir.Meta) string {
		return acpConsentText(m, meta, home, runOpts{}, manifest.ModeAutonomous, nil)
	}

	first := text(wide, meta)
	if !strings.Contains(first, "\nfirst run: a first run of a QuickAgent that declares ") || strings.Contains(first, "last accepted at") {
		t.Errorf("a first run:\n%s", first)
	}
	if got := text(narrow, meta); strings.Contains(got, "first run:") || strings.Contains(got, "last accepted at") {
		t.Errorf("a first run that declares nothing to ask about:\n%s", got)
	}

	key := installdir.ConsentKey(home, "acme", "capdemo")
	var st installed.State
	st.Reconsent(key, "0.9.0", installed.Snapshot(narrow.Contract()), "sha256:old")
	if err := installed.Save(st); err != nil {
		t.Fatal(err)
	}
	widened := text(wide, meta)
	if !strings.Contains(widened, "\nlast accepted at 0.9.0, and this version WIDENS it:\n  + ") || strings.Contains(widened, "first run:") || strings.Contains(widened, "nothing widened") {
		t.Errorf("a version that widens what was accepted:\n%s", widened)
	}
	lines := installed.DiffCapabilities(installed.Snapshot(narrow.Contract()), installed.Snapshot(wide.Contract())).Widened
	if len(lines) == 0 {
		t.Fatal("fixture: the wider contract widens nothing")
	}
	for _, l := range lines {
		if !strings.Contains(widened, "\n  + "+l+"\n") {
			t.Errorf("the widened line %q is not shown:\n%s", l, widened)
		}
	}

	st = installed.State{}
	st.Reconsent(key, "1.0.0", installed.Snapshot(wide.Contract()), "sha256:abc")
	if err := installed.Save(st); err != nil {
		t.Fatal(err)
	}
	if same := text(wide, meta); !strings.Contains(same, "\nlast accepted at 1.0.0; nothing widened\n") || strings.Contains(same, "WIDENS") {
		t.Errorf("a version that widens nothing:\n%s", same)
	}

	unchecked := *meta
	unchecked.Attestation = receipt.Attestation{}
	if got := text(narrow, &unchecked); !strings.Contains(got, ", attestation not checked\n") {
		t.Errorf("a release with no recorded attestation:\n%s", got)
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
	if !errors.As(err, &re) || !strings.HasPrefix(re.Message, "blocked: ") {
		t.Fatalf("want the run's failure as an error answer, got %v", err)
	}
	// The agent's result reaches the client, and its tool calls as they are made.
	if !strings.Contains(strings.Join(cl.text, ""), "done") {
		t.Errorf("the agent's words did not reach the client: %q", cl.text)
	}
	if !slices.Contains(cl.tools, "fetch") {
		t.Errorf("the agent's tool call did not reach the client: %q", cl.tools)
	}
	if len(cl.text) == 0 || !strings.Contains(cl.text[0], "fixtures/pure-text") {
		t.Fatalf("the client was not shown the plan: %q", cl.text)
	}
	// An autonomous release takes the client's first prompt as its task.
	if got := f.Foreground["-agent"].GotStdin; got != "do the thing" {
		t.Errorf("the agent was handed the task %q", got)
	}
}

func TestAgentryACPRefusesBadFlagsBeforeServing(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{}, "pass exactly one of ns/name[@range] or --local DIR"},
		{[]string{"--local", "x", "acme/x"}, "pass exactly one of ns/name[@range] or --local DIR"},
		{[]string{"acme/x", "--mode", "chatty"}, `unknown --mode "chatty" (want autonomous or interactive)`},
		{[]string{"acme/x", "--no-destination", "--destination", "d"}, "--no-destination and --destination exclude each other"},
	} {
		_, _, err := runCLI(t, strings.NewReader(""), append([]string{"acp"}, c.args...)...)
		wantClassified(t, err, engine.ExitUsage)
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("acp %v: %v, want %q", c.args, err, c.want)
		}
	}
}

// An interactive release served over ACP: one session, each prompt a turn, the
// session ended when the client closes it, its sandbox torn down.
func TestAgentryACPServesAnInteractiveSession(t *testing.T) {
	policyPath, _ := acpFixture(t)
	runDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(runDir, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	report := []byte("weekly numbers\n")
	if err := os.WriteFile(filepath.Join(runDir, "reports", "summary.md"), report, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(report)
	rec := &chatSession{events: make(chan session.Event, 16), env: receipt.Envelope{Status: "ok", ExitClass: "ok", OutDir: runDir,
		Deliverables: &receipt.Deliverables{Files: []receipt.Deliverable{{Path: "reports/summary.md", Title: "Weekly summary", SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(report))}}},
		Deliveries:   []receipt.Delivery{{Kind: receipt.DeliveryKindFolder, Status: receipt.DeliveryDelivered, Object: "/delivered/weekly/summary.md"}}}}
	var started engine.Params
	oldStart := startSession
	startSession = func(_ context.Context, _ dockerx.Client, p engine.Params) (session.Session, error) {
		started = p
		if p.Task != nil {
			t.Error("an interactive session sends the first prompt itself; the engine is given no task")
		}
		return rec, nil
	}
	t.Cleanup(func() { startSession = oldStart })
	cl := &acpClient{answer: "allow"}
	out := t.TempDir()
	conn, errOut, stop := startACP(t, cl, "--local", repoPath(t, "testdata/runner/hello-chat"), "--allow-unsigned", "--policy", policyPath, "--out", out,
		"--timeout", "120", "--max-turns", "7", "--max-cost", "1.5")
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
	if rec.finish {
		t.Error("the session was ended asking for a final result; a client's close asks for none")
	}
	if !strings.Contains(strings.Join(cl.text, ""), "you said: thanks") {
		t.Fatalf("the replies did not reach the client: %q", cl.text)
	}
	// What the client is told once the session is over: the deliverables its
	// teardown listed.
	if !strings.Contains(strings.Join(cl.text, ""), "Deliverable:") {
		t.Errorf("the client was not told of the session's deliverable: %q", cl.text)
	}
	// And what the person who launched the command reads on stderr.
	for _, want := range []string{"session ended: quit\n", "transcript, reports and receipts: " + runDir + "\n", "deliverables: 1 file copied to " + filepath.Dir("/delivered/weekly/summary.md") + "\n"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut.String())
		}
	}
	// The budgets and the output folder the command line named reach the engine.
	if started.OutDir != out || started.Budgets.TimeoutSeconds != 120 || started.Budgets.MaxTurns != 7 || started.Budgets.MaxCostUSD != 1.5 {
		t.Errorf("the engine was started with out %q and budgets %+v", started.OutDir, started.Budgets)
	}
}

// chatSession answers each turn with what it was told.
type chatSession struct {
	events chan session.Event
	mu     sync.Mutex
	sent   []string
	ends   int
	closed bool
	finish bool
	// env is what End answers.
	env receipt.Envelope
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
func (c *chatSession) End(_ context.Context, finish bool) (*receipt.Envelope, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ends++
	if !c.closed {
		c.closed, c.finish = true, finish
		close(c.events)
	}
	env := c.env
	return &env, nil
}
func (c *chatSession) Ended() (bool, session.EndReason) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return true, session.EndQuit
	}
	return false, ""
}
