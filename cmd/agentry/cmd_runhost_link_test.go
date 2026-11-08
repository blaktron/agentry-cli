package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/desktoplink"
)

// linkTestJob is a running job whose run is bound to device a's Agentry
// Desktop, with its link socket served in a short temporary folder (a Unix
// socket's path is bounded).
func linkTestJob(t *testing.T) (*job.Job, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "rl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	j := &job.Job{V: job.Version, ID: "0123456789abcdef0123456789abcdef", Device: "device-a",
		Run: job.Run{Desktop: &job.Desktop{Run: "0123456789abcdef0123456789abcdef", Bound: desktop.Bound{Token: "tok-123456",
			Servers: map[string][]desktoplink.Tool{"blender": {{Name: "get_scene_info"}}}}}}}
	l := newRunhostLive(j, func() {})
	lk, sock, err := openRunhostLink(context.Background(), j, dir)
	if err != nil {
		t.Fatal(err)
	}
	l.link = lk
	t.Cleanup(func() {
		lk.close()
		l.forget()
	})
	return j, sock
}

// linkCall sends one message on the job's link socket and reads the answer.
func linkCall(t *testing.T, sock string, m desktop.Message) desktop.Message {
	t.Helper()
	ans, err := desktop.Client{Socket: sock}.Forward(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	return ans
}

// device is a device's side of a link session, driven by hand.
type linkDevice struct {
	in     *io.PipeWriter
	frames chan runhost.Frame
	code   chan int
}

func openLinkSession(t *testing.T, req runhost.Request) *linkDevice {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	d := &linkDevice{in: inW, frames: make(chan runhost.Frame, 16), code: make(chan int, 1)}
	go func() {
		code, _ := runhostLinkVerb(context.Background(), req, inR, runhost.NewFrameWriter(outW))
		_ = outW.Close()
		d.code <- code
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 64<<10), runhost.MaxLine)
		for sc.Scan() {
			var f runhost.Frame
			if json.Unmarshal(sc.Bytes(), &f) == nil {
				d.frames <- f
			}
		}
		close(d.frames)
	}()
	return d
}

func (d *linkDevice) next(t *testing.T) runhost.Frame {
	t.Helper()
	select {
	case f, ok := <-d.frames:
		if !ok {
			t.Fatal("the link session ended")
		}
		return f
	case <-time.After(10 * time.Second):
		t.Fatal("no frame from the runhost")
	}
	return runhost.Frame{}
}

func TestTheLinkChannelIsASessionTheDeviceOpened(t *testing.T) {
	j, _ := linkTestJob(t)
	ctx := context.Background()
	fw := runhost.NewFrameWriter(io.Discard)
	// A key that names no device, another device and a job the runhost does
	// not run are each refused before anything is linked.
	if code, err := runhostLinkVerb(ctx, runhost.Request{Verb: "link", Args: []string{j.ID}}, nil, fw); code != engine.ExitUsage || err == nil {
		t.Fatalf("a key naming no device: exit %d, %v", code, err)
	}
	if code, err := runhostLinkVerb(ctx, runhost.Request{Verb: "link", Args: []string{j.ID}, Device: "device-b"}, nil, fw); code != engine.ExitUsage || err == nil {
		t.Fatalf("another device: exit %d, %v", code, err)
	}
	if code, _ := runhostLinkVerb(ctx, runhost.Request{Verb: "link", Args: []string{"ffffffffffffffffffffffffffffffff"}, Device: "device-a"}, nil, fw); code != runhost.LinkExitNoJob {
		t.Fatalf("a job not running: exit %d", code)
	}
	if !runhost.IsVerb("link") {
		t.Fatal("serve does not relay the link verb")
	}
}

func TestARunhostLinkCarriesOnlyToolCallsToTheDevice(t *testing.T) {
	j, sock := linkTestJob(t)
	d := openLinkSession(t, runhost.Request{Verb: "link", Args: []string{j.ID}, Device: j.Device})
	if f := d.next(t); f.Type != runhost.FrameStarted || f.Text != j.ID {
		t.Fatalf("the link opened with %+v", f)
	}
	// A bind is never carried: it is refused on the runhost.
	if ans := linkCall(t, sock, desktop.Message{Bind: &desktop.BindRequest{Run: "x"}}); ans.Refused == nil || ans.Refused.Reason != "not-a-message" {
		t.Fatalf("a bind on the job's socket: %+v", ans)
	}
	// A tool call crosses as a link frame, and the device's answer comes back.
	got := make(chan desktop.Message, 1)
	go func() {
		got <- linkCall(t, sock, desktop.Message{Call: &desktop.CallRequest{Run: "0123456789abcdef0123456789abcdef", Token: "tok-123456", Server: "blender", Tool: "get_scene_info"}})
	}()
	f := d.next(t)
	if f.Type != runhost.FrameLink {
		t.Fatalf("the call crossed as %+v", f)
	}
	m, err := desktop.ReadMessage(f.Result)
	if err != nil || m.Call == nil || m.Call.Tool != "get_scene_info" || m.Bind != nil {
		t.Fatalf("the link frame carried %s (%v)", f.Result, err)
	}
	// What the device answers is held to an answer: a bind it slips in is
	// dropped on the runhost.
	answer, _ := json.Marshal(desktop.Message{Answer: &desktoplink.Answer{Content: []desktoplink.Content{{Type: "text", Text: "a cube"}}},
		Bind: &desktop.BindRequest{Run: "x"}})
	if err := json.NewEncoder(d.in).Encode(runhost.LinkAnswer{ID: f.Text, Message: answer}); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-got:
		if a.Answer == nil || a.Answer.Content[0].Text != "a cube" || a.Bind != nil {
			t.Fatalf("the run got %+v", a)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the device's answer did not reach the run")
	}
	// The device leaves: the session ends, and the next call is refused
	// desktop-unreachable at once.
	_ = d.in.Close()
	select {
	case code := <-d.code:
		if code != 0 {
			t.Fatalf("the session ended with exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the session did not end when the device left")
	}
	start := time.Now()
	ans := linkCall(t, sock, desktop.Message{Call: &desktop.CallRequest{Run: "0123456789abcdef0123456789abcdef", Token: "tok-123456", Server: "blender", Tool: "get_scene_info"}})
	if ans.Refused == nil || ans.Refused.Reason != receipt.DesktopUnreachable || time.Since(start) > 5*time.Second {
		t.Fatalf("a call after the device left: %+v after %s", ans, time.Since(start))
	}
	// Attach opens it again.
	d2 := openLinkSession(t, runhost.Request{Verb: "link", Args: []string{j.ID}, Device: j.Device})
	if f := d2.next(t); f.Type != runhost.FrameStarted {
		t.Fatalf("the second link opened with %+v", f)
	}
	_ = d2.in.Close()
}

func TestARunhostLinkWaitsForTheFirstSessionThenRefuses(t *testing.T) {
	old := runhostLinkFirstWait
	runhostLinkFirstWait = 200 * time.Millisecond
	t.Cleanup(func() { runhostLinkFirstWait = old })
	_, sock := linkTestJob(t)
	ans := linkCall(t, sock, desktop.Message{Call: &desktop.CallRequest{Run: "0123456789abcdef0123456789abcdef", Token: "tok-123456", Server: "blender", Tool: "get_scene_info"}})
	if ans.Refused == nil || ans.Refused.Reason != receipt.DesktopUnreachable {
		t.Fatalf("a call with no device linked: %+v", ans)
	}
}

func TestARunhostJobRelaysTheDesktopBindingToItsRun(t *testing.T) {
	j := &job.Job{ID: "0123456789abcdef0123456789abcdef", Device: "device-a", Release: job.Release{Namespace: "ns", Name: "n", Version: "1.0.0", Registry: "https://r", Digest: "sha256:0"},
		Run: job.Run{MaxTurns: -1, MaxCost: -1, Desktop: &job.Desktop{Run: "r1", Bound: desktop.Bound{Token: "tok-abcdef"}}}}
	args := runhostRunArgs(j, runhostStaged{desktop: "/j/desktop.json"})
	found := false
	for i, a := range args {
		if a == "--runhost-desktop" && i+1 < len(args) && args[i+1] == "/j/desktop.json" {
			found = true
		}
		if a == "tok-abcdef" {
			t.Fatal("the run token is on the command line")
		}
	}
	if !found {
		t.Fatalf("the run is not handed the binding: %v", args)
	}
	values := runhostJobValues(j)
	masked := false
	for _, v := range values {
		if v == "tok-abcdef" {
			masked = true
		}
	}
	if !masked {
		t.Fatal("the run token is not among the values the narration is masked of")
	}
	r := runhostRelayedDesktop(j, "/j/link.sock")
	if r.Run != "r1" || r.Socket != "/j/link.sock" || r.Bound.Token != "tok-abcdef" {
		t.Fatalf("relayed %+v", r)
	}
}

// A runhost's agents bind over the runner home's link socket, which the
// service serves: with no device linked a bind is refused as the person's
// computer away; a device that links carries an agent's lazy bind and its
// calls; a run's bind (not lazy, no agent) never crosses.
func TestARunhostsAgentsReachTheLinkedDevice(t *testing.T) {
	home, err := os.MkdirTemp("", "ah")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	l, err := openAgentsLink(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	old := runhostAgents
	runhostAgents = l
	t.Cleanup(func() { runhostAgents = old })
	sock := desktop.SocketPath(home)
	lazy := desktop.Message{Bind: &desktop.BindRequest{Run: "seg-1", Agent: "Assistant", Lazy: true, Servers: []desktop.BindServer{{ID: "blender"}}}}
	if ans := linkCall(t, sock, lazy); ans.Refused == nil || ans.Refused.Reason != receipt.DesktopUnreachable {
		t.Fatalf("an agent's bind with no device linked: %+v", ans)
	}
	if code, err := runhostLinkVerb(ctx, runhost.Request{Verb: "link", Args: []string{"agents"}}, nil, runhost.NewFrameWriter(io.Discard)); code != engine.ExitUsage || err == nil {
		t.Fatalf("an agents link from a key naming no device: %d %v", code, err)
	}
	d := openLinkSession(t, runhost.Request{Verb: "link", Args: []string{"agents"}, Device: "device-a"})
	if f := d.next(t); f.Type != runhost.FrameStarted || f.Text != "agents" {
		t.Fatalf("the agents link opened with %+v", f)
	}
	if ans := linkCall(t, sock, desktop.Message{Bind: &desktop.BindRequest{Run: "run-1", Servers: []desktop.BindServer{{ID: "blender"}}}}); ans.Refused == nil || ans.Refused.Reason != "not-a-message" {
		t.Fatalf("a run's bind on the agents link: %+v", ans)
	}
	got := make(chan desktop.Message, 1)
	go func() { got <- linkCall(t, sock, lazy) }()
	f := d.next(t)
	m, err := desktop.ReadMessage(f.Result)
	if f.Type != runhost.FrameLink || err != nil || m.Bind == nil || !m.Bind.Lazy || m.Bind.Agent != "Assistant" {
		t.Fatalf("the bind crossed as %+v (%v)", f, err)
	}
	answer, _ := json.Marshal(desktop.Message{Bound: &desktop.Bound{Token: "t1"}})
	if err := json.NewEncoder(d.in).Encode(runhost.LinkAnswer{ID: f.Text, Message: answer}); err != nil {
		t.Fatal(err)
	}
	if a := <-got; a.Bound == nil || a.Bound.Token != "t1" {
		t.Fatalf("the agent got %+v", a)
	}
	_ = d.in.Close()
}
