package signin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/mcpauth/mcpauthtest"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
)

// memKeys is a keychain in memory: the runhost's store in these tests.
type memKeys struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *memKeys) Get(a string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[a]
	if !ok {
		return "", errors.New("no such entry")
	}
	return v, nil
}

func (k *memKeys) Set(a, v string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[a] = v
	return nil
}

func (k *memKeys) Delete(a string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m, a)
	return nil
}

// local is a Caller that answers in process with the server's verbs, as
// the service does over the run key: the input stays open until the
// caller's send returns, and the handler's context is the service's.
type local struct{ s *Server }

func (l local) Call(ctx context.Context, verb string, args []string, send func(io.Writer) error, each func(runhost.Frame) error) (runhost.Answer, error) {
	if verb != "sign-in" {
		return runhost.Answer{}, errors.New("not a sign-in verb")
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		if send != nil {
			_ = send(inW)
		}
		_ = inW.Close()
	}()
	go func() {
		fw := runhost.NewFrameWriter(outW)
		var code int
		var err error
		switch {
		case len(args) >= 2 && args[0] == "start":
			app := ""
			if len(args) == 3 {
				app = args[2]
			}
			code, err = l.s.Start(context.Background(), args[1], app, inR, fw)
		case len(args) == 2 && args[0] == "relay":
			code, err = l.s.Relay(context.Background(), args[1], inR, fw)
		}
		_ = fw.Exit(code, err)
		_ = outW.Close()
		_ = inR.Close()
	}()
	type res struct {
		a   runhost.Answer
		err error
	}
	done := make(chan res, 1)
	go func() {
		a, err := runhost.ReadFrames(outR, each)
		done <- res{a, err}
	}()
	select {
	case <-ctx.Done():
		_ = inW.CloseWithError(ctx.Err())
		_ = outR.Close()
		return runhost.Answer{}, ctx.Err()
	case r := <-done:
		_ = outR.Close()
		return r.a, r.err
	}
}

// noFollow is a browser's HTTP client that stops at each redirect.
func noFollow(c *http.Client) *http.Client {
	n := *c
	n.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &n
}

func TestAChatGPTSignInIsMadeOnTheRunhostFromTheDevicesBrowser(t *testing.T) {
	v := mcpauthtest.NewChatGPT(t)
	flow := v.Flow()
	host := t.TempDir()
	keys := &memKeys{m: map[string]string{}}
	s := &Server{Home: host, Keys: keys, ChatGPT: func() mcpauth.ChatGPT {
		return mcpauth.ChatGPT{HTTP: flow.HTTP, Issuer: flow.Issuer, AuthorizeURL: flow.AuthorizeURL, TokenURL: flow.TokenURL,
			DiscoveryURL: flow.DiscoveryURL, APIBase: flow.APIBase, Timeout: 10 * time.Second}
	}}
	t.Cleanup(modelsignin.RegisterVendor(modelsignin.ChatGPT, modelsignin.ChatGPTVendor{Flow: flow}))
	prev := chatgptHost
	chatgptHost = hostOf(v.URL)
	t.Cleanup(func() { chatgptHost = prev })

	// The device listens on a port of its own here, since the runhost's
	// flow holds the callback's port on this same machine; the browser is
	// sent there in its place.
	var deviceAddr string
	var opened []string
	browser := noFollow(v.Client())
	p := Person{
		Listen: func(network, _ string) (net.Listener, error) {
			l, err := net.Listen(network, "127.0.0.1:0")
			if err == nil {
				deviceAddr = l.Addr().String()
			}
			return l, err
		},
		Open: func(u string) {
			opened = append(opened, u)
			go func() {
				res, err := browser.Get(u)
				if err != nil {
					return
				}
				_ = res.Body.Close()
				loc, err := url.Parse(res.Header.Get("Location"))
				if err != nil {
					return
				}
				loc.Host = deviceAddr
				if res, err := http.Get(loc.String()); err == nil { //nolint:noctx // a test's browser
					_ = res.Body.Close()
				}
			}()
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	k, err := Run(ctx, local{s}, modelsignin.ChatGPT, "", p)
	if err != nil {
		t.Fatal(err)
	}
	if k.Vendor != modelsignin.ChatGPT || k.Label != v.Email {
		t.Fatalf("kept %+v", k)
	}
	if len(opened) != 1 || !strings.HasPrefix(opened[0], v.URL) {
		t.Fatalf("the device opened %v", opened)
	}
	// The refresh token is in the runhost's store and in no file.
	if len(v.Issued) != 1 {
		t.Fatalf("issued %v", v.Issued)
	}
	if got, err := keys.Get("model-signin:chatgpt"); err != nil || !strings.Contains(got, v.Issued[0]) {
		t.Fatalf("the store keeps %q (%v)", got, err)
	}
	_ = filepath.Walk(host, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			if b, _ := os.ReadFile(path); bytes.Contains(b, []byte(v.RefreshPrefix)) {
				t.Errorf("%s holds the refresh token", path)
			}
		}
		return nil
	})
	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].Vendor != modelsignin.ChatGPT {
		t.Fatalf("list %+v (%v)", list, err)
	}
	if err := s.HasSignIn("chatgpt-plan"); err != nil {
		t.Fatal(err)
	}
	if err := s.HasSignIn("copilot-signin"); err == nil {
		t.Fatal("a Copilot sign-in the runhost does not keep was taken")
	}
	r, err := s.Remove(ctx, modelsignin.ChatGPT, io.Discard)
	if err != nil || !r.Removed {
		t.Fatalf("removed %+v (%v)", r, err)
	}
	if _, err := keys.Get("model-signin:chatgpt"); err == nil {
		t.Fatal("the refresh token stayed in the store")
	}
	if v.Live(v.Issued[0]) {
		t.Fatal("the removed sign-in was not revoked")
	}
}

func TestACopilotSignInShowsTheCodeOnTheDevice(t *testing.T) {
	t.Setenv("AGENTRY_GITHUB_APP_CLIENT_ID", mcpauthtest.GitHubClientID)
	v := mcpauthtest.NewGitHub(t)
	flow := v.Flow()
	keys := &memKeys{m: map[string]string{}}
	s := &Server{Home: t.TempDir(), Keys: keys, GitHub: func() mcpauth.GitHub {
		f := flow
		f.Present = nil
		return f
	}}
	prev := githubDevicePage
	githubDevicePage = flow.DevicePage
	t.Cleanup(func() { githubDevicePage = prev })
	var page, code string
	p := Person{Code: func(pg, c string) { page, code = pg, c }, Open: func(string) {}}
	k, err := Run(context.Background(), local{s}, modelsignin.Copilot, "", p)
	if err != nil {
		t.Fatal(err)
	}
	if k.Label != v.Login || code != "WDJB-MJHT" || page != flow.DevicePage {
		t.Fatalf("kept %+v, shown %q at %q", k, code, page)
	}
	if got, _ := keys.Get("model-signin:copilot"); !strings.Contains(got, v.RefreshPrefix) {
		t.Fatalf("the store keeps %q", got)
	}
}

func TestTheDeviceOpensOnlyTheVendorsOwnPage(t *testing.T) {
	claude := "https://claude.com/cai/oauth/authorize?redirect_uri=" + url.QueryEscape("http://localhost:41234/callback")
	chatgpt := "https://auth.openai.com/api/accounts/authorize?redirect_uri=" + url.QueryEscape("http://127.0.0.1:41235/auth/callback")
	for _, c := range []struct {
		vendor, addr string
		o            runhost.Open
		ok           bool
	}{
		{modelsignin.Claude, claude, runhost.Open{Relay: "r", Port: 41234}, true},
		{modelsignin.Claude, claude, runhost.Open{Relay: "r", Port: 22}, false},
		{modelsignin.Claude, "https://evil.example/?redirect_uri=" + url.QueryEscape("http://localhost:41234/callback"), runhost.Open{Relay: "r", Port: 41234}, false},
		{modelsignin.ChatGPT, chatgpt, runhost.Open{Relay: "r", Port: 41235}, true},
		{modelsignin.ChatGPT, chatgpt, runhost.Open{Relay: "r", Port: 41234}, false},
		{modelsignin.ChatGPT, claude, runhost.Open{Relay: "r", Port: 41234}, false},
		{modelsignin.Copilot, mcpauth.GitHubDevicePage, runhost.Open{Code: "ABCD-EFGH"}, true},
		{modelsignin.Copilot, "https://github.com.evil.example/login/device", runhost.Open{Code: "ABCD-EFGH"}, false},
		{modelsignin.Copilot, mcpauth.GitHubDevicePage, runhost.Open{Relay: "r", Port: 41234}, false},
	} {
		if got := vendorPage(c.vendor, c.addr, c.o); got != c.ok {
			t.Errorf("%s %s %+v: %v, want %v", c.vendor, c.addr, c.o, got, c.ok)
		}
	}
}

func TestAClaudeRelayReachesTheLoginContainerAlone(t *testing.T) {
	fake := &dockerx.Fake{}
	fake.Exec = func(_ context.Context, container string, argv []string, stdin io.Reader, stdout io.Writer) (int, error) {
		b, _ := io.ReadAll(stdin)
		_, _ = stdout.Write(append([]byte(container+" "+argv[len(argv)-1]+" got "), b...))
		return 0, nil
	}
	s := &Server{Home: t.TempDir(), Docker: func() dockerx.Client { return fake }}
	if code, err := s.Relay(context.Background(), "nobody", strings.NewReader(""), runhost.NewFrameWriter(io.Discard)); err == nil || code == 0 {
		t.Fatal("a relay with no sign-in under way was taken")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.active = &active{id: "abc", vendor: modelsignin.Claude, ctx: ctx, container: "agentry-claude-login-x", port: 41234, slots: make(chan struct{}, MaxRelayed)}
	if _, err := s.Relay(context.Background(), "abd", strings.NewReader(""), runhost.NewFrameWriter(io.Discard)); err == nil {
		t.Fatal("a relay naming another sign-in was taken")
	}
	var out bytes.Buffer
	code, err := s.Relay(context.Background(), "abc", strings.NewReader("GET /callback?code=x HTTP/1.1\r\n\r\n"), runhost.NewFrameWriter(&out))
	if err != nil || code != 0 {
		t.Fatal(code, err)
	}
	var got []byte
	_, _ = runhost.ReadFrames(io.MultiReader(&out, strings.NewReader(`{"type":"exit"}`+"\n")), func(f runhost.Frame) error {
		got = append(got, f.Data...)
		return nil
	})
	if string(got) != "agentry-claude-login-x 41234 got GET /callback?code=x HTTP/1.1\r\n\r\n" {
		t.Fatalf("relayed %q", got)
	}
}

func TestOneSignInAtATime(t *testing.T) {
	s := &Server{Home: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.active = &active{id: "abc", ctx: ctx}
	if _, err := s.Start(context.Background(), modelsignin.ChatGPT, "", strings.NewReader(""), runhost.NewFrameWriter(io.Discard)); err == nil || !strings.Contains(err.Error(), "another sign-in") {
		t.Fatalf("a second sign-in: %v", err)
	}
	if _, err := s.Start(context.Background(), "gemini", "", strings.NewReader(""), runhost.NewFrameWriter(io.Discard)); err == nil {
		t.Fatal("a vendor a runhost does not sign in to was taken")
	}
}
