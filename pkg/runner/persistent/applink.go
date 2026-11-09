package persistent

// The owner side of a persistent agent's app link. The gateway runs the app
// link (runnerimage/applink) with one bind, a socket of the supervisor's own,
// apart from the owner socket: it takes tool calls alone, so nothing that
// reaches it can change the roster, send a turn now or stop the agent. The
// supervisor answers self_configure itself, writing the agent's name and
// personality and nothing else, and the Assistant's task tools, which
// register the turns its heartbeat sends later at their times, with the
// roster those times find (heartbeat.go). It refuses the meeting tools
// unless the person lets the agent set up meeting recordings, and a
// meeting_arm outside its rules (meetings.go). It answers the subagent tools
// itself (subagentrun.go), and Agentry's helper tools through the client's
// HelperOps (helper.go) when the client gives them. It hands
// every other tool to its client's handler (the desktop's, which answers
// with the application's own operations and consent); without one, the tool
// is answered as Agentry Desktop's. When the handler has written a
// QuickAgent's files into the share (quickagent_fetch, quickagent_revise),
// the share's check runs on that folder before the answer goes back, so the
// agent reads them in the same turn. Every call and its answer are a line of the segment's
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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
)

// AppLinkHandler answers an app link's tool call other than self_configure.
type AppLinkHandler func(ctx context.Context, req applink.Request) applink.Answer

// AppSocketPath is the socket an agent's supervisor answers its app link
// on, beside its owner socket.
func AppSocketPath(home, name string) string {
	return filepath.Join(home, "s", "agent-"+name+"-app.sock")
}

// appLinkLog is the segment's record of its app link's calls, each answer
// cut at maxLoggedAnswer bytes.
const (
	appLinkLog      = "applink.log"
	maxLoggedAnswer = 4096
	maxAppLinkCalls = 4
)

// listenAppLink serves the app link's socket for an agent whose built-in has
// one: made 0600 before the first segment, so the segment can bind it, and
// kept across segments until Stop.
func (s *Supervisor) listenAppLink() error {
	// An agent from the registry may be given subagents while it runs, and
	// with them an app link it had none of.
	if a := s.agent(); a.AppLink() == "" && (a.Release == nil || a.Source == SourceLocal) {
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
	s.appCtx, s.appCancel = context.WithCancel(context.Background())
	s.mu.Unlock()
	// A few calls at a time: a harness makes them one by one, and a
	// flood waits rather than piles up.
	slots := make(chan struct{}, maxAppLinkCalls)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			slots <- struct{}{}
			go func() {
				defer func() { <-slots }()
				s.serveAppLink(c)
			}()
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
	s.mu.Lock()
	base := s.appCtx
	s.mu.Unlock()
	if base == nil {
		return
	}
	ctx, cancel := context.WithTimeout(base, applink.CallTimeout)
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
	cfg, err := LoadConfig(s.opts.Home, s.opts.Agent.Name)
	if err != nil {
		return applink.Answer{Text: "the agent's configuration could not be read: " + err.Error(), IsError: true}
	}
	switch {
	case !slices.Contains(applink.Tools(s.agent().AppLinkWith(cfg)), req.Tool):
		return applink.Answer{Text: "no such tool: " + req.Tool, IsError: true}
	case applink.IsSubagentTool(req.Tool):
		return s.answerSubagent(ctx, cfg, req)
	case isHelperTool(req.Tool) && s.opts.Helper != nil:
		return s.answerHelper(ctx, cfg, req)
	case req.Tool == applink.SelfConfigure:
		return s.selfConfigure(req.Arguments)
	case req.Tool == applink.TellPerson:
		return s.answerTell(req.Arguments)
	case isTaskTool(req.Tool):
		return s.answerTask(req.Arguments, req.Tool)
	case req.Tool == applink.Recall:
		return s.answerRecall(ctx, req.Arguments)
	case applink.IsMeetingTool(req.Tool):
		if a, refused := s.refuseMeeting(req); refused {
			return a
		}
		if s.opts.AppLink != nil {
			return s.opts.AppLink(ctx, req)
		}
	case applink.IsPublisherTool(req.Tool) && s.opts.AppLink != nil:
		return s.checkWritten(ctx, req, s.opts.AppLink(ctx, req))
	case s.opts.AppLink != nil:
		return s.opts.AppLink(ctx, req)
	}
	return applink.Answer{Text: req.Tool + " is answered by Agentry Desktop, and this agent runs without it", IsError: true}
}

// writtenShareFolder is the folder of the share a publisher tool's call
// writes into, quickagents/<namespace>/<name>: quickagent_fetch's ref names
// it, quickagent_revise's folder is it. It is empty for a tool that writes
// nothing there and for arguments that name no such folder.
func writtenShareFolder(req applink.Request) string {
	var args struct {
		Ref    string `json:"ref"`
		Folder string `json:"folder"`
	}
	if json.Unmarshal(req.Arguments, &args) != nil {
		return ""
	}
	var ns, name string
	switch req.Tool {
	case applink.QuickAgentFetch:
		ref, _, _ := strings.Cut(strings.TrimSpace(args.Ref), "@")
		ns, name, _ = strings.Cut(ref, "/")
	case applink.QuickAgentRevise:
		f := strings.TrimSuffix(strings.TrimSpace(args.Folder), "/")
		f = strings.TrimPrefix(strings.TrimPrefix(f, "/agent/"), "share/")
		parts := strings.Split(f, "/")
		if len(parts) != 3 || parts[0] != applink.PublisherFolder {
			return ""
		}
		ns, name = parts[1], parts[2]
	default:
		return ""
	}
	if !refSegment.MatchString(ns) || !refSegment.MatchString(name) {
		return ""
	}
	return applink.PublisherFolder + "/" + ns + "/" + name
}

// refSegment is a namespace or a name in a QuickAgent's reference.
var refSegment = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// checkWritten has the share's check run on the folder a publisher tool's
// call wrote its files into, once the client's handler answered it without
// an error and before the answer reaches the agent, so the agent reads
// those files in the turn that asked for them, under the same check as any
// file added to the share. Nothing outside that folder is checked early,
// and a failed call checks nothing. Files the check could not take stay
// unread until the next turn's check, and the answer says so.
func (s *Supervisor) checkWritten(ctx context.Context, req applink.Request, a applink.Answer) applink.Answer {
	if a.IsError {
		return a
	}
	folder := writtenShareFolder(req)
	if folder == "" {
		return a
	}
	s.mu.Lock()
	sess := s.sess
	s.mu.Unlock()
	c, ok := sess.(engine.ShareFolderChecker)
	if !ok {
		return a
	}
	if err := c.CheckShareFolder(ctx, folder); err != nil {
		a.Text += "\n\nSome of these files could not be checked yet, so they can be read only after the check before your next turn: " + err.Error()
	}
	return a
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
		a := s.agent()
		if _, err := applyChange(s.opts.Home, a, c, Change{Name: args.Name, Personality: args.Personality}, managed.Hold(s.policy()).MalwareCheckOn()); err != nil {
			return err
		}
		c.PersonaGiven = personaKey(a, *c)
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
	}{time.Now().UTC(), req.Tool, loggedArguments(req.Arguments), cutText(a.Text, maxLoggedAnswer), a.IsError})
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, appLinkLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the segment's own receipt
	if err != nil {
		return
	}
	_, werr := f.Write(append(line, '\n'))
	_ = errors.Join(werr, f.Close())
}

// loggedArguments are a call's arguments as the log keeps them: as they
// came, or, past maxLoggedAnswer bytes, a string of their cut text.
func loggedArguments(raw json.RawMessage) json.RawMessage {
	if len(raw) <= maxLoggedAnswer {
		return raw
	}
	b, _ := json.Marshal(cutText(string(raw), maxLoggedAnswer))
	return b
}

// cutText is s cut at max bytes, on a rune boundary, marked as cut.
func cutText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "") + " [cut]"
}

// closeAppLink stops answering the app link and removes its socket.
func (s *Supervisor) closeAppLink() {
	s.mu.Lock()
	ln, cancel := s.appLn, s.appCancel
	s.appLn, s.appCancel = nil, nil
	s.mu.Unlock()
	if cancel != nil {
		// A call in flight is told the agent stopped.
		cancel()
	}
	if ln != nil {
		_ = ln.Close()
		_ = os.Remove(AppSocketPath(s.opts.Home, s.opts.Agent.Name))
	}
}
