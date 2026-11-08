package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/desktoplink"
)

// fakeApp is Agentry Desktop's link service, recording what reaches it.
type fakeApp struct {
	mu    sync.Mutex
	calls []desktop.CallRequest
	binds int
}

func (a *fakeApp) Bind(context.Context, desktop.BindRequest) (desktop.Bound, *desktop.Refusal) {
	a.mu.Lock()
	a.binds++
	a.mu.Unlock()
	return desktop.Bound{Token: "t"}, nil
}

func (a *fakeApp) Call(_ context.Context, req desktop.CallRequest) (desktoplink.Answer, *desktop.Refusal) {
	a.mu.Lock()
	a.calls = append(a.calls, req)
	a.mu.Unlock()
	return desktoplink.TextAnswer("a cube", false), nil
}

func (a *fakeApp) Unbind(context.Context, desktop.UnbindRequest) *desktop.Refusal { return nil }

func serveFakeApp(t *testing.T) (*fakeApp, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ra")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "desktop.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	app := &fakeApp{}
	go func() { _ = desktop.Serve(ctx, ln, app) }()
	return app, sock
}

// The device keeps a job's link open, and of what the runhost sends on it
// forwards only the bound run's tool calls to this computer's Agentry
// Desktop: a bind, or a call for another run, is refused on the device and
// never reaches the app.
func TestTheDeviceForwardsOnlyTheBoundRunsCalls(t *testing.T) {
	app, sock := serveFakeApp(t)
	asked := 0
	answers := make(chan map[string]desktop.Message, 1)
	l := newLabWith(t, map[string]runhost.Handler{
		"link": func(_ context.Context, req runhost.Request, in io.Reader, fw *runhost.FrameWriter) (int, error) {
			asked++
			if asked == 1 {
				// The job has not started yet: the device asks again.
				return runhost.LinkExitNoJob, errors.New("no such job yet")
			}
			_ = fw.Started(req.Args[0])
			send := map[string]desktop.Message{
				"1": {Call: &desktop.CallRequest{Run: "run-1", Token: "t", Server: "blender", Tool: "get_scene_info"}},
				"2": {Bind: &desktop.BindRequest{Run: "run-1"}},
				"3": {Call: &desktop.CallRequest{Run: "run-2", Token: "t", Server: "blender", Tool: "get_scene_info"}},
			}
			for id, m := range send {
				b, _ := json.Marshal(m)
				_ = fw.Frame(runhost.Frame{Type: runhost.FrameLink, Text: id, Result: b})
			}
			got := map[string]desktop.Message{}
			dec := json.NewDecoder(in)
			for len(got) < len(send) {
				var a runhost.LinkAnswer
				if err := dec.Decode(&a); err != nil {
					return 1, err
				}
				m, err := desktop.ReadMessage(a.Message)
				if err != nil {
					return 1, err
				}
				got[a.ID] = m
			}
			answers <- got
			return 0, nil
		},
	})
	old := LinkRetry
	LinkRetry = 10 * time.Millisecond
	t.Cleanup(func() { LinkRetry = old })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := runhost.DialRunhost(ctx, l.rec, l.keys)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	linked := 0
	if err := KeepLink(ctx, LinkDialerFrom(c, &l.rec, l.keys), "0123456789abcdef0123456789abcdef", SocketForward(sock, "run-1"), func() { linked++ }); err != nil {
		t.Fatal(err)
	}
	got := <-answers
	if linked != 1 || asked != 2 {
		t.Fatalf("linked %d times after %d asks", linked, asked)
	}
	if a := got["1"]; a.Answer == nil || a.Answer.Content[0].Text != "a cube" {
		t.Fatalf("the bound run's call was answered %+v", a)
	}
	for _, id := range []string{"2", "3"} {
		if a := got[id]; a.Refused == nil || a.Refused.Reason != "not-a-message" {
			t.Fatalf("message %s was answered %+v", id, a)
		}
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.calls) != 1 || app.calls[0].Run != "run-1" || app.binds != 0 {
		t.Fatalf("the app was reached by %+v and %d binds", app.calls, app.binds)
	}
}

// With this computer's Agentry Desktop gone, a relayed call is refused
// desktop-unreachable.
func TestACallToAClosedAppIsUnreachable(t *testing.T) {
	ans := SocketForward(filepath.Join(t.TempDir(), "none.sock"), "run-1")(context.Background(),
		desktop.Message{Call: &desktop.CallRequest{Run: "run-1", Token: "t", Server: "blender", Tool: "look"}})
	if ans.Refused == nil || ans.Refused.Reason != receipt.DesktopUnreachable {
		t.Fatalf("answered %+v", ans)
	}
}

// A binding held in this process answers the bound run's calls and unbind,
// and nothing else reaches it.
func TestABindingInProcessAnswersOnlyTheBoundRun(t *testing.T) {
	calls, unbinds := 0, 0
	link := &engine.DesktopLink{Token: "t",
		Call: func(context.Context, desktop.CallRequest) (desktoplink.Answer, error) {
			calls++
			return desktoplink.TextAnswer("ok", false), nil
		},
		Unbind: func(context.Context) error { unbinds++; return nil },
	}
	fw := BindingForward(link, "run-1")
	ctx := context.Background()
	if a := fw(ctx, desktop.Message{Call: &desktop.CallRequest{Run: "run-1", Token: "t", Tool: "look"}}); a.Answer == nil {
		t.Fatalf("the bound run's call: %+v", a)
	}
	for _, m := range []desktop.Message{
		{Call: &desktop.CallRequest{Run: "run-2", Token: "t"}},
		{Call: &desktop.CallRequest{Run: "run-1", Token: "other"}},
		{Bind: &desktop.BindRequest{Run: "run-1"}},
	} {
		if a := fw(ctx, m); a.Refused == nil {
			t.Fatalf("%+v was answered %+v", m, a)
		}
	}
	if a := fw(ctx, desktop.Message{Unbind: &desktop.UnbindRequest{Run: "run-1", Token: "t"}}); !a.OK {
		t.Fatalf("the unbind: %+v", a)
	}
	if calls != 1 || unbinds != 1 {
		t.Fatalf("%d calls and %d unbinds reached the binding", calls, unbinds)
	}
}

// The device's agents link binds an agent's segment here, keeps the
// binding by its run, answers its calls with it and ends it at its unbind
// or when the link is given up; a run's bind never reaches the app.
func TestTheAgentsLinkBindsAgentsHereAlone(t *testing.T) {
	var binds []desktop.BindRequest
	unbinds := 0
	bind := func(_ context.Context, req desktop.BindRequest) (*engine.DesktopLink, error) {
		binds = append(binds, req)
		return &engine.DesktopLink{Token: "t-" + req.Run,
			Call: func(context.Context, desktop.CallRequest) (desktoplink.Answer, error) {
				return desktoplink.TextAnswer("ok", false), nil
			},
			Unbind: func(context.Context) error { unbinds++; return nil },
		}, nil
	}
	fw, closeAll := AgentsForward(bind)
	ctx := context.Background()
	if a := fw(ctx, desktop.Message{Bind: &desktop.BindRequest{Run: "run-1"}}); a.Refused == nil || len(binds) != 0 {
		t.Fatalf("a run's bind: %+v, %d binds", a, len(binds))
	}
	a := fw(ctx, desktop.Message{Bind: &desktop.BindRequest{Run: "seg-1", Agent: "Assistant", Lazy: true, Reports: "/elsewhere"}})
	if a.Bound == nil || a.Bound.Token != "t-seg-1" || binds[0].Reports != "" {
		t.Fatalf("an agent's bind: %+v, %+v", a, binds)
	}
	if a := fw(ctx, desktop.Message{Call: &desktop.CallRequest{Run: "seg-1", Token: "t-seg-1", Tool: "look"}}); a.Answer == nil {
		t.Fatalf("the agent's call: %+v", a)
	}
	if a := fw(ctx, desktop.Message{Call: &desktop.CallRequest{Run: "seg-9", Token: "t-seg-1", Tool: "look"}}); a.Refused == nil {
		t.Fatalf("a call of a run not bound here: %+v", a)
	}
	fw(ctx, desktop.Message{Bind: &desktop.BindRequest{Run: "seg-2", Agent: "Assistant", Lazy: true}})
	if a := fw(ctx, desktop.Message{Unbind: &desktop.UnbindRequest{Run: "seg-1", Token: "t-seg-1"}}); !a.OK || unbinds != 1 {
		t.Fatalf("the unbind: %+v, %d", a, unbinds)
	}
	closeAll()
	if unbinds != 2 {
		t.Fatalf("giving the link up left %d bindings", 2-unbinds)
	}
}
