package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/desktoplink"
)

// A job's desktop link, on the runhost. A job whose release uses
// applications on the person's computer carries the binding the device made
// with its Agentry Desktop. The service serves the job an owner-only socket
// in the job's own directory, which the job's run reaches as it would reach
// Agentry Desktop's link socket, and carries each message there (a tool
// call, or the run's unbind) to the device as a link frame on the link
// session that device opened for the job; the device's answer comes back on
// that session's input. The runhost opens nothing towards the device: with
// no link session open, a call is refused desktop-unreachable, and the run
// goes on.

const (
	// runhostLinkSocket is the job's link socket in its directory.
	runhostLinkSocket = "link.sock"
	// runhostLinkFile is the relayed binding the job's run reads.
	runhostLinkFile = "desktop.json"
)

// runhostLinkFirstWait bounds how long a call waits for the device's first
// link session, which the device opens once the runhost says it runs the
// job. After a session has dropped, a call is refused at once.
var runhostLinkFirstWait = 2 * time.Minute

// runhostLink is one job's desktop link.
type runhostLink struct {
	job, device string
	ln          net.Listener

	mu      sync.Mutex
	current *runhostLinkSession
	ever    bool
	// linked is closed when the first session opens.
	linked chan struct{}
	done   chan struct{}
	next   uint64
	once   sync.Once
	wg     sync.WaitGroup
}

// runhostLinkSession is one link session the device holds open.
type runhostLinkSession struct {
	fw      *runhost.FrameWriter
	mu      sync.Mutex
	pending map[string]chan desktop.Message
	gone    chan struct{}
}

// openRunhostLink serves job j's link socket in dir until the job ends.
func openRunhostLink(ctx context.Context, j *job.Job, dir string) (*runhostLink, string, error) {
	sock := filepath.Join(dir, runhostLinkSocket)
	ln, err := runhost.ListenOwnerOnly(sock)
	if err != nil {
		return nil, "", fmt.Errorf("the job's desktop link socket: %w", err)
	}
	l := &runhostLink{job: j.ID, device: j.Device, ln: ln, linked: make(chan struct{}), done: make(chan struct{})}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.serve(ctx)
	}()
	return l, sock, nil
}

// close ends the link with the job: the socket closes, every call in flight
// is refused, and the device's session is told the job is over.
func (l *runhostLink) close() {
	l.once.Do(func() {
		close(l.done)
		_ = l.ln.Close()
		_ = os.Remove(l.ln.Addr().String())
	})
	l.wg.Wait()
}

func (l *runhostLink) serve(ctx context.Context) {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.answer(ctx, c)
		}()
	}
}

// answer takes one message on the job's socket and answers it: a tool call
// or an unbind of the bound run crosses to the device, anything else is
// refused here and never leaves the runhost.
func (l *runhostLink) answer(ctx context.Context, c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(time.Minute))
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64<<10), desktop.MaxLine)
	if !sc.Scan() {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	var ans desktop.Message
	m, err := desktop.ReadMessage(bytes.TrimSpace(sc.Bytes()))
	switch {
	case err != nil, m.Bind != nil, m.Bound != nil, m.Answer != nil, m.Refused != nil, m.OK,
		m.Call == nil && m.Unbind == nil, m.Call != nil && m.Unbind != nil:
		ans.Refused = &desktop.Refusal{Reason: "not-a-message", Message: "a job's desktop link carries a tool call or the run's unbind, and this was neither"}
	default:
		ans = l.forward(ctx, m)
	}
	b, err := json.Marshal(ans)
	if err != nil {
		return
	}
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, _ = c.Write(append(b, '\n'))
}

// unreachable is the refusal of a call no link session can carry.
func unreachable() desktop.Message {
	return desktop.Message{Refused: &desktop.Refusal{Reason: receipt.DesktopUnreachable,
		Message: "the computer that started this run is not linked to the runhost now: its Agentry Desktop answers the run only while that computer follows the run (agentry runhost attach follows it again)"}}
}

// session is the link session open now: it waits for the device's first
// session while none has opened yet, and is nil once the job has ended or
// the device has left.
func (l *runhostLink) session(ctx context.Context) *runhostLinkSession {
	l.mu.Lock()
	s, ever := l.current, l.ever
	l.mu.Unlock()
	if s != nil || ever {
		return s
	}
	select {
	case <-l.linked:
	case <-l.done:
		return nil
	case <-ctx.Done():
		return nil
	case <-time.After(runhostLinkFirstWait):
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.current
}

// forward carries m to the device and waits for its answer.
func (l *runhostLink) forward(ctx context.Context, m desktop.Message) desktop.Message {
	s := l.session(ctx)
	if s == nil {
		return unreachable()
	}
	b, err := json.Marshal(m)
	if err != nil {
		return unreachable()
	}
	l.mu.Lock()
	l.next++
	id := strconv.FormatUint(l.next, 10)
	l.mu.Unlock()
	ch := make(chan desktop.Message, 1)
	s.mu.Lock()
	if s.pending == nil {
		s.mu.Unlock()
		return unreachable()
	}
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.pending != nil {
			delete(s.pending, id)
		}
		s.mu.Unlock()
	}()
	if err := s.fw.Frame(runhost.Frame{Type: runhost.FrameLink, Text: id, Result: b}); err != nil {
		return unreachable()
	}
	wait := desktoplink.CallTimeout + time.Minute
	select {
	case a := <-ch:
		return a
	case <-s.gone:
	case <-l.done:
	case <-ctx.Done():
	case <-time.After(wait):
	}
	return unreachable()
}

// attach holds one link session from the device: it replaces any earlier
// one (whose calls in flight are refused), says the link is open with a
// started frame, takes the device's answers from in, and returns when the
// job ends, the device leaves, or the service stops.
func (l *runhostLink) attach(ctx context.Context, fw *runhost.FrameWriter, in io.Reader) (int, error) {
	s := &runhostLinkSession{fw: fw, pending: map[string]chan desktop.Message{}, gone: make(chan struct{})}
	l.mu.Lock()
	select {
	case <-l.done:
		l.mu.Unlock()
		return runhost.LinkExitNoJob, fmt.Errorf("job %s has ended", l.job)
	default:
	}
	old := l.current
	l.current = s
	if !l.ever {
		l.ever = true
		close(l.linked)
	}
	l.mu.Unlock()
	if old != nil {
		old.end()
	}
	defer func() {
		l.mu.Lock()
		if l.current == s {
			l.current = nil
		}
		l.mu.Unlock()
		s.end()
	}()
	if err := fw.Started(l.job); err != nil {
		return 0, nil
	}
	read := make(chan struct{})
	go func() {
		defer close(read)
		br := bufio.NewReaderSize(in, 64<<10)
		dec := json.NewDecoder(br)
		for {
			var a runhost.LinkAnswer
			if err := dec.Decode(&a); err != nil {
				return
			}
			m, err := desktop.ReadMessage(a.Message)
			if err != nil {
				continue
			}
			// Only an answer, a refusal or an unbind's ok goes back to the
			// run.
			m = desktop.Message{Answer: m.Answer, Refused: m.Refused, OK: m.OK}
			s.deliver(a.ID, m)
		}
	}()
	select {
	case <-read:
	case <-l.done:
	case <-s.gone:
	case <-ctx.Done():
	}
	return 0, nil
}

// end closes the session: its calls in flight are refused.
func (s *runhostLinkSession) end() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		return
	}
	s.pending = nil
	close(s.gone)
}

func (s *runhostLinkSession) deliver(id string, m desktop.Message) {
	s.mu.Lock()
	ch := s.pending[id]
	s.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- m:
	default:
	}
}

// runhostLinkVerb answers `link <job>`: the device that started the job,
// and no other, holds the job's desktop link open on this session.
func runhostLinkVerb(ctx context.Context, req runhost.Request, in io.Reader, fw *runhost.FrameWriter) (int, error) {
	if len(req.Args) != 1 || !job.ValidID(req.Args[0]) {
		return engine.ExitUsage, errors.New("link takes a job's id")
	}
	if req.Device == "" {
		return engine.ExitUsage, errors.New("a job's desktop link is opened by the device that started the job, over its own run key, and this key names no device")
	}
	l := liveJob(req.Args[0])
	if l == nil {
		return runhost.LinkExitNoJob, fmt.Errorf("no job %s runs here", req.Args[0])
	}
	if l.device != req.Device {
		return engine.ExitUsage, fmt.Errorf("job %s was started by another device: only that device's Agentry Desktop answers it", req.Args[0])
	}
	l.mu.Lock()
	lk := l.link
	l.mu.Unlock()
	if lk == nil {
		return runhost.LinkExitNoJob, fmt.Errorf("job %s uses no application on the person's computer, or has not started yet", req.Args[0])
	}
	return lk.attach(ctx, fw, in)
}

// runhostRelayedDesktop is the relayed binding job j's run reads: the
// device's run and binding, the job's link socket, and how the calls travel,
// for the receipt.
func runhostRelayedDesktop(j *job.Job, sock string) desktop.Relayed {
	return desktop.Relayed{Socket: sock, Run: j.Run.Desktop.Run, Bound: j.Run.Desktop.Bound,
		Via: "Agentry Desktop on the device that started the job (device " + j.Device + "), over the link session that device opened to this runhost with its run key and keeps open while the run lasts, through the runhost service's owner-only socket " + sock + " (the runhost opens no connection towards the device; with no link session open, a call is refused desktop-unreachable)"}
}
