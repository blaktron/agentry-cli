package claudesignin

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/harnessbuild"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// freePort is a loopback port nothing listens on now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return p
}

// autoURL is the address Claude Code 2.1.296 hands its browser opener: its
// own callback on the container's loopback at port.
func autoURL(port int) string {
	return "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=http%3A%2F%2Flocalhost%3A" +
		strconv.Itoa(port) + "%2Fcallback&state=abc"
}

// claudeLogin plays Claude Code's `auth login` in the image: it prints the
// paste-code address and its prompt, hands the automatic one to its opener
// (the stand-in's stderr line), then waits for the callback or a pasted
// code, and exits 0 once one came.
type claudeLogin struct {
	port     int
	noOpener bool
	callback chan string
	gotStdin chan string
	pasted   chan string
	spec     chan dockerx.RunSpec
}

func newClaudeLogin(port int) *claudeLogin {
	return &claudeLogin{port: port, callback: make(chan string, 1), gotStdin: make(chan string, 1), pasted: make(chan string, 1), spec: make(chan dockerx.RunSpec, 1)}
}

func (c *claudeLogin) handler(ctx context.Context, r dockerx.RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	c.spec <- r
	_, _ = io.WriteString(stdout, "Opening browser to sign in…\nIf the browser didn't open, visit: "+signInURL+"\n")
	if !c.noOpener {
		_, _ = io.WriteString(stderr, "agentry-open "+autoURL(c.port)+"\n")
	}
	_, _ = io.WriteString(stdout, "Paste code here if prompted > ")
	if stdin != nil {
		go func() {
			line, _ := bufio.NewReader(stdin).ReadString('\n')
			if line != "" {
				c.gotStdin <- line
			}
		}()
	}
	select {
	case <-c.callback:
	case line := <-c.gotStdin:
		c.pasted <- line
	case <-ctx.Done():
		return 137, nil
	}
	_, _ = io.WriteString(stdout, "Login successful.\n")
	return 0, nil
}

// claudeCallback plays Claude Code's callback server behind the relay: it
// reads the browser's request and answers as Claude Code does, sending the
// browser on to Anthropic's success page.
func (c *claudeLogin) exec(_ context.Context, _ string, _ []string, stdin io.Reader, stdout io.Writer) (int, error) {
	req, err := http.ReadRequest(bufio.NewReader(stdin))
	if err != nil {
		return 1, nil
	}
	_, _ = io.WriteString(stdout, "HTTP/1.1 302 Found\r\nLocation: https://platform.claude.com/oauth/code/success?app=claude-code\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	c.callback <- req.URL.RawQuery
	return 0, nil
}

func browserFixture(t *testing.T, login *claudeLogin) (SignIn, *dockerx.Fake) {
	t.Helper()
	pol := policy.Default()
	h, _ := pol.Images.HarnessImageFor("claude-image")
	f := &dockerx.Fake{
		ExistingImages: map[string]bool{h.Ref: true},
		Labels:         map[string]map[string]string{h.Ref: {harnessbuild.LabelArtifact: h.Artifact, harnessbuild.LabelRecipe: harnessbuild.ClaudeRecipe}},
		Foreground: map[string]*dockerx.FakeForeground{
			"agentry-claude-login":  {Handler: login.handler},
			"agentry-claude-status": {Stdout: signedIn},
		},
		Exec: login.exec,
	}
	return SignIn{Home: t.TempDir(), Policy: pol, Docker: func() dockerx.Client { return f }}, f
}

// browser is the person's browser: it opens the address it is handed and,
// for Claude Code's own callback, comes back to this computer with the code
// as Anthropic's page would, without following the success redirect.
type browser struct {
	mu     sync.Mutex
	opened []string
	got    chan *http.Response
}

func newBrowser() *browser { return &browser{got: make(chan *http.Response, 1)} }

func (b *browser) open(code string) func(string) {
	return func(u string) {
		b.mu.Lock()
		b.opened = append(b.opened, u)
		b.mu.Unlock()
		port, ok := callbackPort(u)
		if !ok {
			return
		}
		go func() {
			c := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := c.Get(fmt.Sprintf("http://localhost:%d/callback?code=%s&state=abc", port, code))
			if err == nil {
				_ = resp.Body.Close()
				b.got <- resp
			}
		}()
	}
}

// pastes is the person's side for the paste route: what they paste reaches
// Claude Code's prompt only once Anthropic's paste-code page was opened.
func (b *browser) pastes(code string) (io.Reader, func(string)) {
	r, w := io.Pipe()
	open := b.open("")
	return r, func(u string) {
		open(u)
		if u == signInURL {
			go func() { _, _ = io.WriteString(w, code+"\n") }()
		}
	}
}

func (b *browser) addresses() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.opened)
}

// The image signs in through the browser alone (operator, 2026-10-11): the
// browser is sent to the address Claude Code handed its opener, and the
// page's answer reaches Claude Code's own callback through the relay
// unread. Nothing is pasted, the code is shown and kept nowhere, and the
// relay is gone when the sign-in returns.
func TestTheImageSignsInThroughTheBrowserAlone(t *testing.T) {
	port := freePort(t)
	login := newClaudeLogin(port)
	s, f := browserFixture(t, login)
	b := newBrowser()
	var out strings.Builder
	rec, err := s.Run(context.Background(), Person{Out: &out, Open: b.open("THE-CODE")}, false)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := b.addresses(); !slices.Equal(got, []string{autoURL(port)}) {
		t.Fatalf("opened %v", got)
	}
	resp := <-b.got
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "https://platform.claude.com/oauth/code/success?app=claude-code" {
		t.Fatalf("the browser got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	r := <-login.spec
	if r.Env["BROWSER"] != harnessbuild.ClaudeOpener {
		t.Fatalf("BROWSER = %q", r.Env["BROWSER"])
	}
	execs := slices.DeleteFunc(slices.Clone(f.Calls), func(c dockerx.FakeCall) bool { return c.Op != "ExecStream" })
	if len(execs) == 0 || execs[0].Arg != r.Name || !slices.Equal(execs[0].Run.Argv, []string{"node", "-e", relayJS, strconv.Itoa(port)}) {
		t.Fatalf("relay execs %+v (login %s)", execs, r.Name)
	}
	for _, never := range []string{"THE-CODE", "agentry-open", signInURL, "Paste code here"} {
		if strings.Contains(out.String(), never) {
			t.Errorf("the output shows %q:\n%s", never, out.String())
		}
	}
	if !strings.Contains(out.String(), "Approve the sign-in in your browser") {
		t.Errorf("no word of the browser:\n%s", out.String())
	}
	if rec.ConfigDir != Volume || rec.Label != "person@example.com (Example)" {
		t.Fatalf("record %+v", rec)
	}
	if c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second); err == nil {
		_ = c.Close()
		t.Fatal("the relay still listens after the sign-in returned")
	}
}

// Where the browser cannot hand the sign-in back (here, the port taken on
// this computer), a person who can paste is sent to Anthropic's paste-code
// page and told why; one who cannot (the desktop) is told the sign-in did
// not happen, and nothing is kept.
func TestThePasteRouteOnlyWhereTheBrowserCannotReachClaudeCode(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	port := busy.Addr().(*net.TCPAddr).Port

	login := newClaudeLogin(port)
	s, _ := browserFixture(t, login)
	b := newBrowser()
	var out strings.Builder
	in, open := b.pastes("the-code#abc")
	if _, err := s.Run(context.Background(), Person{In: in, Out: &out, Open: open}, false); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := b.addresses(); !slices.Equal(got, []string{signInURL}) {
		t.Fatalf("opened %v", got)
	}
	if !strings.Contains(out.String(), fmt.Sprintf("port %d on this computer is in use", port)) || strings.Contains(out.String(), "the-code") ||
		!strings.Contains(out.String(), signInURL) || !strings.Contains(out.String(), "Paste code here if prompted > ") {
		t.Fatalf("output:\n%s", out.String())
	}

	login = newClaudeLogin(port)
	s, f := browserFixture(t, login)
	b = newBrowser()
	_, err = s.Run(context.Background(), Person{Out: io.Discard, Open: b.open("")}, false)
	if err == nil || !strings.Contains(err.Error(), "could not reach Claude Code on this computer") {
		t.Fatalf("err = %v", err)
	}
	if len(b.addresses()) != 0 {
		t.Fatalf("opened %v for a person who cannot paste", b.addresses())
	}
	if !strings.Contains(strings.Join(f.Ops(), " "), "VolumeRemove:"+Volume) {
		t.Fatalf("ops %v", f.Ops())
	}
	if _, err := modelsignin.Open(s.Home, nil).Get(modelsignin.Claude); err == nil {
		t.Fatal("a record was kept")
	}
}

// A Claude Code that hands its opener nothing still signs in by paste,
// after a wait.
func TestNoAddressFromTheOpenerFallsBackToPaste(t *testing.T) {
	prev := fallbackWait
	fallbackWait = 20 * time.Millisecond
	t.Cleanup(func() { fallbackWait = prev })
	login := newClaudeLogin(freePort(t))
	login.noOpener = true
	s, _ := browserFixture(t, login)
	b := newBrowser()
	in, open := b.pastes("the-code#abc")
	if _, err := s.Run(context.Background(), Person{In: in, Out: io.Discard, Open: open}, false); err != nil {
		t.Fatal(err)
	}
	if got := b.addresses(); !slices.Equal(got, []string{signInURL}) {
		t.Fatalf("opened %v", got)
	}
}

// Only Anthropic's sign-in whose redirect is Claude Code's own callback on
// localhost is relayed.
func TestOnlyClaudeCodesOwnCallbackIsRelayed(t *testing.T) {
	for raw, want := range map[string]int{
		autoURL(45437): 45437,
		"https://platform.claude.com/oauth/authorize?redirect_uri=http%3A%2F%2Flocalhost%3A50000%2Fcallback": 50000,
		"https://evil.example/oauth/authorize?redirect_uri=http%3A%2F%2Flocalhost%3A50000%2Fcallback":        0,
		"https://claude.com/x?redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback":      0,
		"https://claude.com/x?redirect_uri=http%3A%2F%2Fevil.example%3A50000%2Fcallback":                     0,
		"https://claude.com/x?redirect_uri=http%3A%2F%2Flocalhost%3A22%2Fcallback":                           0,
		"https://claude.com/x?redirect_uri=http%3A%2F%2Flocalhost%3A50000%2Fother":                           0,
		"https://claude.com/x?redirect_uri=http%3A%2F%2Flocalhost%2Fcallback":                                0,
	} {
		if got, ok := callbackPort(raw); got != want || ok != (want != 0) {
			t.Errorf("%s: %d %v", raw, got, ok)
		}
	}
}

// Agentry opens Anthropic's own sign-in pages from Claude Code's output and
// nothing else, finding an address split across writes; the rest of the
// output is shown as it comes.
func TestOnlyAnthropicsSignInPagesAreOpened(t *testing.T) {
	for raw, ok := range map[string]bool{
		signInURL: true, "https://platform.claude.com/oauth/authorize?x=1": true,
		"http://claude.com/cai/oauth/authorize": false, "https://claude.com.example/x": false,
		"https://user@claude.com/x": false, "https://evil.example/claude.com": false,
	} {
		if AnthropicSignInURL(raw) != ok {
			t.Errorf("%s: %v", raw, !ok)
		}
	}
	prev := fallbackWait
	fallbackWait = 10 * time.Millisecond
	t.Cleanup(func() { fallbackWait = prev })
	var opened []string
	var mu sync.Mutex
	var sink strings.Builder
	w := &browserSignIn{out: &sink, canPaste: true, open: func(u string) { mu.Lock(); opened = append(opened, u); mu.Unlock() }, fail: func(error) {}}
	s := &stream{b: w}
	for _, chunk := range []string{"see https://evil.example/x\nIf the browser didn't open, visit: https://claude", ".com/cai/oauth/authorize?state=1\n", "again " + signInURL + "\nPaste code here if prompted > "} {
		_, _ = s.Write([]byte(chunk))
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(opened, []string{"https://claude.com/cai/oauth/authorize?state=1"}) {
		t.Fatalf("opened %v", opened)
	}
	if !strings.Contains(sink.String(), "evil.example") || !strings.Contains(sink.String(), "Paste code here if prompted > ") {
		t.Fatalf("passed through %q", sink.String())
	}
}
