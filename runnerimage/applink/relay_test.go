package applink

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startRelay runs a relay on a socket in a temporary folder, its stdio held
// by the test as the CLI holds it. It returns the socket, the calls the
// relay sends out and a writer for the answers.
func startRelay(t *testing.T) (string, *bufio.Scanner, io.Writer) {
	t.Helper()
	dir, err := os.MkdirTemp("", "rl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "app.sock")
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Relay(ctx, RelayConfig{Listen: sock, In: inR, Out: outW}) }()
	t.Cleanup(func() {
		cancel()
		_ = inW.Close()
		go func() { _, _ = io.Copy(io.Discard, outR) }()
		select {
		case err := <-done:
			_ = err
		case <-time.After(5 * time.Second):
			t.Error("the relay did not stop")
		}
	})
	out := bufio.NewScanner(outR)
	if !out.Scan() {
		t.Fatal("the relay wrote no ready line")
	}
	if f, err := ReadFrame(out.Bytes()); err != nil || !f.Ready {
		t.Fatalf("the first line is not the ready line: %q", out.Text())
	}
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("the socket is %v, not 0600", fi.Mode().Perm())
	}
	return sock, out, inW
}

// A tool call made on the relay's socket goes out as one frame, and the
// answer the CLI sends back for its id is the connection's answer.
func TestRelayCarriesACallAndItsAnswer(t *testing.T) {
	sock, out, in := startRelay(t)
	got := make(chan Answer, 1)
	go func() {
		a, err := Call(context.Background(), sock, Request{Tool: SelfConfigure, Arguments: json.RawMessage(`{"name":"Sandra"}`)})
		if err != nil {
			t.Error(err)
		}
		got <- a
	}()
	if !out.Scan() {
		t.Fatal("no call went out")
	}
	f, err := ReadFrame(out.Bytes())
	if err != nil || f.ID == 0 || f.Request == nil || f.Request.Tool != SelfConfigure || string(f.Request.Arguments) != `{"name":"Sandra"}` {
		t.Fatalf("the call went out as %q", out.Text())
	}
	b, _ := json.Marshal(Frame{ID: f.ID, Answer: &Answer{Text: "saved"}})
	if _, err := in.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-got:
		if a.Text != "saved" || a.IsError {
			t.Fatalf("answer %+v", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no answer")
	}
}

// A line that is not a tool call, such as the owner socket's roster change,
// is refused at the relay's socket and never reaches the CLI.
func TestRelayRefusesALineThatIsNotAToolCall(t *testing.T) {
	sock, out, _ := startRelay(t)
	for _, line := range []string{
		`{"cmd":"change","entry":"share","on":true}`,
		`{"cmd":"send","text":"hello"}`,
		`{}`,
		`not json`,
	} {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(c)
		if !sc.Scan() {
			t.Fatalf("%s: no answer", line)
		}
		var a Answer
		if json.Unmarshal(sc.Bytes(), &a) != nil || !a.IsError || a.Text != refusedLine {
			t.Fatalf("%s: answered %q", line, sc.Text())
		}
		_ = c.Close()
	}
	// The next line out is the next call's, not one of the refused lines.
	go func() {
		_, _ = Call(context.Background(), sock, Request{Tool: SelfConfigure, Arguments: json.RawMessage(`{}`)})
	}()
	if !out.Scan() {
		t.Fatal("no call went out")
	}
	if f, err := ReadFrame(out.Bytes()); err != nil || f.Request == nil || f.Request.Tool != SelfConfigure {
		t.Fatalf("a refused line reached the CLI: %q", out.Text())
	}
}

// A call still waiting when the CLI's side ends is answered as not
// answered, rather than left hanging.
func TestRelayEndsAWaitingCallWhenTheCLIGoes(t *testing.T) {
	dir, err := os.MkdirTemp("", "rl")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	sock := filepath.Join(dir, "app.sock")
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Relay(context.Background(), RelayConfig{Listen: sock, In: inR, Out: outW}) }()
	out := bufio.NewScanner(outR)
	out.Scan()
	got := make(chan Answer, 1)
	go func() {
		a, _ := Call(context.Background(), sock, Request{Tool: SelfConfigure, Arguments: json.RawMessage(`{}`)})
		got <- a
	}()
	out.Scan()
	go func() { _, _ = io.Copy(io.Discard, outR) }()
	_ = inW.Close()
	select {
	case a := <-got:
		if !a.IsError || !strings.Contains(a.Text, "did not answer") {
			t.Fatalf("answer %+v", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting call was left hanging")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the relay did not stop when its stdin ended")
	}
}

func TestRelayOwnerIsUIDColonGID(t *testing.T) {
	for _, bad := range []string{"1000", "a:b", "-1:0", "1000:"} {
		if _, _, err := parseOwner(bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	if u, g, err := parseOwner("1000:1001"); err != nil || u != 1000 || g != 1001 {
		t.Fatalf("%d %d %v", u, g, err)
	}
}
