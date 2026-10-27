// Command context-measure reads the receipts of runs and of persistent agent
// segments and reports how large each conversation's context grew, which
// tools' results filled it, and what a clear of old tool results after a
// cold pause would have removed.
//
//	go run ./scripts/context-measure [flags] <receipts folder or events.ndjson>...
//
// Each argument is searched for events.ndjson files. A run's events are the
// harness's own lines, read with the adapter its run-plan.json names; a
// session's (an interactive run or a persistent agent's segment) are the
// session records, whose tool results are read from docker-agent's ACP
// messages. Byte counts are of the text a tool returned; tokens are the
// harness's own figures.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
)

// options are the clear being modelled and how to report.
type options struct {
	after         time.Duration
	keepTurns     int
	keepBrowser   int
	browserPrefix string
	top           int
	adapter       string
	json          bool
}

// toolStat is one tool's share of a conversation's results.
type toolStat struct {
	Tool     string `json:"tool"`
	Calls    int    `json:"calls"`
	Results  int    `json:"results"`
	Bytes    int    `json:"bytes"`
	MaxBytes int    `json:"maxBytes"`
}

// coldStart is a turn that began after a pause longer than the cache lives:
// the context the turn's first request resent, and the bytes of tool results
// a clear at that moment would have replaced.
type coldStart struct {
	Turn             int     `json:"turn"`
	GapSeconds       float64 `json:"gapSeconds"`
	ContextTokens    int64   `json:"contextTokens"`
	ResultBytes      int     `json:"resultBytes"`
	ClearableBytes   int     `json:"clearableBytes"`
	BrowserClearable int     `json:"browserClearableBytes"`
}

// measure is one events.ndjson file's report.
type measure struct {
	File           string      `json:"file"`
	Adapter        string      `json:"adapter"`
	Mode           string      `json:"mode"`
	Format         string      `json:"format"`
	Requests       int         `json:"requests"`
	FirstContext   int64       `json:"firstContextTokens"`
	PeakContext    int64       `json:"peakContextTokens"`
	ContextLimit   int64       `json:"contextLimit"`
	SumContext     int64       `json:"sumContextTokens"`
	Compactions    int         `json:"harnessCompactions"`
	Restarts       int         `json:"contextRestarts"`
	Turns          int         `json:"turns"`
	ToolCalls      int         `json:"toolCalls"`
	ToolResults    int         `json:"toolResults"`
	ResultBytes    int         `json:"resultBytes"`
	MaxTurnBytes   int         `json:"maxTurnResultBytes"`
	BrowserResults []int       `json:"browserResultBytes,omitempty"`
	Tools          []toolStat  `json:"tools"`
	ColdStarts     []coldStart `json:"coldStarts,omitempty"`
	Notes          []string    `json:"notes,omitempty"`
}

// result is one tool result as the conversation holds it.
type result struct {
	turn  int
	tool  string
	bytes int
}

// tally accumulates one file's events.
type tally struct {
	opt      options
	m        measure
	tools    map[string]*toolStat
	names    map[string]string // call id -> tool
	results  []result
	turnSize map[int]int
	answered map[string]bool // call ids whose result is counted
	lastCtx  int64
	lastTime time.Time
}

func newTally(opt options, file, adapterName, mode, format string) *tally {
	return &tally{
		opt:      opt,
		m:        measure{File: file, Adapter: adapterName, Mode: mode, Format: format},
		tools:    map[string]*toolStat{},
		names:    map[string]string{},
		turnSize: map[int]int{},
		answered: map[string]bool{},
	}
}

func (t *tally) tool(name string) *toolStat {
	name = toolName(name)
	s := t.tools[name]
	if s == nil {
		s = &toolStat{Tool: name}
		t.tools[name] = s
	}
	return s
}

func (t *tally) call(id, name string) {
	t.m.ToolCalls++
	t.tool(name).Calls++
	if id != "" && name != "" {
		t.names[id] = name
	}
}

func (t *tally) result(id, name string, n int) {
	if id != "" {
		if t.answered[id] {
			return
		}
		t.answered[id] = true
	}
	if name == "" {
		name = t.names[id]
	}
	s := t.tool(name)
	s.Results++
	s.Bytes += n
	if n > s.MaxBytes {
		s.MaxBytes = n
	}
	t.m.ToolResults++
	t.m.ResultBytes += n
	t.turnSize[t.m.Turns] += n
	name = toolName(name)
	t.results = append(t.results, result{turn: t.m.Turns, tool: name, bytes: n})
	if t.isBrowser(name) {
		t.m.BrowserResults = append(t.m.BrowserResults, n)
	}
}

func (t *tally) usage(ctx, limit int64, compacted bool) {
	if compacted {
		t.m.Compactions++
	}
	if ctx <= 0 {
		if compacted {
			t.m.Restarts++
			t.results = nil
		}
		return
	}
	t.m.Requests++
	if t.m.FirstContext == 0 {
		t.m.FirstContext = ctx
	}
	if ctx > t.m.PeakContext {
		t.m.PeakContext = ctx
	}
	if limit > t.m.ContextLimit {
		t.m.ContextLimit = limit
	}
	t.m.SumContext += ctx
	// A conversation that got smaller was compacted or started over: the
	// results counted so far are no longer in its history.
	if compacted || (t.lastCtx > 0 && ctx < t.lastCtx*9/10) {
		t.m.Restarts++
		t.results = nil
	}
	t.lastCtx = ctx
}

// toolName is a tool's name as it is printed: a harness's own identifier,
// never free text, so anything else is shown as unnamed.
func toolName(name string) string {
	if name == "" || len(name) > 64 {
		return "(unnamed)"
	}
	if strings.IndexFunc(name, func(r rune) bool { return !nameRune(r) }) >= 0 {
		return "(unnamed)"
	}
	return name
}

// nameRune reports whether r may appear in a tool's name.
func nameRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("_-.:/", r)
}

func (t *tally) isBrowser(name string) bool {
	return t.opt.browserPrefix != "" && strings.HasPrefix(name, t.opt.browserPrefix)
}

// turn starts a new turn at the given time; when the pause before it is
// longer than the cache lives, it records what a clear would remove.
func (t *tally) turn(at time.Time) {
	if t.m.Turns > 0 && !t.lastTime.IsZero() && !at.IsZero() {
		if gap := at.Sub(t.lastTime); gap > t.opt.after {
			t.m.ColdStarts = append(t.m.ColdStarts, t.cold(gap))
		}
	}
	t.m.Turns++
}

// cold models the clear at the start of turn t.m.Turns+1: every result of a
// turn older than the newest keepTurns is cleared, and so is every browser
// result older than the newest keepBrowser of them.
func (t *tally) cold(gap time.Duration) coldStart {
	c := coldStart{Turn: t.m.Turns + 1, GapSeconds: gap.Seconds(), ContextTokens: t.lastCtx}
	oldest := t.m.Turns - t.opt.keepTurns // results of turns <= oldest are cleared
	browserSeen := 0
	for i := len(t.results) - 1; i >= 0; i-- {
		r := t.results[i]
		c.ResultBytes += r.bytes
		browser := t.isBrowser(r.tool)
		if browser {
			browserSeen++
		}
		switch {
		case browser && browserSeen <= t.opt.keepBrowser:
			// The newest browser results stay whole wherever they fall.
		case browser:
			c.ClearableBytes += r.bytes
			c.BrowserClearable += r.bytes
		case r.turn <= oldest:
			c.ClearableBytes += r.bytes
		}
	}
	return c
}

func (t *tally) note(format string, args ...any) {
	n := fmt.Sprintf(format, args...)
	for _, have := range t.m.Notes {
		if have == n {
			return
		}
	}
	t.m.Notes = append(t.m.Notes, n)
}

func (t *tally) finish() measure {
	t.m.Tools = make([]toolStat, 0, len(t.tools))
	for _, s := range t.tools {
		t.m.Tools = append(t.m.Tools, *s)
	}
	sort.Slice(t.m.Tools, func(i, j int) bool {
		if t.m.Tools[i].Bytes != t.m.Tools[j].Bytes {
			return t.m.Tools[i].Bytes > t.m.Tools[j].Bytes
		}
		return t.m.Tools[i].Tool < t.m.Tools[j].Tool
	})
	for _, n := range t.turnSize {
		if n > t.m.MaxTurnBytes {
			t.m.MaxTurnBytes = n
		}
	}
	return t.m
}

// sessionEvent is the part of a session record this command reads.
type sessionEvent struct {
	Kind             string          `json:"kind"`
	Time             time.Time       `json:"time"`
	Text             string          `json:"text"`
	TotalTokens      int64           `json:"totalTokens"`
	ContextTokens    int64           `json:"contextTokens"`
	ContextLimit     int64           `json:"contextLimit"`
	ContextCompacted bool            `json:"contextCompacted"`
	Raw              json.RawMessage `json:"raw"`
}

// acpUpdate is the part of an ACP session/update this command reads.
type acpUpdate struct {
	Method string `json:"method"`
	Params struct {
		Update struct {
			SessionUpdate string          `json:"sessionUpdate"`
			Name          string          `json:"name"`
			Title         string          `json:"title"`
			ToolCallID    string          `json:"toolCallId"`
			Status        string          `json:"status"`
			Content       json.RawMessage `json:"content"`
			Used          int64           `json:"used"`
			Size          int64           `json:"size"`
		} `json:"update"`
	} `json:"params"`
}

// acpText is the text of a tool call update's content, as the model read it.
func acpText(content json.RawMessage) int {
	var items []struct {
		Type    string `json:"type"`
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(content, &items) != nil {
		return 0
	}
	n := 0
	for _, it := range items {
		if it.Type == "content" && it.Content.Type == "text" {
			n += len(it.Content.Text)
		}
	}
	return n
}

// acpOf reads a session record's raw message as an ACP session/update.
func acpOf(raw json.RawMessage) (acpUpdate, bool) {
	var u acpUpdate
	if len(raw) == 0 || json.Unmarshal(raw, &u) != nil || u.Method != "session/update" {
		return acpUpdate{}, false
	}
	return u, true
}

// modelActivity is what the model or its tools did: the time of the last one
// is when the model's previous response ended. A record the runner writes
// for itself (a notice, a note carried into the next turn, an input added)
// is not.
var modelActivity = map[string]bool{
	"assistant_text": true, "assistant_turn": true, "usage": true,
	"tool_call": true, "permission_granted": true, "permission_denied": true,
}

func (t *tally) sessionLine(e sessionEvent) {
	u, isACP := acpOf(e.Raw)
	up := u.Params.Update
	activity := modelActivity[e.Kind]
	switch e.Kind {
	case "user_turn":
		t.turn(e.Time)
	case "usage":
		ctx, limit := e.ContextTokens, e.ContextLimit
		if ctx == 0 && isACP && up.SessionUpdate == "usage_update" {
			ctx, limit = up.Used, up.Size
		}
		t.usage(ctx, limit, e.ContextCompacted)
	case "tool_call":
		// The record's text is the harness's display title, which can
		// carry the call's arguments: only the tool's own name is kept.
		t.call(up.ToolCallID, up.Name)
	case "other":
		if isACP && up.SessionUpdate == "tool_call_update" && (up.Status == "completed" || up.Status == "failed") {
			t.result(up.ToolCallID, up.Name, acpText(up.Content))
			activity = true
		}
	}
	if activity && !e.Time.IsZero() {
		t.lastTime = e.Time
	}
}

func (t *tally) runLine(a adapter.Adapter, line []byte) {
	for _, ev := range a.Events(line) {
		switch ev.Kind {
		case adapter.EventToolCall:
			name := ev.Tool
			if name == "" {
				name = ev.Text
			}
			t.call(ev.CallID, name)
		case adapter.EventToolResult:
			t.result(ev.CallID, ev.Tool, len(ev.Text))
		case adapter.EventUsage:
			// What a request resent is its input: output the model spent on
			// reasoning is not carried into the next request.
			ctx := ev.ContextTokens
			if ctx == 0 && !ev.ContextCompacted {
				ctx = ev.InputTokens
			}
			t.usage(ctx, ev.ContextLimit, ev.ContextCompacted)
		}
	}
}

// planInfo is the part of run-plan.json this command reads.
type planInfo struct {
	Mode    string `json:"mode"`
	Harness struct {
		Adapter string `json:"adapter"`
	} `json:"harness"`
}

func readPlan(dir string) planInfo {
	var p planInfo
	if b, err := os.ReadFile(filepath.Join(dir, "run-plan.json")); err == nil {
		_ = json.Unmarshal(b, &p)
	}
	return p
}

// measureFile reads one events.ndjson.
func measureFile(opt options, path string) (measure, error) {
	f, err := os.Open(path)
	if err != nil {
		return measure{}, err
	}
	defer func() { _ = f.Close() }()
	plan := readPlan(filepath.Dir(path))
	if plan.Harness.Adapter == "" {
		plan.Harness.Adapter = opt.adapter
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)

	var t *tally
	var a adapter.Adapter
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if t == nil {
			var probe sessionEvent
			if json.Unmarshal(line, &probe) == nil && probe.Kind != "" && !probe.Time.IsZero() {
				t = newTally(opt, path, plan.Harness.Adapter, plan.Mode, "session")
			} else {
				t = newTally(opt, path, plan.Harness.Adapter, plan.Mode, "run")
				if a, err = adapter.Get(plan.Harness.Adapter); err != nil {
					return measure{}, fmt.Errorf("%s: no adapter for %q beside it (run-plan.json): %w", path, plan.Harness.Adapter, err)
				}
				t.turn(time.Time{})
			}
		}
		if t.m.Format == "session" {
			var e sessionEvent
			if json.Unmarshal(line, &e) == nil {
				t.sessionLine(e)
			}
			continue
		}
		// An adapter may keep the line it is given; the scanner reuses its buffer.
		t.runLine(a, append([]byte(nil), line...))
	}
	if err := sc.Err(); err != nil {
		return measure{}, fmt.Errorf("%s: %w", path, err)
	}
	if t == nil {
		return measure{File: path, Adapter: plan.Harness.Adapter, Mode: plan.Mode, Tools: []toolStat{}}, nil
	}
	if t.m.Format == "session" && t.m.ToolCalls > 0 && t.m.ToolResults == 0 {
		t.note("no tool result was found: a session's results are read from docker-agent's ACP messages only")
	}
	if t.m.Format == "run" && t.m.Requests > 0 && t.m.ContextLimit == 0 {
		t.note("context is each usage event's input as the harness reported it, which can be per turn rather than per request")
	}
	if t.m.Format == "run" && strings.HasPrefix(t.m.Adapter, "claude") {
		t.note("claude reports input without its cache reads, so this context is understated")
	}
	return t.finish(), nil
}

// findEvents lists every events.ndjson at or under each argument.
func findEvents(args []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, arg := range args {
		info, err := os.Stat(arg)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if !seen[arg] {
				seen[arg] = true
				out = append(out, arg)
			}
			continue
		}
		err = filepath.WalkDir(arg, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrPermission) {
					return nil
				}
				return err
			}
			if !d.IsDir() && d.Name() == "events.ndjson" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

func kib(n int) string { return fmt.Sprintf("%.1f", float64(n)/1024) }

// printer writes formatted lines and keeps the first error.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) f(format string, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, args...)
	}
}

func report(w io.Writer, opt options, ms []measure) error {
	p := &printer{w: w}
	for _, m := range ms {
		p.f("%s\n", m.File)
		p.f("  %s %s (%s): %d requests, %d turns, context first %d, peak %d of %d, summed over requests %d, harness compactions %d, restarts %d\n",
			orDash(m.Adapter), orDash(m.Mode), m.Format, m.Requests, m.Turns, m.FirstContext, m.PeakContext, m.ContextLimit, m.SumContext, m.Compactions, m.Restarts)
		p.f("  tools: %d calls, %d results, %s KiB of results, largest turn %s KiB\n",
			m.ToolCalls, m.ToolResults, kib(m.ResultBytes), kib(m.MaxTurnBytes))
		for i, s := range m.Tools {
			if i == opt.top {
				p.f("    … %d more tools\n", len(m.Tools)-opt.top)
				break
			}
			p.f("    %-32s %4d calls %4d results %9s KiB (largest %s KiB)\n", s.Tool, s.Calls, s.Results, kib(s.Bytes), kib(s.MaxBytes))
		}
		for _, c := range m.ColdStarts {
			p.f("  cold start before turn %d after %.0fs: context %d, results held %s KiB, a clear would replace %s KiB (browser %s KiB)\n",
				c.Turn, c.GapSeconds, c.ContextTokens, kib(c.ResultBytes), kib(c.ClearableBytes), kib(c.BrowserClearable))
		}
		for _, n := range m.Notes {
			p.f("  note: %s\n", n)
		}
	}
	return p.err
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func run(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("context-measure", flag.ContinueOnError)
	fl.SetOutput(stderr)
	var opt options
	fl.DurationVar(&opt.after, "after", 5*time.Minute, "a pause longer than this before a turn counts as a cold start")
	fl.IntVar(&opt.keepTurns, "keep-turns", 2, "the newest turns whose tool results a clear keeps whole")
	fl.IntVar(&opt.keepBrowser, "keep-browser", 2, "the newest browser results a clear keeps whole")
	fl.StringVar(&opt.browserPrefix, "browser-prefix", "browser_", "the name prefix of the browser server's tools")
	fl.IntVar(&opt.top, "top", 8, "tools listed per conversation")
	fl.StringVar(&opt.adapter, "adapter", "", "the adapter that wrote a run's events when no run-plan.json beside them names it")
	fl.BoolVar(&opt.json, "json", false, "print one JSON array instead of the table")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: context-measure [flags] <receipts folder or events.ndjson>...")
		return 2
	}
	files, err := findEvents(fl.Args())
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	ms := make([]measure, 0, len(files))
	failed := false
	for _, p := range files {
		m, err := measureFile(opt, p)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			failed = true
			continue
		}
		ms = append(ms, m)
	}
	var werr error
	if opt.json {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		werr = enc.Encode(ms)
	} else {
		werr = report(stdout, opt, ms)
	}
	if werr != nil {
		_, _ = fmt.Fprintln(stderr, werr)
		return 1
	}
	if failed {
		return 1
	}
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
