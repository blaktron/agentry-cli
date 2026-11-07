package desktoplink

// The relay carries the desktop link's calls out of the sandbox. It runs in
// a container of the run's own with no network and a read-only root, holds
// the run's relay volume, writes the run's binding there for the desktop
// link entries, serves the link's socket in it, and carries each call over
// its own stdio, which the CLI holds through the engine's attach, to Agentry
// Desktop. No socket of the person's computer is bound into any container,
// so the transport is the same on every engine and under every runtime.
// Only a tool call crosses: a connection's one line must read as a Request
// naming a bound server and a tool and carrying the run token, and anything
// else is refused at the socket without reaching the CLI.

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
	"strings"
	"sync"
	"time"
)

// refusedLine is the answer to a connection whose line is not a tool call
// of a bound server carrying the run token.
const refusedLine = "the desktop link takes one tool call a connection, for a server this run bound and with this run's token, and this was not one"

// RelayConfig is what the relay serves.
type RelayConfig struct {
	// Listen is the socket's path; the binding is written beside it.
	Listen string
	// Owner is the uid:gid the socket and the binding are given, the
	// desktop link's user; empty leaves them the relay's.
	Owner string
	// In carries the binding and then the answers from the CLI, Out the
	// ready line and the calls to it.
	In  io.Reader
	Out io.Writer
	// Log takes what goes wrong.
	Log io.Writer
}

// Relay reads the binding from In, writes it beside the socket, serves the
// socket until In ends or ctx is done.
func Relay(ctx context.Context, cfg RelayConfig) error {
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	in := bufio.NewReader(cfg.In)
	bind, err := readBind(in)
	if err != nil {
		return fmt.Errorf("desktoplink relay: the binding: %w", err)
	}
	var uid, gid int
	chown := cfg.Owner != ""
	if chown {
		if uid, gid, err = parseOwner(cfg.Owner); err != nil {
			return err
		}
	}
	bindPath := filepath.Join(filepath.Dir(cfg.Listen), filepath.Base(BindFile))
	raw, err := json.Marshal(bind)
	if err != nil {
		return err
	}
	if err := os.WriteFile(bindPath, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("desktoplink relay: write the binding: %w", err)
	}
	if chown {
		if err := os.Chown(bindPath, uid, gid); err != nil {
			return fmt.Errorf("desktoplink relay: give the binding to %s: %w", cfg.Owner, err)
		}
	}
	_ = os.Remove(cfg.Listen)
	ln, err := net.Listen("unix", cfg.Listen)
	if err != nil {
		return fmt.Errorf("desktoplink relay: %w", err)
	}
	defer func() { _ = ln.Close() }()
	if err := os.Chmod(cfg.Listen, 0o600); err != nil {
		return fmt.Errorf("desktoplink relay: %w", err)
	}
	if chown {
		if err := os.Chown(cfg.Listen, uid, gid); err != nil {
			return fmt.Errorf("desktoplink relay: give the socket to %s: %w", cfg.Owner, err)
		}
	}
	r := &relay{token: bind.Token, bound: bind.Servers, out: cfg.Out, log: cfg.Log, pending: map[uint64]chan Answer{}}
	if err := r.send(Frame{Ready: true}); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	inDone := make(chan struct{})
	go func() {
		if err := r.readAnswers(in); err != nil {
			_, _ = fmt.Fprintf(r.log, "desktoplink relay: reading the CLI's answers: %v\n", err)
		}
		close(inDone)
	}()
	go func() {
		select {
		case <-ctx.Done():
		case <-inDone:
			cancel()
		}
		_ = ln.Close()
	}()
	slots := make(chan struct{}, maxRelayCalls)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				r.failAll()
				return nil
			}
			return fmt.Errorf("desktoplink relay: %w", err)
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			_ = c.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			r.serve(ctx, c)
		}()
	}
}

// readBind reads the CLI's first line, which must be a bind frame with a
// token and at least one server.
func readBind(in *bufio.Reader) (Bind, error) {
	line, err := readLine(in)
	if err != nil {
		return Bind{}, err
	}
	f, err := ReadFrame(line)
	if err != nil {
		return Bind{}, err
	}
	if f.Bind == nil || f.ID != 0 || f.Ready || f.Request != nil || f.Answer != nil || f.Server != "" || f.Token != "" {
		return Bind{}, errors.New("the first line was not the binding")
	}
	if f.Bind.Token == "" || len(f.Bind.Servers) == 0 {
		return Bind{}, errors.New("the binding names no token or no server")
	}
	return *f.Bind, nil
}

// readLine reads one line of at most MaxLine bytes.
func readLine(in *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		part, err := in.ReadSlice('\n')
		buf = append(buf, part...)
		if len(buf) > MaxLine {
			return nil, errors.New("a line over the limit")
		}
		if err == nil {
			return bytes.TrimSpace(buf), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			if len(bytes.TrimSpace(buf)) > 0 && errors.Is(err, io.EOF) {
				return bytes.TrimSpace(buf), nil
			}
			return nil, err
		}
	}
}

func parseOwner(s string) (int, int, error) {
	u, g, ok := strings.Cut(s, ":")
	uid, err1 := strconv.Atoi(u)
	gid, err2 := strconv.Atoi(g)
	if !ok || err1 != nil || err2 != nil || uid < 0 || gid < 0 {
		return 0, 0, fmt.Errorf("desktoplink relay: --owner is uid:gid, not %q", s)
	}
	return uid, gid, nil
}

type relay struct {
	token   string
	bound   map[string][]Tool
	mu      sync.Mutex
	out     io.Writer
	log     io.Writer
	next    uint64
	pending map[uint64]chan Answer
	closed  bool
}

func (r *relay) send(f Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err = r.out.Write(append(b, '\n'))
	return err
}

// readAnswers hands each answer to the call it names, until in ends.
func (r *relay) readAnswers(in *bufio.Reader) error {
	for {
		line, err := readLine(in)
		if err != nil {
			r.failAll()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if len(line) == 0 {
			continue
		}
		f, err := ReadFrame(line)
		if err != nil || f.Answer == nil || f.ID == 0 {
			_, _ = fmt.Fprintln(r.log, "desktoplink relay: a line from the CLI that is not an answer was dropped")
			continue
		}
		r.mu.Lock()
		ch := r.pending[f.ID]
		delete(r.pending, f.ID)
		r.mu.Unlock()
		if ch != nil {
			ch <- *f.Answer
		}
	}
}

// failAll ends every call still waiting: no answer will come.
func (r *relay) failAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for id, ch := range r.pending {
		close(ch)
		delete(r.pending, id)
	}
}

// accepts says a request is a call this relay carries: the run token, a
// bound server, and a tool of it.
func (r *relay) accepts(req Request) bool {
	if req.Token != r.token || req.Tool == "" {
		return false
	}
	tools, ok := r.bound[req.Server]
	if !ok {
		return false
	}
	for _, t := range tools {
		if t.Name == req.Tool {
			return true
		}
	}
	return false
}

// serve carries one connection's call and writes its answer back.
func (r *relay) serve(ctx context.Context, c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(time.Minute))
	req, err := ReadRequest(c)
	if err != nil && !errors.Is(err, io.EOF) || err == nil && !r.accepts(req) {
		writeAnswer(c, TextAnswer(refusedLine, true))
		return
	}
	if err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	ch := make(chan Answer, 1)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		writeAnswer(c, TextAnswer("Agentry Desktop did not answer: the desktop link's relay has stopped", true))
		return
	}
	r.next++
	id := r.next
	r.pending[id] = ch
	r.mu.Unlock()
	if err := r.send(Frame{ID: id, Server: req.Server, Token: req.Token, Request: &Call{Tool: req.Tool, Arguments: req.Arguments}}); err != nil {
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		writeAnswer(c, TextAnswer("Agentry Desktop did not answer: "+err.Error(), true))
		return
	}
	cctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	select {
	case a, ok := <-ch:
		if !ok {
			a = TextAnswer("Agentry Desktop did not answer: the desktop link's relay has stopped", true)
		}
		writeAnswer(c, a)
	case <-cctx.Done():
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		writeAnswer(c, TextAnswer("Agentry Desktop did not answer in time", true))
	}
}

func writeAnswer(c net.Conn, a Answer) {
	b, err := json.Marshal(a)
	if err != nil {
		return
	}
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, _ = c.Write(append(b, '\n'))
}
