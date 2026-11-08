// Package desktoplink is the desktop link: the MCP servers through which a
// run reaches the applications installed on the person's own computer, and
// nothing else. For each application server a release declares and the run
// bound, the gateway launches one entry of this role on the runner image,
// agentry-desktop-<id>, with no network and one mount, the run's relay
// volume. Each tool call is written to the relay's socket in that volume as
// one request; the relay carries it over its own stdio, which the CLI holds
// through the engine's attach, to Agentry Desktop, which launches the
// installed server as the person, gates the call by its class and answers
// it; the answer comes back the same way and is returned as the tool's
// result. Nothing of the installed server runs in the sandbox, and nothing
// the agent sends names a command: a call carries a tool's name and its
// arguments, and the application's server is the one the person approved
// for it, chosen by Agentry Desktop from the entry's id.
//
// This package is part of the embedded runner-image build context and
// therefore MUST remain stdlib-only.
package desktoplink

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
	"slices"
	"strings"
	"time"
)

// ServerPrefix starts every desktop link entry's name in the gateway's
// catalog: agentry-desktop-<id> for the application server <id>.
const ServerPrefix = "agentry-desktop-"

// ComputerID is the id of the computer-use entry, agentry-desktop-computer,
// whose tools are ComputerTools.
const ComputerID = "computer"

// ServerName is the catalog entry's name for an application server.
func ServerName(id string) string { return ServerPrefix + id }

// Command is a desktop link entry's command, which the run's docker filter
// knows its container by.
func Command(id string) []string { return []string{"desktoplink", "--server", id} }

// RelayRole is the relay's command on the runner image.
const RelayRole = "desktoplink-relay"

// SocketDir is the relay volume's mount point in the relay's container and
// in every desktop link entry; SocketMount the socket the relay serves in
// it, and BindFile the run's binding the relay writes there for the
// entries: the run token and each bound server's tools.
const (
	SocketDir   = "/run/agentry-desktop"
	SocketMount = SocketDir + "/link.sock"
	BindFile    = SocketDir + "/bind.json"
)

// MaxLine bounds a line either way, on the socket and on the relay's stdio.
const MaxLine = 4 << 20

// CallTimeout bounds one call: an exec call waits for the person's answer
// in Agentry Desktop.
var CallTimeout = 30 * time.Minute

// maxRelayCalls bounds the calls the relay carries at once; Agentry
// Desktop answers as many.
const maxRelayCalls = 4

// Content is one item of a tool's result, as MCP carries it: text, an image
// (Data base64 and its MimeType) or an embedded resource's text.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// Answer is a call's answer: the content items the installed server
// returned, or one text item saying why the call was refused.
type Answer struct {
	Content []Content `json:"content,omitempty"`
	IsError bool      `json:"isError,omitempty"`
	// Window names the window a computer-use call saw or acted in, as
	// Agentry Desktop names it (the application and the title), for the
	// run's call log; the agent is not sent it.
	Window string `json:"window,omitempty"`
}

// TextAnswer is an answer of one text item.
func TextAnswer(text string, isError bool) Answer {
	return Answer{Content: []Content{{Type: "text", Text: text}}, IsError: isError}
}

// Text is the answer's text items joined, for a log or a message.
func (a Answer) Text() string {
	var parts []string
	for _, c := range a.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Tool is one tool of a bound server, as Agentry Desktop listed it from the
// installed server and as the desktop link lists it to the gateway.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	// Class is the tool's class as Agentry Desktop gives it (read, act or
	// exec), for the run's counts and call log; Agentry Desktop sends it
	// only to a run that names the link protocol's revision.
	Class string `json:"class,omitempty"`
}

// Bind is the run's binding, written by the relay for the desktop link
// entries alone: the run token Agentry Desktop issued, which every call
// carries, and each bound server's tools.
type Bind struct {
	Token   string            `json:"token"`
	Servers map[string][]Tool `json:"servers"`
}

// Request is one tool call as a desktop link entry writes it to the relay's
// socket: one JSON line on a connection of its own, carrying the run token.
type Request struct {
	Token     string          `json:"token"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Call is a tool call as a frame carries it.
type Call struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Frame is one line between the relay and the CLI: the CLI's binding (first,
// before the relay listens), the relay's ready line, a call it carries out
// (ID, Server, Token and Request), or the answer carried back (ID and
// Answer).
type Frame struct {
	ID      uint64  `json:"id,omitempty"`
	Ready   bool    `json:"ready,omitempty"`
	Bind    *Bind   `json:"bind,omitempty"`
	Server  string  `json:"server,omitempty"`
	Token   string  `json:"token,omitempty"`
	Request *Call   `json:"request,omitempty"`
	Answer  *Answer `json:"answer,omitempty"`
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

// IsCall says the frame is a call and nothing else: an id, a server, a
// token and a request naming a tool, with no answer and no ready mark.
func (f Frame) IsCall() bool {
	return f.ID != 0 && f.Server != "" && f.Token != "" && f.Request != nil && f.Request.Tool != "" &&
		f.Answer == nil && !f.Ready && f.Bind == nil
}

// ReadRequest reads one request line.
func ReadRequest(r io.Reader) (Request, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), MaxLine)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return Request{}, err
		}
		return Request{}, io.EOF
	}
	var req Request
	dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return Request{}, err
	}
	return req, nil
}

// ReadBind reads the binding the relay wrote.
func ReadBind(path string) (Bind, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the relay volume's own file
	if err != nil {
		return Bind{}, err
	}
	var bind Bind
	if err := json.Unmarshal(b, &bind); err != nil {
		return Bind{}, fmt.Errorf("desktoplink: the binding: %w", err)
	}
	return bind, nil
}

// Serve answers MCP on stdio for the application server id, forwarding each
// call to the relay's socket with the run token the binding holds. The
// tools it lists are the bound server's; a server the run did not bind
// lists none and answers every call as not bound.
func Serve(ctx context.Context, in io.Reader, out, logw io.Writer, id, bindFile, socket string) error {
	if id == "" {
		return errors.New("desktoplink: --server is required")
	}
	bind, err := ReadBind(bindFile)
	if err != nil {
		return err
	}
	s := &server{id: id, tools: bind.Servers[id], token: bind.Token, socket: socket, out: out, logw: logw}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), MaxLine)
	for sc.Scan() {
		if line := bytes.TrimSpace(sc.Bytes()); len(line) > 0 {
			s.handleLine(ctx, line)
		}
	}
	return sc.Err()
}

type server struct {
	id        string
	tools     []Tool
	token     string
	socket    string
	out, logw io.Writer
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

func (s *server) write(v map[string]any) {
	v["jsonrpc"] = "2.0"
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = s.out.Write(append(b, '\n'))
}

func (s *server) toolNames() []string {
	names := make([]string, 0, len(s.tools))
	for _, t := range s.tools {
		names = append(names, t.Name)
	}
	return names
}

func (s *server) handleLine(ctx context.Context, line []byte) {
	var req rpcRequest
	if json.Unmarshal(line, &req) != nil || req.Method == "" || len(req.ID) == 0 {
		return
	}
	switch req.Method {
	case "initialize":
		pv := "2025-06-18"
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &p) == nil && p.ProtocolVersion != "" {
			pv = p.ProtocolVersion
		}
		s.write(map[string]any{"id": req.ID, "result": map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": ServerName(s.id), "version": "1"},
		}})
	case "ping":
		s.write(map[string]any{"id": req.ID, "result": map[string]any{}})
	case "tools/list":
		list := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.tools {
			schema := t.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			list = append(list, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": schema})
		}
		s.write(map[string]any{"id": req.ID, "result": map[string]any{"tools": list}})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &p) != nil || !slices.Contains(s.toolNames(), p.Name) {
			s.write(map[string]any{"id": req.ID, "error": map[string]any{"code": -32602, "message": "unknown tool: " + p.Name}})
			return
		}
		a := s.forward(ctx, Request{Token: s.token, Server: s.id, Tool: p.Name, Arguments: p.Arguments})
		s.write(map[string]any{"id": req.ID, "result": map[string]any{"content": a.Content, "isError": a.IsError}})
	default:
		s.write(map[string]any{"id": req.ID, "error": map[string]any{"code": -32601, "message": "method not found: " + req.Method}})
	}
}

// forward sends one call to the relay and reads its answer.
func (s *server) forward(ctx context.Context, req Request) Answer {
	a, err := Send(ctx, s.socket, req)
	if err != nil {
		_, _ = fmt.Fprintf(s.logw, "desktoplink: %s: %v\n", req.Tool, err)
		return TextAnswer("Agentry Desktop did not answer: "+err.Error(), true)
	}
	return a
}

// Send writes one request to the socket and reads one answer.
func Send(ctx context.Context, socket string, req Request) (Answer, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return Answer{}, err
	}
	defer func() { _ = c.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	b, err := json.Marshal(req)
	if err != nil {
		return Answer{}, err
	}
	if _, err := c.Write(append(b, '\n')); err != nil {
		return Answer{}, err
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64*1024), MaxLine)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return Answer{}, err
		}
		return Answer{}, errors.New("the socket closed without an answer")
	}
	var a Answer
	if err := json.Unmarshal(sc.Bytes(), &a); err != nil {
		return Answer{}, fmt.Errorf("the answer was not one: %w", err)
	}
	return a, nil
}
