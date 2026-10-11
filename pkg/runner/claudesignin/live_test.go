package claudesignin

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// syncWriter is a strings.Builder safe for the two output streams.
type syncWriter struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// The real Claude Code image signs in through the browser alone
// (agentry-cli#848), on a real daemon, with no account: the test is the
// browser and answers Claude Code's callback with a code Anthropic will
// refuse. That the refusal comes back as Claude Code's own "Login failed"
// proves the address reached the opener, the relay reached Claude Code's
// callback in the container, and Claude Code exchanged the code itself.
// It builds the image when it is not here (network: npm and the base).
//
//	AGENTRY_LIVE_CLAUDE_SIGNIN=1 go test -run TestLiveClaudeImageBrowserSignIn ./pkg/runner/claudesignin
func TestLiveClaudeImageBrowserSignIn(t *testing.T) {
	if os.Getenv("AGENTRY_LIVE_CLAUDE_SIGNIN") == "" {
		t.Skip("set AGENTRY_LIVE_CLAUDE_SIGNIN=1 to run the real Claude Code image's sign-in on this daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	s := SignIn{Home: t.TempDir(), Policy: policy.Default(), Docker: func() dockerx.Client { return dockerx.New() }}
	var out syncWriter
	answered := make(chan int, 1)
	opened := make(chan string, 1)
	open := func(u string) {
		opened <- u
		port, ok := callbackPort(u)
		if !ok {
			return
		}
		go func() {
			au, _ := url.Parse(u)
			c := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := c.Get("http://localhost:" + strconv.Itoa(port) + "/callback?code=agentry-live-check-not-a-code&state=" + url.QueryEscape(au.Query().Get("state")))
			if err != nil {
				t.Logf("the browser's callback: %v", err)
				answered <- 0
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			answered <- resp.StatusCode
		}()
	}
	_, err := s.Run(ctx, Person{Out: &out, Open: open}, false)
	t.Logf("Claude Code and the sign-in said:\n%s", out.String())
	select {
	case u := <-opened:
		if _, ok := callbackPort(u); !ok {
			t.Fatalf("opened %s, not Claude Code's own callback address", u)
		}
	default:
		t.Fatal("nothing was opened")
	}
	if err == nil || !strings.Contains(err.Error(), "did not complete") {
		t.Fatalf("a refused code was kept? err = %v", err)
	}
	if !strings.Contains(out.String(), "Login failed") {
		t.Fatalf("Claude Code never exchanged the relayed code")
	}
	t.Logf("the browser's callback was answered %d; err = %v", <-answered, err)
	_ = s.docker().VolumeRemove(context.Background(), Volume)
}
