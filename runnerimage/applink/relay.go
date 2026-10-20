package applink

// The relay carries the app link's calls where the container engine's daemon
// runs in a VM (Docker Desktop, Colima, a podman machine), whose containers a
// socket on the person's computer does not reach. It runs in a container of
// the run's own with no network, serves the app link's socket in a volume
// the two containers share, and carries each call over its own stdio, which
// the CLI holds through the engine's attach, to the supervisor's socket on
// the computer. Only a tool call crosses: a connection's one line must read
// as a Request naming a tool, and anything else is refused at the socket
// without reaching the CLI.

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
	"strconv"
	"strings"
	"sync"
	"time"
)

// Frame is one line between the relay and the CLI: the relay's ready line,
// a call it carries out (ID and Request), or the answer carried back (ID
// and Answer).
type Frame struct {
	ID      uint64   `json:"id,omitempty"`
	Ready   bool     `json:"ready,omitempty"`
	Request *Request `json:"request,omitempty"`
	Answer  *Answer  `json:"answer,omitempty"`
}

// ReadFrame decodes one frame strictly: a field it does not know refuses it.
func ReadFrame(line []byte) (Frame, error) {
	var f Frame
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Frame{}, err
	}
	return f, nil
}

// refusedLine is the answer to a connection whose line is not a tool call.
const refusedLine = "the app link takes one tool call a connection, and this was not one"

// maxRelayCalls bounds the calls the relay carries at once; the supervisor
// answers as many.
const maxRelayCalls = 4

// RelayConfig is what the relay serves.
type RelayConfig struct {
	// Listen is the socket's path.
	Listen string
	// Owner is the uid:gid the socket is given, the app link's user; empty
	// leaves it the relay's.
	Owner string
	// In carries the answers from the CLI, Out the calls to it.
	In  io.Reader
	Out io.Writer
	// Log takes what goes wrong.
	Log io.Writer
}

// Relay serves the socket until In ends or ctx is done.
func Relay(ctx context.Context, cfg RelayConfig) error {
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	_ = os.Remove(cfg.Listen)
	ln, err := net.Listen("unix", cfg.Listen)
	if err != nil {
		return fmt.Errorf("applink relay: %w", err)
	}
	defer func() { _ = ln.Close() }()
	if err := os.Chmod(cfg.Listen, 0o600); err != nil {
		return fmt.Errorf("applink relay: %w", err)
	}
	if cfg.Owner != "" {
		uid, gid, err := parseOwner(cfg.Owner)
		if err != nil {
			return err
		}
		if err := os.Chown(cfg.Listen, uid, gid); err != nil {
			return fmt.Errorf("applink relay: give the socket to %s: %w", cfg.Owner, err)
		}
	}
	r := &relay{out: cfg.Out, log: cfg.Log, pending: map[uint64]chan Answer{}}
	if err := r.send(Frame{Ready: true}); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	inDone := make(chan struct{})
	go func() {
		if err := r.readAnswers(cfg.In); err != nil {
			_, _ = fmt.Fprintf(r.log, "applink relay: reading the CLI's answers: %v\n", err)
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
			return fmt.Errorf("applink relay: %w", err)
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

func parseOwner(s string) (int, int, error) {
	u, g, ok := strings.Cut(s, ":")
	uid, err1 := strconv.Atoi(u)
	gid, err2 := strconv.Atoi(g)
	if !ok || err1 != nil || err2 != nil || uid < 0 || gid < 0 {
		return 0, 0, fmt.Errorf("applink relay: --owner is uid:gid, not %q", s)
	}
	return uid, gid, nil
}

type relay struct {
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
func (r *relay) readAnswers(in io.Reader) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), MaxLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		f, err := ReadFrame(line)
		if err != nil || f.Answer == nil || f.ID == 0 {
			_, _ = fmt.Fprintln(r.log, "applink relay: a line from the CLI that is not an answer was dropped")
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
	r.failAll()
	return sc.Err()
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

// serve carries one connection's call and writes its answer back.
func (r *relay) serve(ctx context.Context, c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(time.Minute))
	req, err := ReadRequest(c)
	if err != nil && !errors.Is(err, io.EOF) || err == nil && req.Tool == "" {
		writeAnswer(c, Answer{Text: refusedLine, IsError: true})
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
		writeAnswer(c, Answer{Text: "Agentry did not answer: the app link's relay has stopped", IsError: true})
		return
	}
	r.next++
	id := r.next
	r.pending[id] = ch
	r.mu.Unlock()
	if err := r.send(Frame{ID: id, Request: &Request{Tool: req.Tool, Arguments: req.Arguments}}); err != nil {
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		writeAnswer(c, Answer{Text: "Agentry did not answer: " + err.Error(), IsError: true})
		return
	}
	cctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	select {
	case a, ok := <-ch:
		if !ok {
			a = Answer{Text: "Agentry did not answer: the app link's relay has stopped", IsError: true}
		}
		writeAnswer(c, a)
	case <-cctx.Done():
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		writeAnswer(c, Answer{Text: "Agentry did not answer in time", IsError: true})
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
