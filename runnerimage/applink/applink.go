// Package applink is the app link: the MCP server through which a persistent
// agent reaches the application that runs it, and nothing else. The gateway
// launches it on the runner image with no network and one bind, a socket the
// agent's supervisor serves; each tool call is forwarded over that socket as
// one request, and the supervisor's answer is returned as the tool's result.
// It holds no credential, and the tools it lists are a fixed set per agent:
// none of them changes what an agent is given, so an agent cannot widen its
// own grants through it.
//
// This package is part of the embedded runner-image build context and
// therefore MUST remain stdlib-only.
package applink

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sort"
	"time"
)

// ServerName is the app link's name in the gateway's catalog.
const ServerName = "agentry-applink"

// SocketMount is where the app link finds the supervisor's socket: the
// supervisor's own, bound by its exact path, or, where the engine's daemon
// runs in a VM, the relay's, in a volume mounted at SocketDir.
const (
	SocketDir   = "/run/agentry"
	SocketMount = SocketDir + "/app.sock"
)

// The tool sets, by the agent they are for.
const (
	SetMain      = "main"
	SetAssistant = "assistant"
)

// Tool is one tool of a set: its name, what it does, and its input schema.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func strs(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func integer(desc string, min, max int) map[string]any {
	return map[string]any{"type": "integer", "minimum": min, "maximum": max, "description": desc}
}

// SelfConfigure is the Assistant's tool for its own name and personality.
const SelfConfigure = "self_configure"

// Recall is the Assistant's search of its own earlier conversations: it
// reads what was said, and changes nothing.
const Recall = "recall"

// RecallMaxLimit bounds the turns one recall answers with.
const RecallMaxLimit = 20

// The Assistant's task tools: the tasks its heartbeat fires, which it
// registers itself. A task is a turn at a time; it grants nothing.
const (
	TaskAdd    = "task_add"
	TaskList   = "task_list"
	TaskUpdate = "task_update"
	TaskRemove = "task_remove"
)

// helperTools are Agentry Helper's tools, which reach the application's own
// operations, each under the application's own consent; none changes a
// setting's value, starts or stops an agent, publishes, signs in or touches
// a credential.
var helperTools = []Tool{
	{"registry_search", "Search Agentry's registry for QuickAgents by what they do. Returns each match's reference, summary and rating.",
		obj(map[string]any{"query": str("what the QuickAgent should do, in a few words")}, "query")},
	{"quickagent_show", "Show one QuickAgent: what it does, what it may reach, its versions and its security report.",
		obj(map[string]any{"ref": str("its reference, namespace/name or namespace/name@version")}, "ref")},
	{"quickagent_install", "Install a QuickAgent on this computer. The person is asked in Agentry first, and nothing is installed unless they agree.",
		obj(map[string]any{"ref": str("its reference")}, "ref")},
	{"quickagent_run", "Run an installed QuickAgent on this computer, on files from the share folder. The person is asked in Agentry before its first run; the run is an ordinary run, listed in Recent runs, and this returns its id.",
		obj(map[string]any{"ref": str("its reference"), "task": str("what to ask it to do"), "files": strs("paths of files in the share folder to give it, as share/…")}, "ref")},
	{"run_result", "Read a run's outcome: its status, its result and where its files are.",
		obj(map[string]any{"run": str("the run id quickagent_run returned")}, "run")},
	{"installed_list", "List the QuickAgents installed on this computer.", obj(map[string]any{})},
	{"schedule_add", "Schedule an installed QuickAgent to run on its own. The person is asked in Agentry first.",
		obj(map[string]any{"ref": str("its reference"), "when": str("when it runs, as Agentry's schedule takes it (\"every weekday at 09:00\")"), "task": str("what to ask it to do")}, "ref", "when")},
	{"schedule_list", "List the scheduled runs.", obj(map[string]any{})},
	{"schedule_pause", "Pause or resume a scheduled run.",
		obj(map[string]any{"schedule": str("the schedule's id"), "paused": boolean("true to pause, false to resume")}, "schedule", "paused")},
	{"schedule_remove", "Remove a scheduled run. The person is asked in Agentry first.",
		obj(map[string]any{"schedule": str("the schedule's id")}, "schedule")},
	{"settings_open", "Open a section of Agentry's Settings for the person. It changes nothing: the person does.",
		obj(map[string]any{"section": str("the section, as Settings names it")}, "section")},
	{"docs_read", "Read a page of Agentry's documentation, as text.",
		obj(map[string]any{"page": str("the page's path on docs.secureagentry.net, such as /desktop/")}, "page")},
}

// assistantTools are the Assistant's own tools: they change its own name and
// personality and its own tasks, and nothing else.
var assistantTools = []Tool{
	{SelfConfigure, "Save the name the person gives you and the personality they describe for you. Call it only with what the person asked for; it changes nothing else.",
		obj(map[string]any{"name": str("the name they want you to go by"), "personality": str("the personality they described, in their words")})},
	{TaskAdd, "Register a task: an instruction you will be sent as a turn of its own at the times a cron expression names, while you are running. Use it when the person asks you to do something at a time or on a schedule (a reminder, a daily summary), or when you agree to follow something up. At most 25 tasks, none more often than every 15 minutes. Returns the task's id and its next time.",
		obj(map[string]any{
			"name":        str("a short name for the task, as the person would call it"),
			"cron":        str("when it fires: five cron fields, minute hour day-of-month month day-of-week, such as \"0 8 * * 1-5\" for 08:00 on weekdays or \"30 17 21 10 *\" for 17:30 on 21 October"),
			"zone":        str("the IANA time zone the time is in, such as Europe/Paris; leave it out for this computer's"),
			"instruction": str("what you are to do when it fires, written to yourself: it is all you are given then"),
		}, "name", "cron", "instruction")},
	{TaskList, "List your tasks: each one's id, name, time, zone, state, next time, and when it last fired with the first line of what you answered.", obj(map[string]any{})},
	{TaskUpdate, "Change one of your tasks: its name, time, zone or instruction, or pause or resume it. Only what you give changes.",
		obj(map[string]any{
			"task":        str("the task's id, from task_list"),
			"name":        str("a new name"),
			"cron":        str("a new time, as five cron fields"),
			"zone":        str("a new IANA time zone"),
			"instruction": str("a new instruction"),
			"paused":      boolean("true to pause it, false to resume it"),
		}, "task")},
	{TaskRemove, "Remove one of your tasks.", obj(map[string]any{"task": str("the task's id, from task_list")}, "task")},
	{Recall, "Search what you and the person said in your earlier conversations, including the ones your memory was compacted from: the person's messages, your answers and your tasks' turns, never tool results. Returns the matching turns, newest first, each with its date, who said it, its segment and turn, and at most 600 characters around the match. Use it when your memory has nothing on something the person says you discussed, before you say there was nothing, and say that the answer came from the earlier conversation.",
		obj(map[string]any{
			"query": str("the words to look for: every word must appear in a turn, in any case"),
			"from":  str("only turns on or after this date, YYYY-MM-DD"),
			"to":    str("only turns on or before this date, YYYY-MM-DD"),
			"limit": integer("at most this many turns (10 when left out)", 1, RecallMaxLimit),
		}, "query")},
}

// sets are the tools each agent's app link lists. Agentry Helper's (main)
// are the helper tools. The Assistant's are its own, every one of Agentry
// Helper's, so it can do whatever the Helper can, and the meeting tools,
// which its supervisor answers only while the person lets it set up
// meeting recordings.
var sets = map[string][]Tool{
	SetMain:      helperTools,
	SetAssistant: slices.Concat(assistantTools, helperTools, meetingTools),
}

// Sets are the tool sets there are.
func Sets() []string {
	out := make([]string, 0, len(sets))
	for k := range sets {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Tools are a set's tool names, in a fixed order; none for an unknown set.
func Tools(set string) []string {
	var out []string
	for _, t := range sets[set] {
		out = append(out, t.Name)
	}
	return out
}

// Request is one tool call as the app link sends it over the socket: one
// JSON line on a connection of its own.
type Request struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Answer is the supervisor's answer, one JSON line.
type Answer struct {
	Text    string `json:"text"`
	IsError bool   `json:"isError,omitempty"`
}

// CallTimeout bounds one call: a QuickAgent's run is answered once it is
// started, and a consent waits for the person.
var CallTimeout = 30 * time.Minute

// MaxLine bounds a line either way, on the socket and on the relay's stdio.
const MaxLine = 4 << 20

// Serve answers MCP on stdio for set, forwarding each call to socket.
func Serve(ctx context.Context, in io.Reader, out, logw io.Writer, set, socket string) error {
	if _, ok := sets[set]; !ok {
		return fmt.Errorf("applink: no tool set %q (the sets are %v)", set, Sets())
	}
	s := &server{set: set, socket: socket, out: out, logw: logw}
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
	set, socket string
	out, logw   io.Writer
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
			"serverInfo":      map[string]any{"name": ServerName, "version": "1"},
		}})
	case "ping":
		s.write(map[string]any{"id": req.ID, "result": map[string]any{}})
	case "tools/list":
		var list []map[string]any
		for _, t := range sets[s.set] {
			list = append(list, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.Schema})
		}
		s.write(map[string]any{"id": req.ID, "result": map[string]any{"tools": list}})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &p) != nil || !slices.Contains(Tools(s.set), p.Name) {
			s.write(map[string]any{"id": req.ID, "error": map[string]any{"code": -32602, "message": "unknown tool: " + p.Name}})
			return
		}
		a := s.forward(ctx, Request{Tool: p.Name, Arguments: p.Arguments})
		s.write(map[string]any{"id": req.ID, "result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": a.Text}},
			"isError": a.IsError,
		}})
	default:
		s.write(map[string]any{"id": req.ID, "error": map[string]any{"code": -32601, "message": "method not found: " + req.Method}})
	}
}

// forward sends one call to the supervisor and reads its answer.
func (s *server) forward(ctx context.Context, req Request) Answer {
	a, err := Call(ctx, s.socket, req)
	if err != nil {
		_, _ = fmt.Fprintf(s.logw, "applink: %s: %v\n", req.Tool, err)
		return Answer{Text: "Agentry did not answer: " + err.Error(), IsError: true}
	}
	return a
}

// Call sends one request over the socket and reads one answer.
func Call(ctx context.Context, socket string, req Request) (Answer, error) {
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
		return Answer{}, fmt.Errorf("an answer that does not parse: %w", err)
	}
	return a, nil
}

// ReadRequest reads one request from a connection, as the supervisor does.
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
