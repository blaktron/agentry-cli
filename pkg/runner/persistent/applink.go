package persistent

// The owner side of a persistent agent's app link. The gateway runs the app
// link (runnerimage/applink) with one bind, a socket of the supervisor's own,
// apart from the owner socket: it takes tool calls alone, so nothing that
// reaches it can change the roster, send a turn or stop the agent. The
// supervisor answers self_configure itself, writing the agent's name and
// personality and nothing else, and hands every other tool to its client's
// handler (the desktop's, which answers with the application's own
// operations and consent); without one, the tool is answered as Agentry
// Desktop's. Every call and its answer are a line of the segment's
// receipts/applink.log.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/applink"
)

// AppLinkHandler answers an app link's tool call other than self_configure.
type AppLinkHandler func(ctx context.Context, req applink.Request) applink.Answer

// AppSocketPath is the socket an agent's supervisor answers its app link
// on, beside its owner socket.
func AppSocketPath(home, name string) string {
	return filepath.Join(home, "s", "agent-"+name+"-app.sock")
}

// appLinkLog is the segment's record of its app link's calls.
const appLinkLog = "applink.log"

// listenAppLink serves the app link's socket for an agent whose built-in has
// one: made 0600 before the first segment, so the segment can bind it, and
// kept across segments until Stop.
func (s *Supervisor) listenAppLink() error {
	if s.opts.Agent.Builtin.AppLink() == "" {
		return nil
	}
	path := AppSocketPath(s.opts.Home, s.opts.Agent.Name)
	if len(path) > maxSocketPath {
		return fmt.Errorf("the %s agent's app link socket %s is longer than a socket's name may be; use a shorter runner home", s.opts.Agent.Name, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("the %s agent's app link socket: %w", s.opts.Agent.Name, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	s.mu.Lock()
	s.appLn = ln
	s.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serveAppLink(c)
		}
	}()
	return nil
}

// serveAppLink answers one call on one connection.
func (s *Supervisor) serveAppLink(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(time.Minute))
	req, err := applink.ReadRequest(c)
	if err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	ctx, cancel := context.WithTimeout(context.Background(), applink.CallTimeout)
	defer cancel()
	a := s.answerAppLink(ctx, req)
	s.logAppLink(req, a)
	b, err := json.Marshal(a)
	if err != nil {
		return
	}
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, _ = c.Write(append(b, '\n'))
}

// answerAppLink answers a call of the agent's own tool set.
func (s *Supervisor) answerAppLink(ctx context.Context, req applink.Request) applink.Answer {
	set := s.opts.Agent.Builtin.AppLink()
	known := false
	for _, t := range applink.Tools(set) {
		known = known || t == req.Tool
	}
	switch {
	case !known:
		return applink.Answer{Text: "no such tool: " + req.Tool, IsError: true}
	case req.Tool == applink.SelfConfigure:
		return s.selfConfigure(req.Arguments)
	case s.opts.AppLink != nil:
		return s.opts.AppLink(ctx, req)
	}
	return applink.Answer{Text: req.Tool + " is answered by Agentry Desktop, and this agent runs without it", IsError: true}
}

// selfConfigure saves the name and the personality the agent was given,
// and nothing else: an argument it does not know refuses the call. The
// conversation has them already, so they are not given again at the next
// turn.
func (s *Supervisor) selfConfigure(raw json.RawMessage) applink.Answer {
	var args struct {
		Name        *string `json:"name"`
		Personality *string `json:"personality"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if len(bytes.TrimSpace(raw)) == 0 || dec.Decode(&args) != nil {
		return applink.Answer{Text: "self_configure takes a name, a personality, or both, and nothing else", IsError: true}
	}
	if args.Name == nil && args.Personality == nil {
		return applink.Answer{Text: "self_configure needs a name, a personality, or both", IsError: true}
	}
	var saved Config
	err := s.updateConfig(func(c *Config) error {
		if _, err := ApplyChange(s.opts.Home, s.opts.Agent, c, Change{Name: args.Name, Personality: args.Personality}); err != nil {
			return err
		}
		c.PersonaGiven = personaKey(*c)
		saved = *c
		return nil
	})
	if err != nil {
		return applink.Answer{Text: "not saved: " + err.Error(), IsError: true}
	}
	s.mu.Lock()
	s.persona = saved.PersonaGiven
	s.mu.Unlock()
	if saved.DisplayName == "" {
		return applink.Answer{Text: "Saved. You have no name."}
	}
	return applink.Answer{Text: "Saved. You go by " + strconv.Quote(saved.DisplayName) + " from now on."}
}

// logAppLink appends a call and its answer to the segment's receipts.
func (s *Supervisor) logAppLink(req applink.Request, a applink.Answer) {
	s.mu.Lock()
	dir := filepath.Join(s.outDir, "receipts")
	s.mu.Unlock()
	line, err := json.Marshal(struct {
		Time      time.Time       `json:"time"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
		Answer    string          `json:"answer"`
		IsError   bool            `json:"isError,omitempty"`
	}{time.Now().UTC(), req.Tool, req.Arguments, a.Text, a.IsError})
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, appLinkLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the segment's own receipt
	if err != nil {
		return
	}
	_, werr := f.Write(append(line, '\n'))
	_ = errors.Join(werr, f.Close())
}

// closeAppLink stops answering the app link and removes its socket.
func (s *Supervisor) closeAppLink() {
	s.mu.Lock()
	ln := s.appLn
	s.appLn = nil
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
		_ = os.Remove(AppSocketPath(s.opts.Home, s.opts.Agent.Name))
	}
}
