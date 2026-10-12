package hostlink

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTerm stands in for ssh -tt and the host's pseudo-terminal: it echoes
// what is typed (but a password), runs a line on a return, and knows a few
// commands: sudo (asks for the password, with echo off), sleep (runs until
// ctrl-c), big N (prints N numbered lines), color, printpw (prints the
// run's password back), exit.
type fakeTerm struct {
	argv, env []string
	pr        *io.PipeReader
	pw        *io.PipeWriter
	queue     chan string
	mu        sync.Mutex
	line      []byte
	password  bool
	running   bool
	done      chan struct{}
	once      sync.Once
	exit      int
}

// endOfOutput, queued, ends the shell once what came before it is printed.
const endOfOutput = "\x00end"

func newFakeTerm(argv, env []string) *fakeTerm {
	pr, pw := io.Pipe()
	f := &fakeTerm{argv: argv, env: env, pr: pr, pw: pw, queue: make(chan string, 4096), done: make(chan struct{})}
	go func() {
		for s := range f.queue {
			if s == endOfOutput {
				f.end(0)
				return
			}
			if _, err := f.pw.Write([]byte(s)); err != nil {
				return
			}
		}
	}()
	f.print("Welcome to box.example\r\n\x1b[?2004hagent@box:~$ ")
	return f
}

// print queues output, printed in order.
func (f *fakeTerm) print(s string) {
	select {
	case f.queue <- s:
	case <-f.done:
	}
}

func (f *fakeTerm) Read(b []byte) (int, error) { return f.pr.Read(b) }

func (f *fakeTerm) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range b {
		switch c {
		case 0x03:
			if f.running {
				f.running = false
				f.print("^C\r\nagent@box:~$ ")
			}
		case '\r', '\n':
			line := string(f.line)
			f.line = nil
			if f.password {
				f.password = false
				if line == testSudo {
					f.print("\r\nok, as root\r\nagent@box:~$ ")
				} else {
					f.print("\r\nSorry, try again.\r\nagent@box:~$ ")
				}
				continue
			}
			f.print("\r\n")
			f.run(line)
		default:
			f.line = append(f.line, c)
			if !f.password && !f.running {
				f.print(string(c))
			}
		}
	}
	return len(b), nil
}

func (f *fakeTerm) run(line string) {
	switch {
	case strings.HasPrefix(line, "sudo "):
		f.password = true
		f.print("[sudo] password for agent: ")
	case strings.HasPrefix(line, "sleep"):
		f.running = true
	case strings.HasPrefix(line, "big "):
		var n int
		_, _ = fmt.Sscanf(line, "big %d", &n)
		var b strings.Builder
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&b, "line %06d\r\n", i)
		}
		b.WriteString("agent@box:~$ ")
		f.print(b.String())
	case line == "color":
		f.print("\x1b[1;31mred\x1b[0m and \x1b]0;title\x07plain\r\n10%\r\x1b[K100%\r\nagent@box:~$ ")
	case line == "printpw":
		f.print("the password is " + testPassword + "\r\nagent@box:~$ ")
	case line == "exit":
		f.print("logout\r\n")
		f.print(endOfOutput)
	default:
		f.print("agent@box:~$ ")
	}
}

func (f *fakeTerm) end(code int) {
	f.once.Do(func() {
		f.exit = code
		_ = f.pw.Close()
		close(f.done)
	})
}

func (f *fakeTerm) Wait() (int, error) {
	<-f.done
	return f.exit, nil
}

func (f *fakeTerm) Kill() { f.end(-1) }

// liveServer is a host link serving over pipes, a call at a time.
type liveServer struct {
	t     *testing.T
	in    *io.PipeWriter
	out   *bufio.Scanner
	id    int
	terms []*fakeTerm
	mu    sync.Mutex
	done  chan error
}

func (fx *fixture) serveLive(t *testing.T) *liveServer {
	t.Helper()
	ls := &liveServer{t: t, done: make(chan error, 1)}
	fx.cfg.Shell = func(argv, env []string) (Terminal, error) {
		f := newFakeTerm(argv, env)
		ls.mu.Lock()
		ls.terms = append(ls.terms, f)
		ls.mu.Unlock()
		return f, nil
	}
	if fx.cfg.ShellSettle == 0 {
		fx.cfg.ShellSettle = 100 * time.Millisecond
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ls.in = inW
	ls.out = bufio.NewScanner(outR)
	ls.out.Buffer(nil, 8<<20)
	go func() {
		ls.done <- Serve(context.Background(), inR, outW, io.Discard, fx.cfg)
		_ = outW.Close()
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		select {
		case <-ls.done:
		case <-time.After(10 * time.Second):
			t.Error("the host link did not end when its input closed")
		}
	})
	return ls
}

// call makes one tool call and returns its text and whether it is an error.
func (ls *liveServer) call(tool string, args any) (string, bool) {
	ls.t.Helper()
	ls.id++
	if _, err := fmt.Fprintf(ls.in, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":%s}`+"\n", ls.id, call(tool, args)); err != nil {
		ls.t.Fatal(err)
	}
	if !ls.out.Scan() {
		ls.t.Fatalf("no answer to %s: %v", tool, ls.out.Err())
	}
	var r struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(ls.out.Bytes(), &r); err != nil || r.Result == nil {
		ls.t.Fatalf("answer %s: %v", ls.out.Bytes(), err)
	}
	text, isErr, _ := answer(ls.t, r.Result)
	return text, isErr
}

func sudoTarget() Target {
	t := passwordTarget()
	t.ID = "server"
	return t
}

// TestASudoPromptIsAnsweredWithAPasswordTheAgentNeverSees: the shell's
// sudo asks, sudo-password answers, and neither the results, the
// transcript nor the log hold the password.
func TestASudoPromptIsAnsweredWithAPasswordTheAgentNeverSees(t *testing.T) {
	fx := newFixture(t, sudoTarget())
	ls := fx.serveLive(t)
	out, isErr := ls.call(ToolShellOpen, map[string]any{})
	if isErr || !strings.Contains(out, "[session 1: a login shell on server as admin, 1 of 4 open]") || !strings.Contains(out, "agent@box:~$") {
		t.Fatalf("open: %v %q", isErr, out)
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("the text view kept a control sequence: %q", out)
	}
	out, isErr = ls.call(ToolShellSend, map[string]any{"session": 1, "text": "sudo apk add jq", "key": "enter"})
	if isErr || !strings.Contains(out, "sudo apk add jq\n[sudo] password for agent:") {
		t.Fatalf("send: %v %q", isErr, out)
	}
	out, isErr = ls.call(ToolShellSend, map[string]any{"session": 1, "key": KeySudoPassword})
	if isErr || !strings.Contains(out, "ok, as root") {
		t.Fatalf("sudo-password: %v %q", isErr, out)
	}
	if _, isErr := ls.call(ToolShellClose, map[string]any{"session": 1}); isErr {
		t.Fatal("close")
	}
	transcript, err := os.ReadFile(filepath.Join(fx.logs, ShellLogFile("server", 1)))
	if err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(filepath.Join(fx.logs, LogFile("server")))
	for name, b := range map[string]string{"transcript": string(transcript), "log": string(log)} {
		if strings.Contains(b, testSudo) || strings.Contains(b, testPassword) {
			t.Errorf("the %s holds the sudo password: %s", name, b)
		}
	}
	for _, want := range []string{`sent "sudo apk add jq" enter`, "sent sudo-password", "[sudo] password for agent:", "ok, as root", "closed by the agent"} {
		if !strings.Contains(string(transcript), want) {
			t.Errorf("the transcript lacks %q:\n%s", want, transcript)
		}
	}
	if !strings.Contains(string(log), `"tool":"host_shell_send","command":"sudo apk add jq",`) || !strings.Contains(string(log), `"session":1,"keys":["enter"]`) ||
		!strings.Contains(string(log), `"keys":["sudo-password"]`) || strings.Count(string(log), "\n") != 4 {
		t.Errorf("the host's log:\n%s", log)
	}
	ls.mu.Lock()
	f := ls.terms[0]
	ls.mu.Unlock()
	opts := options(t, f.argv)
	if opts["requesttty"] != "force" || f.argv[len(f.argv)-2] != "--" || f.argv[len(f.argv)-1] != "172.20.0.4" {
		t.Errorf("the shell's ssh: %q", f.argv)
	}
	if !strings.Contains(strings.Join(f.env, " "), "TERM="+shellTerm) {
		t.Errorf("the shell's env: %q", f.env)
	}
}

func TestCtrlCStopsALongCommand(t *testing.T) {
	fx := newFixture(t, sudoTarget())
	ls := fx.serveLive(t)
	ls.call(ToolShellOpen, map[string]any{})
	out, _ := ls.call(ToolShellSend, map[string]any{"session": 1, "text": "sleep 600\n", "wait": 1})
	if !strings.Contains(out, "running") || strings.HasSuffix(out, "$ ") {
		t.Fatalf("sleep: %q", out)
	}
	out, isErr := ls.call(ToolShellSend, map[string]any{"session": 1, "keys": []string{"ctrl-c"}})
	if isErr || !strings.Contains(out, "^C\nagent@box:~$") {
		t.Fatalf("ctrl-c: %v %q", isErr, out)
	}
}

// TestALargeShellOutputIsPaged: a result holds one page; host_shell_read
// returns the rest, in order, nothing lost or repeated.
func TestALargeShellOutputIsPaged(t *testing.T) {
	fx := newFixture(t, sudoTarget())
	ls := fx.serveLive(t)
	ls.call(ToolShellOpen, map[string]any{})
	out, _ := ls.call(ToolShellSend, map[string]any{"session": 1, "text": "big 3000", "key": "enter"})
	all := out
	pages := 1
	for strings.Contains(out, "more output is kept") {
		if len(out) > 4<<10 {
			t.Fatalf("a page of %d bytes, over the 4 KiB result", len(out))
		}
		out, _ = ls.call(ToolShellRead, map[string]any{"session": 1})
		all += out
		pages++
	}
	if pages < 10 {
		t.Errorf("%d pages for 36 KB", pages)
	}
	var lines []string
	for _, l := range strings.Split(all, "\n") {
		if strings.HasPrefix(l, "line ") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 3000 || lines[0] != "line 000001" || lines[2999] != "line 003000" {
		t.Fatalf("%d lines, from %q", len(lines), lines[:min(len(lines), 3)])
	}
	for i, l := range lines {
		if l != fmt.Sprintf("line %06d", i+1) {
			t.Fatalf("line %d is %q", i+1, l)
		}
	}
}

func TestAnIdleSessionIsClosed(t *testing.T) {
	fx := newFixture(t, sudoTarget())
	fx.cfg.ShellIdle = 300 * time.Millisecond
	ls := fx.serveLive(t)
	ls.call(ToolShellOpen, map[string]any{"wait": 0})
	time.Sleep(700 * time.Millisecond)
	out, isErr := ls.call(ToolShellSend, map[string]any{"session": 1, "text": "ls", "key": "enter"})
	if !isErr || !strings.Contains(out, "is over") || !strings.Contains(out, "with no call") {
		t.Fatalf("a send to an idle session: %v %q", isErr, out)
	}
	out, isErr = ls.call(ToolShellRead, map[string]any{"session": 1})
	if !isErr || !strings.Contains(out, "session 1 is over") {
		t.Fatalf("a read after: %v %q", isErr, out)
	}
	transcript, _ := os.ReadFile(filepath.Join(fx.logs, ShellLogFile("server", 1)))
	if !strings.Contains(string(transcript), "ended after 300ms with no call") {
		t.Errorf("transcript:\n%s", transcript)
	}
}

func TestAtMostFourSessionsAHost(t *testing.T) {
	fx := newFixture(t, sudoTarget())
	ls := fx.serveLive(t)
	for i := 1; i <= 4; i++ {
		if out, isErr := ls.call(ToolShellOpen, map[string]any{"wait": 0}); isErr || !strings.Contains(out, fmt.Sprintf("[session %d:", i)) {
			t.Fatalf("open %d: %q", i, out)
		}
	}
	out, isErr := ls.call(ToolShellOpen, map[string]any{"wait": 0})
	if !isErr || !strings.Contains(out, "has 4 shell sessions open") {
		t.Fatalf("a fifth: %v %q", isErr, out)
	}
	// A shell that ended frees its place, as does a closed one.
	ls.call(ToolShellSend, map[string]any{"session": 2, "text": "exit", "key": "enter"})
	ls.call(ToolShellClose, map[string]any{"session": 3})
	for _, want := range []int{5, 6} {
		if out, isErr := ls.call(ToolShellOpen, map[string]any{"wait": 0}); isErr || !strings.Contains(out, fmt.Sprintf("[session %d:", want)) {
			t.Fatalf("open after two ended: %q", out)
		}
	}
	if out, isErr := ls.call(ToolShellOpen, map[string]any{"wait": 0}); !isErr {
		t.Fatalf("a fifth again: %q", out)
	}
}

func TestAShellThatEndsSaysSo(t *testing.T) {
	fx := newFixture(t, sudoTarget())
	ls := fx.serveLive(t)
	ls.call(ToolShellOpen, map[string]any{})
	out, _ := ls.call(ToolShellSend, map[string]any{"session": 1, "text": "exit", "key": "enter"})
	if !strings.Contains(out, "over: the shell ended, exit status 0") || !strings.Contains(out, "logout") {
		t.Fatalf("exit: %q", out)
	}
	if out, isErr := ls.call(ToolShellSend, map[string]any{"session": 1, "text": "ls"}); !isErr || !strings.Contains(out, "session 1 is over") {
		t.Fatalf("a send after: %q", out)
	}
	if out, isErr := ls.call(ToolShellSend, map[string]any{"session": 9, "text": "ls"}); !isErr || !strings.Contains(out, "no session 9") {
		t.Fatalf("an unknown session: %q", out)
	}
}

func TestTheShellsOutputIsMaskedAndViewable(t *testing.T) {
	fx := newFixture(t, sudoTarget())
	ls := fx.serveLive(t)
	ls.call(ToolShellOpen, map[string]any{})
	out, _ := ls.call(ToolShellSend, map[string]any{"session": 1, "text": "printpw", "key": "enter"})
	if strings.Contains(out, testPassword) || !strings.Contains(out, "the password is "+Redacted) {
		t.Errorf("the run's password reached the agent: %q", out)
	}
	out, _ = ls.call(ToolShellSend, map[string]any{"session": 1, "text": "color", "key": "enter"})
	if !strings.Contains(out, "red and plain\n100%\n") || strings.ContainsAny(out, "\x1b\x07\r") {
		t.Errorf("text view: %q", out)
	}
	out, _ = ls.call(ToolShellSend, map[string]any{"session": 1, "text": "color", "key": "enter", "raw": true})
	if !strings.Contains(out, "\x1b[1;31mred\x1b[0m") {
		t.Errorf("raw view: %q", out)
	}
	if out, isErr := ls.call(ToolShellSend, map[string]any{"session": 1, "key": "f13"}); !isErr || !strings.Contains(out, "no key \"f13\"") {
		t.Errorf("an unknown key: %q", out)
	}
	if out, isErr := ls.call(ToolShellSend, map[string]any{"session": 1, "text": "x", "wait": 601}); !isErr || !strings.Contains(out, "wait is 0 to 600") {
		t.Errorf("a long wait: %q", out)
	}
}

// TestASudoPasswordTypedAtThePromptIsMasked: typed where no prompt hides
// it, the terminal echoes the password and the shell reads it as a
// command; neither reaches the agent or the transcript.
func TestASudoPasswordTypedAtThePromptIsMasked(t *testing.T) {
	fx := newFixture(t, sudoTarget())
	ls := fx.serveLive(t)
	ls.call(ToolShellOpen, map[string]any{})
	out, _ := ls.call(ToolShellSend, map[string]any{"session": 1, "key": KeySudoPassword})
	if strings.Contains(out, testSudo) || !strings.Contains(out, Redacted) {
		t.Fatalf("the echoed sudo password: %q", out)
	}
	if out, isErr := ls.call(ToolShellSend, map[string]any{"session": 1, "text": "echo '" + testSudo + "'", "key": "enter"}); strings.Contains(out, testSudo) || isErr {
		t.Fatalf("typed as text: %q", out)
	}
	ls.call(ToolShellClose, map[string]any{"session": 1})
	b, _ := os.ReadFile(filepath.Join(fx.logs, ShellLogFile("server", 1)))
	l, _ := os.ReadFile(filepath.Join(fx.logs, LogFile("server")))
	if strings.Contains(string(b), testSudo) || strings.Contains(string(l), testSudo) || !strings.Contains(string(b), `sent "echo '`+Redacted+`'" enter`) {
		t.Errorf("transcript:\n%s\nlog:\n%s", b, l)
	}
}

// TestAShellErrorCarryingHostOutputIsAResource is D9: what the host
// printed reaches the agent past the gateway's secret block even when the
// answer is an error.
func TestAShellErrorCarryingHostOutputIsAResource(t *testing.T) {
	fx := newFixture(t, sudoTarget())
	fx.cfg.Shell = func(argv, env []string) (Terminal, error) {
		f := newFakeTerm(argv, env)
		f.print("Permission denied (publickey,password).\r\n")
		f.print(endOfOutput)
		return f, nil
	}
	res, _, err := fx.session(t, call(ToolShellOpen, map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	text, isErr, typ := answer(t, res["1"])
	if !isErr || typ != "resource" || !strings.Contains(text, "ended as it opened") || !strings.Contains(text, "Permission denied") {
		t.Fatalf("%v %s %q", isErr, typ, text)
	}
}

func TestSudoPasswordWithoutOneIsRefused(t *testing.T) {
	tg := sudoTarget()
	tg.Sudo = false
	fx := newFixture(t, tg)
	ls := fx.serveLive(t)
	ls.call(ToolShellOpen, map[string]any{})
	if out, isErr := ls.call(ToolShellSend, map[string]any{"session": 1, "key": KeySudoPassword}); !isErr || !strings.Contains(out, "no sudo password") {
		t.Fatalf("%v %q", isErr, out)
	}
}

// TestASignInSplitBetweenReadsIsMaskedWhole: output still arriving that
// ends with the head of a sign-in is kept back until its end comes, so no
// read returns a half that the scrub would not know.
func TestASignInSplitBetweenReadsIsMaskedWhole(t *testing.T) {
	fx := newFixture(t, sudoTarget())
	s, err := newServer(fx.cfg, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	sh := &shell{n: 1, h: s.hosts["server"], notified: make(chan struct{}, 1)}
	sh.unread = []byte("cat pw\r\nthe password is " + testPassword[:9])
	sh.lastOut = time.Now()
	first := s.take(sh, false)
	if first != "cat pw\nthe password is " || string(sh.unread) != testPassword[:9] {
		t.Fatalf("first take %q, kept %q", first, sh.unread)
	}
	sh.unread = append(sh.unread, testPassword[9:]+"\r\n\x1b[3"...)
	second := s.take(sh, false)
	if second != Redacted+"\n" || string(sh.unread) != "\x1b[3" {
		t.Fatalf("second take %q, kept %q", second, sh.unread)
	}
	sh.lastOut = time.Now().Add(-time.Hour) // quiet: nothing is kept back
	sh.unread = append(sh.unread, "1mx"...)
	if third := s.take(sh, false); third != "x" {
		t.Fatalf("third take %q", third)
	}
}

func TestTextView(t *testing.T) {
	for in, want := range map[string]string{
		"plain\r\nline\r\n":                     "plain\nline\n",
		"\x1b[32mgreen\x1b[0m":                  "green",
		"0%\r50%\r100%\r\n":                     "100%\n",
		"downloading\r\x1b[Kdone\r\n":           "done\n",
		"abc\b\bX\n":                            "aXc\n",
		"\x1b]0;user@host: ~\x07$ ":             "$ ",
		"\x1b[?2004h$ ls\r\n\x1b[?2004l":        "$ ls\n",
		"a\x1b(Bb\x1bPq#0\x1b\\c\x07d":          "abcd",
		"ünï\xffcode":                           "ünï�code",
		"col\x1b[10Gx\n":                        "col      x\n",
		"\x1b[1;1H\x1b[2Jtop - 10:00\x1b[K\r\n": "top - 10:00\n",
		"half \x1b[3":                           "half ",
		"tab\there\r\r\n":                       "tab\there\n",
	} {
		if got := textView([]byte(in)); got != want {
			t.Errorf("textView(%q) = %q, want %q", in, got, want)
		}
	}
	// A sequence a write splits is finished by the next.
	var v vt
	var out strings.Builder
	v.write([]byte("a\x1b[3"), &out)
	v.write([]byte("1mb\xc3"), &out)
	v.write([]byte("\xa9\n"), &out)
	if out.String() != "abé\n" {
		t.Errorf("split writes: %q", out.String())
	}
	for in, want := range map[string]int{"ok": 0, "a\x1b": 1, "a\x1b[1;3": 5, "\x1b]0;t": 5, "x\xc3": 1, "x\xe2\x82": 2, "\x1b[0mz": 0} {
		if got := unfinishedTail([]byte(in)); got != want {
			t.Errorf("unfinishedTail(%q) = %d, want %d", in, got, want)
		}
	}
}
