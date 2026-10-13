package hostlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The interactive shell (plan runtime-hosts.md §5, D4, D18; agentry-cli#862):
// host_shell_open signs in to a target host and starts the account's login
// shell on a pseudo-terminal the host allocates (ssh -tt), host_shell_send
// types into it, host_shell_read reads what it printed, host_shell_close
// ends it. A session is one ssh process this server keeps between calls,
// which it can because the gateway keeps the host link up once it has
// started it for a call. What the shell prints reaches the agent as it is
// (D9), as text with the terminal's control sequences removed or, asked,
// raw, but for the run's own sign-ins (D19); the sudo password is typed by
// the host link and never seen. Every session's transcript is kept,
// ShellLogFile, and every call is a line of the host's log.

// The bounds of §5. The exported ones are stated in the Drafter's runtime
// facts (agentry-cli#864), generated from these.
const (
	MaxShellsPerHost  = 4
	DefaultShellIdle  = 30 * time.Minute
	defaultShellWait  = 5
	maxShellWait      = 600
	defaultSettle     = 1500 * time.Millisecond
	maxTypedBytes     = 64 << 10
	maxUnreadBytes    = 8 << 20
	maxTranscriptSize = 64 << 20
)

// keyBytes are the keys host_shell_send types by name, as an xterm sends
// them. sudo-password is not here: it is the host's sudo password and a
// return, typed by the host link.
var keyBytes = func() map[string]string {
	m := map[string]string{
		"enter": "\r", "tab": "\t", "backspace": "\x7f", "esc": "\x1b", "space": " ",
		"up": "\x1b[A", "down": "\x1b[B", "right": "\x1b[C", "left": "\x1b[D",
		"home": "\x1b[H", "end": "\x1b[F", "page-up": "\x1b[5~", "page-down": "\x1b[6~", "delete": "\x1b[3~",
	}
	for c := 'a'; c <= 'z'; c++ {
		m["ctrl-"+string(c)] = string(rune(c - 'a' + 1))
	}
	return m
}()

// KeySudoPassword is the key that types the run's sudo password.
const KeySudoPassword = "sudo-password"

// shell is one session.
type shell struct {
	n    int
	h    *host
	term Terminal

	mu       sync.Mutex
	unread   []byte
	dropped  int64
	pending  string // the rest of a result that did not fit, as it was given
	lastOut  time.Time
	ended    bool
	endWhy   string
	closed   string // why the host link ended it, if it did
	tlog     *transcript
	idle     *time.Timer
	notified chan struct{}
}

// signal wakes a call waiting on the session.
func (sh *shell) signal() {
	select {
	case sh.notified <- struct{}{}:
	default:
	}
}

// pump reads what the session prints until it ends.
func (sh *shell) pump() {
	buf := make([]byte, 32<<10)
	for {
		n, err := sh.term.Read(buf)
		if n > 0 {
			sh.mu.Lock()
			sh.unread = append(sh.unread, buf[:n]...)
			if over := len(sh.unread) - maxUnreadBytes; over > 0 {
				// Cut at a line end, so that what is kept never begins
				// with the end of a sign-in whose head was dropped.
				if nl := bytes.IndexByte(sh.unread[over:], '\n'); nl >= 0 && nl < 64<<10 {
					over += nl + 1
				}
				sh.unread = append(sh.unread[:0], sh.unread[over:]...)
				sh.dropped += int64(over)
			}
			sh.lastOut = time.Now()
			sh.tlog.output(buf[:n])
			sh.mu.Unlock()
			sh.signal()
		}
		if err != nil {
			break
		}
	}
	code, err := sh.term.Wait()
	sh.mu.Lock()
	sh.ended = true
	if sh.idle != nil {
		sh.idle.Stop()
	}
	switch {
	case sh.closed != "":
		sh.endWhy = sh.closed
	case err != nil:
		sh.endWhy = "the shell ended: " + err.Error()
	default:
		sh.endWhy = fmt.Sprintf("the shell ended, exit status %d", code)
	}
	sh.tlog.mark(sh.endWhy)
	sh.tlog.close()
	sh.mu.Unlock()
	sh.signal()
}

// await waits until the session prints something after since and then
// nothing for settle, or it ends, or wait passes.
func (sh *shell) await(ctx context.Context, since time.Time, wait, settle time.Duration) {
	deadline := time.Now().Add(wait)
	for {
		sh.mu.Lock()
		ended, last := sh.ended, sh.lastOut
		sh.mu.Unlock()
		now := time.Now()
		if ended || !now.Before(deadline) {
			return
		}
		next := deadline
		if last.After(since) {
			quiet := last.Add(settle)
			if !now.Before(quiet) {
				return
			}
			next = minTime(next, quiet)
		}
		t := time.NewTimer(next.Sub(now))
		select {
		case <-sh.notified:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return
		}
		t.Stop()
	}
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// --- the server's side ---

type shells struct {
	pumps  sync.WaitGroup // every session's pump, so the run's end waits on the transcripts
	mu     sync.Mutex
	open   map[int]*shell
	gone   map[int]string
	seq    int
	idle   time.Duration
	settle time.Duration
	start  func(argv, env []string) (Terminal, error)
}

func newShells(cfg Config) *shells {
	sh := &shells{open: map[int]*shell{}, gone: map[int]string{}, idle: cfg.ShellIdle, settle: cfg.ShellSettle, start: cfg.Shell}
	if sh.idle <= 0 {
		sh.idle = DefaultShellIdle
	}
	if sh.settle <= 0 {
		sh.settle = defaultSettle
	}
	if sh.start == nil {
		sh.start = startTerminal
	}
	return sh
}

// count is how many of h's sessions are running.
func (ss *shells) count(h *host) int {
	n := 0
	for _, sh := range ss.open {
		sh.mu.Lock()
		if sh.h == h && !sh.ended {
			n++
		}
		sh.mu.Unlock()
	}
	return n
}

// get is session n, or why there is none.
func (ss *shells) get(n int) (*shell, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if sh, ok := ss.open[n]; ok {
		return sh, nil
	}
	if why, ok := ss.gone[n]; ok {
		return nil, fmt.Errorf("session %d is over: %s", n, why)
	}
	if n <= 0 {
		return nil, errors.New("name the session: its number, as host_shell_open gave it")
	}
	return nil, fmt.Errorf("there is no session %d", n)
}

// end stops a session for why: its pump records why when the process is
// gone.
func (ss *shells) end(sh *shell, why string) {
	sh.mu.Lock()
	if sh.closed == "" && !sh.ended {
		sh.closed = why
	}
	if sh.idle != nil {
		sh.idle.Stop()
	}
	sh.mu.Unlock()
	sh.term.Kill()
}

// forget drops a session that has ended and whose output has all been
// read.
func (ss *shells) forget(sh *shell) {
	sh.mu.Lock()
	done := sh.ended && len(sh.unread) == 0 && sh.pending == ""
	why := sh.endWhy
	sh.mu.Unlock()
	if !done {
		return
	}
	ss.mu.Lock()
	delete(ss.open, sh.n)
	ss.gone[sh.n] = why
	ss.mu.Unlock()
}

// closeAll ends every session, the run being over, and waits a moment for
// each transcript's last lines.
func (ss *shells) closeAll() {
	ss.mu.Lock()
	list := make([]*shell, 0, len(ss.open))
	for _, sh := range ss.open {
		list = append(list, sh)
	}
	ss.mu.Unlock()
	for _, sh := range list {
		ss.end(sh, "the run ended")
	}
	done := make(chan struct{})
	go func() {
		ss.pumps.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// touch restarts a session's idle clock: a session no call has used for
// the idle time is ended.
func (s *server) touch(sh *shell) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.ended || sh.closed != "" {
		return
	}
	if sh.idle != nil {
		sh.idle.Reset(s.shells.idle)
		return
	}
	sh.idle = time.AfterFunc(s.shells.idle, func() {
		s.shells.end(sh, fmt.Sprintf("ended after %s with no call", words(s.shells.idle)))
	})
}

func words(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d minutes", d/time.Minute)
	}
	return d.String()
}

// --- the tools ---

func (s *server) toolShellOpen(ctx context.Context, args json.RawMessage) (string, bool) {
	var a struct {
		Host string `json:"host"`
		Wait *int   `json:"wait"`
		Raw  bool   `json:"raw"`
	}
	if err := decode(args, &a); err != nil {
		return err.Error(), true
	}
	h, err := s.pick(a.Host)
	if err != nil {
		return err.Error(), true
	}
	wait, err := waitArg(a.Wait, defaultShellWait)
	if err != nil {
		return err.Error(), true
	}
	ss := s.shells
	ss.mu.Lock()
	if ss.count(h) >= MaxShellsPerHost {
		ss.mu.Unlock()
		return fmt.Sprintf("%s has %d shell sessions open, the most a target host may have: close one with %s first", h.t.ID, MaxShellsPerHost, ToolShellClose), true
	}
	ss.seq++
	n := ss.seq
	ss.mu.Unlock()
	entry := LogEntry{Tool: ToolShellOpen, Session: n}
	term, err := ss.start(shellArgv(h.t, h.dir, h.relay), shellEnv(h.t, h.dir))
	if err != nil {
		entry.Error = err.Error()
		s.writeLog(h, entry)
		ss.mu.Lock()
		ss.gone[n] = "it could not be started"
		ss.mu.Unlock()
		return "the shell could not be started: " + err.Error(), true
	}
	sh := &shell{n: n, h: h, term: term, notified: make(chan struct{}, 1), tlog: s.openTranscript(h, n)}
	since := time.Now()
	ss.mu.Lock()
	ss.open[n] = sh
	ss.mu.Unlock()
	ss.pumps.Add(1)
	go func() {
		defer ss.pumps.Done()
		sh.pump()
	}()
	s.touch(sh)
	sh.await(ctx, since, wait, ss.settle)
	out := s.take(sh, a.Raw)
	sh.mu.Lock()
	ended, why := sh.ended, sh.endWhy
	sh.mu.Unlock()
	entry.Bytes = int64(len(out))
	if ended {
		// It ended before it was used: the host refused the sign-in, or the
		// account has no shell.
		entry.Error = why
		s.writeLog(h, entry)
		ss.forget(sh)
		// ssh names the relay it dialled; the person knows the host.
		out = strings.ReplaceAll(out, h.relay, h.t.Named)
		return fmt.Sprintf("the shell on %s ended as it opened (%s); what it printed:\n%s", h.t.ID, why, out), true
	}
	s.writeLog(h, entry)
	ss.mu.Lock()
	running := ss.count(h)
	ss.mu.Unlock()
	return fmt.Sprintf("[session %d: a login shell on %s as %s, %d of %d open]\n%s", n, h.t.ID, h.t.User, running, MaxShellsPerHost, out), false
}

func (s *server) toolShellSend(ctx context.Context, args json.RawMessage) (string, bool) {
	var a struct {
		Session int      `json:"session"`
		Text    string   `json:"text"`
		Key     string   `json:"key"`
		Keys    []string `json:"keys"`
		Wait    *int     `json:"wait"`
		Raw     bool     `json:"raw"`
	}
	if err := decode(args, &a); err != nil {
		return err.Error(), true
	}
	sh, err := s.shells.get(a.Session)
	if err != nil {
		return err.Error(), true
	}
	wait, err := waitArg(a.Wait, defaultShellWait)
	if err != nil {
		return err.Error(), true
	}
	keys := a.Keys
	if a.Key != "" {
		keys = append([]string{a.Key}, keys...)
	}
	switch {
	case a.Text == "" && len(keys) == 0:
		return fmt.Sprintf("nothing to send: give text, keys or both (%s reads what the session printed)", ToolShellRead), true
	case len(a.Text) > maxTypedBytes:
		return fmt.Sprintf("the text is %d bytes; at most %d a call (write a file with %s instead)", len(a.Text), maxTypedBytes, ToolWrite), true
	case strings.ContainsRune(a.Text, 0):
		return "the text holds a NUL byte", true
	}
	typed := []byte(a.Text)
	for _, k := range keys {
		if k == KeySudoPassword {
			if !sh.h.t.Sudo {
				return fmt.Sprintf("this run was given no sudo password for %s, so %s has nothing to type", sh.h.t.ID, KeySudoPassword), true
			}
			typed = append(typed, strings.TrimRight(sh.h.sudo, "\r\n")+"\r"...)
			continue
		}
		b, ok := keyBytes[k]
		if !ok {
			return fmt.Sprintf("no key %q: the keys are %s", k, keyNames()), true
		}
		typed = append(typed, b...)
	}
	sh.mu.Lock()
	ended, why := sh.ended, sh.endWhy
	sh.mu.Unlock()
	entry := LogEntry{Tool: ToolShellSend, Session: sh.n, Command: a.Text, Keys: keys}
	if ended {
		out := s.take(sh, a.Raw)
		s.shells.forget(sh)
		entry.Error = "the session had ended"
		s.writeLog(sh.h, entry)
		return fmt.Sprintf("session %d is over (%s), so nothing was sent; what it printed last:\n%s", sh.n, why, out), true
	}
	s.touch(sh)
	sh.mu.Lock()
	sh.tlog.sent(a.Text, keys)
	sh.mu.Unlock()
	since := time.Now()
	if _, err := sh.term.Write(typed); err != nil {
		entry.Error = err.Error()
		s.writeLog(sh.h, entry)
		return fmt.Sprintf("could not type into session %d: %v", sh.n, err), true
	}
	sh.await(ctx, since, wait, s.shells.settle)
	out := s.take(sh, a.Raw)
	entry.Bytes = int64(len(out))
	s.writeLog(sh.h, entry)
	return s.shellResult(sh, out), false
}

func (s *server) toolShellRead(ctx context.Context, args json.RawMessage) (string, bool) {
	var a struct {
		Session int  `json:"session"`
		Wait    *int `json:"wait"`
		Raw     bool `json:"raw"`
	}
	if err := decode(args, &a); err != nil {
		return err.Error(), true
	}
	sh, err := s.shells.get(a.Session)
	if err != nil {
		return err.Error(), true
	}
	wait, err := waitArg(a.Wait, 0)
	if err != nil {
		return err.Error(), true
	}
	s.touch(sh)
	sh.mu.Lock()
	nothing := len(sh.unread) == 0 && sh.pending == "" && !sh.ended
	sh.mu.Unlock()
	if nothing && wait > 0 {
		sh.await(ctx, time.Now(), wait, s.shells.settle)
	}
	out := s.take(sh, a.Raw)
	s.writeLog(sh.h, LogEntry{Tool: ToolShellRead, Session: sh.n, Bytes: int64(len(out))})
	return s.shellResult(sh, out), false
}

func (s *server) toolShellClose(_ context.Context, args json.RawMessage) (string, bool) {
	var a struct {
		Session int  `json:"session"`
		Raw     bool `json:"raw"`
	}
	if err := decode(args, &a); err != nil {
		return err.Error(), true
	}
	sh, err := s.shells.get(a.Session)
	if err != nil {
		return err.Error(), true
	}
	s.shells.end(sh, "closed by the agent")
	// The process is killed; its last output is in by the time pump sees
	// the pipe close.
	deadline := time.Now().Add(5 * time.Second)
	for {
		sh.mu.Lock()
		ended := sh.ended
		sh.mu.Unlock()
		if ended || time.Now().After(deadline) {
			break
		}
		select {
		case <-sh.notified:
		case <-time.After(100 * time.Millisecond):
		}
	}
	out := s.take(sh, a.Raw)
	sh.mu.Lock()
	rest := len(sh.unread) + len(sh.pending)
	sh.unread, sh.pending = nil, ""
	sh.mu.Unlock()
	s.shells.forget(sh)
	s.writeLog(sh.h, LogEntry{Tool: ToolShellClose, Session: sh.n, Bytes: int64(len(out))})
	msg := fmt.Sprintf("[session %d on %s is closed]", sh.n, sh.h.t.ID)
	if out != "" {
		msg += " What it printed that was not read:\n" + out
	}
	if rest > 0 {
		msg += fmt.Sprintf("\n[%d more bytes it printed were not returned; the session's transcript in the run's receipts holds its text]", rest)
	}
	return msg, false
}

// shellResult is a send's or a read's answer: the session's state, then
// what it printed.
func (s *server) shellResult(sh *shell, out string) string {
	sh.mu.Lock()
	ended, why, more := sh.ended, sh.endWhy, len(sh.unread) > 0 || sh.pending != ""
	sh.mu.Unlock()
	state := "running"
	if ended {
		state = "over: " + why
	}
	head := fmt.Sprintf("[session %d on %s, %s]\n", sh.n, sh.h.t.ID, state)
	if more {
		out += fmt.Sprintf("\n[more output is kept: %s with {\"session\": %d} reads on]", ToolShellRead, sh.n)
	}
	if ended && !more {
		s.shells.forget(sh)
	}
	return head + out
}

// take is what the session printed since the last take, as text or raw,
// masked of the run's sign-ins, and at most one result's worth: the rest
// is kept for the next. While the session is still printing, an end of the
// output that could begin a sign-in, or a sequence or character not yet
// finished, waits for the bytes that finish it.
func (s *server) take(sh *shell, raw bool) string {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	var text string
	if sh.pending != "" {
		text, sh.pending = sh.pending, ""
	} else {
		data := sh.unread
		quiet := sh.ended || time.Since(sh.lastOut) >= s.shells.settle
		hold := 0
		if !quiet {
			hold = max(s.scrub.heldTail(data), unfinishedTail(data))
		}
		ready := data[:len(data)-hold]
		sh.unread = append([]byte(nil), data[len(data)-hold:]...)
		text = s.scrub.String(string(ready))
		if raw {
			text = strings.ToValidUTF8(text, "�")
		} else {
			text = s.scrub.String(textView([]byte(text)))
		}
		if sh.dropped > 0 {
			text = fmt.Sprintf("[%d bytes it printed before this were not kept: more than %d MiB came between two reads]\n", sh.dropped, maxUnreadBytes>>20) + text
			sh.dropped = 0
		}
	}
	head := string(s.limits.Head([]byte(text)))
	sh.pending = text[len(head):]
	return head
}

// waitArg reads a wait: def when it is not given, else 0 to maxShellWait
// seconds.
func waitArg(w *int, def int) (time.Duration, error) {
	if w == nil {
		return time.Duration(def) * time.Second, nil
	}
	if *w < 0 || *w > maxShellWait {
		return 0, fmt.Errorf("wait is 0 to %d seconds", maxShellWait)
	}
	return time.Duration(*w) * time.Second, nil
}

func keyNames() string {
	names := make([]string, 0, len(keyBytes)+1)
	for k := range keyBytes {
		if !strings.HasPrefix(k, "ctrl-") {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return strings.Join(append(names, "ctrl-a … ctrl-z", KeySudoPassword), ", ")
}

// --- the transcript ---

// transcript is a session's record as the run's receipts keep it: what
// was typed, by its text and the keys' names, and what the shell printed,
// as text, masked of the run's sign-ins as the agent's view is, with the
// same hold for a sign-in a write splits.
type transcript struct {
	f       *os.File
	scrub   *scrubber
	now     func() time.Time
	v       vt
	hold    []byte
	size    int64
	cut     bool
	midLine bool
}

func (s *server) openTranscript(h *host, n int) *transcript {
	if s.cfg.LogDir == "" {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(s.cfg.LogDir, ShellLogFile(h.t.ID, n)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		s.logf("session %d's transcript: %v", n, err)
		return nil
	}
	t := &transcript{f: f, scrub: s.scrub, now: s.cfg.Now}
	t.mark(fmt.Sprintf("session %d: a login shell on %s (%s, %s:%d) as %s", n, h.t.ID, h.t.Named, h.t.Address, h.t.Port, h.t.User))
	return t
}

func (t *transcript) write(s string) {
	if t == nil || t.cut || s == "" {
		return
	}
	if t.size+int64(len(s)) > maxTranscriptSize {
		s = fmt.Sprintf("\n── the transcript stops here: it reached %d MiB\n", maxTranscriptSize>>20)
		t.cut = true
	}
	n, _ := t.f.WriteString(s)
	t.size += int64(n)
	t.midLine = !strings.HasSuffix(s, "\n")
}

// output records what the shell printed.
func (t *transcript) output(p []byte) {
	if t == nil {
		return
	}
	b := append(t.hold, p...)
	k := t.scrub.heldTail(b)
	t.hold = append([]byte(nil), b[len(b)-k:]...)
	var out strings.Builder
	t.v.write([]byte(t.scrub.String(string(b[:len(b)-k]))), &out)
	t.write(t.scrub.String(out.String()))
}

// mark records an event on a line of its own.
func (t *transcript) mark(what string) {
	if t == nil {
		return
	}
	var out strings.Builder
	if len(t.hold) > 0 {
		t.v.write([]byte(t.scrub.String(string(t.hold))), &out)
		t.hold = nil
	}
	t.v.flush(&out)
	t.write(t.scrub.String(out.String()))
	prefix := ""
	if t.midLine {
		prefix = "\n"
	}
	t.write(fmt.Sprintf("%s── %s %s\n", prefix, t.now().UTC().Format(time.RFC3339), what))
}

// sent records what was typed: the text quoted, and each key by its name.
func (t *transcript) sent(text string, keys []string) {
	if t == nil {
		return
	}
	var parts []string
	if text != "" {
		// Masked before it is quoted: quoting would respell a sign-in.
		parts = append(parts, fmt.Sprintf("%q", t.scrub.String(text)))
	}
	parts = append(parts, keys...)
	t.mark("sent " + strings.Join(parts, " "))
}

func (t *transcript) close() {
	if t == nil {
		return
	}
	_ = t.f.Close()
}
