// The MCP wire loop: newline-delimited JSON-RPC 2.0 on
// stdio — one JSON object per line, no Content-Length framing — which is
// the framing the Docker MCP Gateway speaks to the servers it launches.
// Only the method subset a one-shot tool server needs is implemented:
// initialize, the initialized notification, ping, tools/list, tools/call.
//
// Requests are handled serially on the read goroutine: a one-shot agent's
// tool calls arrive one at a time anyway, and serial handling is what
// lets the path checks in paths.go ignore check-to-use races. Response
// writes still take a mutex so future parallelism cannot interleave two
// half-written lines.

package toolsrv

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// maxLineBytes bounds one JSON-RPC line. 4 MiB comfortably exceeds
	// the largest legitimate request (a capped fs_write payload plus
	// envelope) while refusing to buffer unbounded garbage.
	maxLineBytes = 4 << 20
	// defaultProtocolVersion is offered when the client names none. The
	// server otherwise echoes the client's requested version: the gateway
	// is the only client, and this surface has no version-specific
	// behavior worth failing a handshake over.
	defaultProtocolVersion = "2025-06-18"
	serverName             = "agentry-runner-tools"
	serverVersion          = "1"
)

// JSON-RPC 2.0 error codes (the spec's reserved range).
const (
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// rpcRequest is one incoming message. ID is kept raw so string, number,
// and null ids echo back byte-identically; a nil ID (absent member) marks
// a notification.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// rpcResponse is one outgoing message; exactly one of Result/Error is set.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// server is one configured tool server: the granted tool table plus the
// exec shim state, built once so tools/list and tools/call cannot drift
// from each other mid-run.
type server struct {
	spec    *Spec
	out     io.Writer
	outMu   sync.Mutex
	logw    io.Writer
	tools   map[string]toolDef
	order   []string
	shimDir string
	// shims maps a declared command to its shim symlink; a declared
	// command absent from the image is absent here (tools.go).
	shims map[string]string
	// calendars holds each calendar feed's last fetch (calendar.go).
	calendars calendarCache
	// confine is how a declared command is held to the places it may
	// write (confinement.go); set when exec is granted.
	confine confinement
	// webState is the web tools' per-run state, built on first use
	// (web.go); a test may set it first.
	webOnce  sync.Once
	webState *webState
	// reviewState is the review grant's per-run state, built on first use
	// (review.go); a test may set it first.
	reviewOnce  sync.Once
	reviewState *reviewState
	// reads is the current turn's re-read log (reread.go), and callStubbed
	// says the call being handled was answered with its stub.
	reads       rereadLog
	callStubbed bool
	// callFault says the call being handled failed on the run's own
	// machinery (calls.go): set by failed, read and cleared by recordCall.
	callFault bool
	// callPersonsInput says the answer of the call being handled is the
	// person's own input, served unchanged as a person's-input tool's is:
	// set by grep for a search of the inputs (search.go), cleared before
	// every call.
	callPersonsInput bool
	// search, rgBin and searchTimeout are grep and glob's ripgrep, how it
	// is held, and how long a search may take (search.go); set when they
	// are granted, and by a test.
	search        searchConfinement
	rgBin         string
	searchTimeout time.Duration
	// share is what the server remembers of a checked share folder
	// (sharecheck.go).
	share shareState
	// typst, render and pdfTimeout are export_pdf's renderer, how it is
	// held, and how long a render may take (pdf.go); set when it is
	// granted, and by a test.
	typst      typstFiles
	render     renderConfinement
	pdfTimeout time.Duration
	// fetchSeq numbers the bodies fetch saves in the scratch folder
	// unasked, and partSeq the files a saveTo is written to before it is
	// renamed into place.
	fetchSeq, partSeq int
	// renderRootMade, when a test sets it, is called with each render
	// root as soon as it exists: what a declared command racing an export
	// could do to it.
	renderRootMade func(dir string)
	// bg is the programs run_command started in the background
	// (background.go).
	bg background
	// afterCheck, when a test sets it, is called with the name of a root
	// ("workspace", "scratch", "reports", "inputs" or "render") between
	// the check of a name and its use: what a declared command could do to
	// the folder in that gap.
	afterCheck func(root string)
}

// checked marks the gap between the check of a name beneath root and its
// use (see afterCheck).
func (s *server) checked(root string) {
	if s.afterCheck != nil {
		s.afterCheck(root)
	}
}

func newServer(spec *Spec, out, logw io.Writer) (*server, error) {
	if logw == nil {
		logw = io.Discard
	}
	s := &server{
		spec:  spec,
		out:   out,
		logw:  logw,
		tools: map[string]toolDef{},
		shims: map[string]string{},
	}
	if len(spec.Exec.Commands) > 0 || spec.Exec.Any {
		s.confine = newConfinement(spec)
		_, _ = fmt.Fprintln(logw, "agentry-runner tools: run_command: "+s.confine.describe())
	}
	if spec.Exec.Any {
		s.makeExecHome()
		s.writeGitIdentities()
	}
	if spec.PDF.Enabled {
		s.typst, s.render, s.pdfTimeout = defaultTypst(), newRenderConfinement(), PDFRenderTimeout
		_, _ = fmt.Fprintln(logw, "agentry-runner tools: export_pdf: "+s.render.describe())
	}
	if spec.Search.Enabled {
		s.rgBin, s.search, s.searchTimeout = ripgrepPath(), newSearchConfinement(), SearchTimeout
		_, _ = fmt.Fprintln(logw, "agentry-runner tools: grep and glob: "+s.search.describe())
	}
	s.buildTools()
	if err := s.buildShims(); err != nil {
		return nil, err
	}
	return s, nil
}

// Serve runs the MCP loop until in closes (the gateway closing stdin is
// the shutdown signal — no other lifecycle exists for a per-run server).
// Diagnostics go to logw (stderr in the container); out carries protocol
// lines only, because anything else on stdout corrupts the stream.
func Serve(in io.Reader, out io.Writer, logw io.Writer, spec *Spec) error {
	if err := validateSpec(spec); err != nil {
		return err
	}
	// Before any tool can run: a catalogued exec command must not be able
	// to read the granted secrets out of /proc/<this pid>/environ.
	// Fail closed when exec is granted: the
	// protection is what makes exec and secrets safe to grant together.
	if err := hideEnvironment(); err != nil {
		if len(spec.Exec.Commands) > 0 || spec.Exec.Any {
			return fmt.Errorf("hide the tools server's environment from exec children: %w", err)
		}
		_, _ = fmt.Fprintf(logw, "agentry-runner tools: could not mark the process non-dumpable: %v\n", err)
	}
	s, err := newServer(spec, out, logw)
	if err != nil {
		return err
	}
	s.startCalls()
	// A program left running in the background ends with the server.
	defer s.stopAllBackground()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		s.handleLine(line)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read mcp stream: %w", err)
	}
	return nil
}

// validatePorts refuses published ports but under Any, and ports that are
// not each a distinct name and a distinct number from 1024 to 65535.
func validatePorts(x Exec) error {
	if len(x.Ports) == 0 {
		return nil
	}
	if !x.Any {
		return errors.New("exec.ports are the Coding agent's, under exec.any alone")
	}
	names, numbers := map[string]bool{}, map[int]bool{}
	for _, p := range x.Ports {
		if p.Name == "" || p.Port < 1024 || p.Port > 65535 || names[p.Name] || numbers[p.Port] {
			return fmt.Errorf("exec.ports: %q on %d is not a distinct name on a distinct port from 1024 to 65535", p.Name, p.Port)
		}
		names[p.Name], numbers[p.Port] = true, true
	}
	return nil
}

// validateSpec refuses configurations the compiler should never have
// produced. Failing before the first request means a bad spec surfaces as
// a dead server the gateway health-check catches, not as tools that
// half-work against wrong roots.
func validateSpec(spec *Spec) error {
	if spec == nil {
		return errors.New("nil tool spec")
	}
	if spec.SpecVersion != SpecVersion {
		return fmt.Errorf("tool spec version %q not supported (this server implements %q — rebuild the runner image or recompile the plan)",
			spec.SpecVersion, SpecVersion)
	}
	if spec.WorkspaceDir == "" || !filepath.IsAbs(spec.WorkspaceDir) {
		return fmt.Errorf("workspaceDir %q must be a non-empty absolute container path", spec.WorkspaceDir)
	}
	if spec.OutDir == "" || !filepath.IsAbs(spec.OutDir) {
		return fmt.Errorf("outDir %q must be a non-empty absolute container path", spec.OutDir)
	}
	if spec.InputsDir != "" && !filepath.IsAbs(spec.InputsDir) {
		return fmt.Errorf("inputsDir %q must be an absolute container path when set", spec.InputsDir)
	}
	if spec.InputsTextDir != "" && (!filepath.IsAbs(spec.InputsTextDir) || spec.InputsDir == "") {
		return fmt.Errorf("inputsTextDir %q must be an absolute container path, and only beside the inputs channel", spec.InputsTextDir)
	}
	if sc := spec.ShareChecks; sc != nil {
		// The checked folder is one of the read grant's own folders, named
		// plainly, and its verdicts are a file by absolute path.
		if sc.Folder == "" || path.Clean(sc.Folder) != sc.Folder || strings.Contains(sc.Folder, "/") || sc.Folder == "." || sc.Folder == ".." ||
			!spec.FSRead.Enabled || !slices.Contains(spec.FSRead.Paths, sc.Folder) || !filepath.IsAbs(sc.File) {
			return fmt.Errorf("shareChecks %+v must name one of the read grant's folders and an absolute verdicts file", *sc)
		}
	}
	if x := spec.Exec; x.Any && (len(x.Commands) > 0 || !x.ImagePath || !filepath.IsAbs(spec.WorkspaceDir) ||
		(x.Home != "" && (!filepath.IsAbs(x.Home) || filepath.Clean(x.Home) != x.Home || !strings.HasPrefix(x.Home, spec.WorkspaceDir+"/")))) {
		// Any program runs with the image's PATH and no allowlist, so it
		// has no declared commands beside it, and its HOME is a folder
		// beneath the workspace the confinement lets it write.
		return errors.New("exec.any takes no declared commands, runs with the image's PATH and keeps HOME beneath the workspace")
	}
	if err := validatePorts(spec.Exec); err != nil {
		return err
	}
	if spec.Web.Enabled && (spec.Fetch.Enabled || len(spec.Exec.Commands) > 0 || spec.Exec.Any) {
		// The web tools' container has no network allowlist of its own
		// (web.go): a fetch grant or a declared command beside them would
		// reach whatever it liked, bounded by nothing of ours.
		return errors.New("the web tools are never granted beside fetch or exec")
	}
	if spec.Review.Enabled && (spec.Fetch.Enabled || len(spec.Exec.Commands) > 0 || spec.Exec.Any || spec.Web.Enabled || spec.InputsDir == "") {
		// The review's container reaches TypeSafe alone (review.go): a
		// fetch grant, a command or the web tools beside it would reach
		// further, and without the inputs channel it has nothing to read.
		return errors.New("the review grant is never granted beside fetch, exec or the web tools, nor without the inputs channel")
	}
	if err := validateKnowledge(spec); err != nil {
		return err
	}
	if err := validatePDF(spec); err != nil {
		return err
	}
	if err := validateDeliverables(spec); err != nil {
		return err
	}
	if err := validateCalls(spec); err != nil {
		return err
	}
	if err := validateFetchLedger(spec); err != nil {
		return err
	}
	if err := validateVoice(spec); err != nil {
		return err
	}
	return validateCalendar(spec)
}

// handleLine dispatches one message. Malformed traffic never kills the
// loop: a broken line with an id gets a JSON-RPC error, one without an id
// gets a log line — the run should die on the agent's terms, not because
// one message was garbage.
func (s *server) handleLine(line []byte) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		// Unparseable JSON carries no extractable id to address an error
		// to, so log and keep serving.
		s.logf("dropping unparseable line: %v", err)
		return
	}
	if req.Method == "" {
		if len(req.ID) != 0 {
			s.writeError(req.ID, codeInvalidRequest, "invalid request: missing method")
		} else {
			s.logf("dropping message with neither method nor id")
		}
		return
	}
	if len(req.ID) == 0 {
		// A notification never gets a response (JSON-RPC 2.0); it also
		// never triggers side effects here — the known one is a no-op and
		// an unknown one is not silently executed work.
		if req.Method != "notifications/initialized" {
			s.logf("ignoring notification %q", req.Method)
		}
		return
	}
	switch req.Method {
	case "initialize":
		s.writeResult(req.ID, s.initializeResult(req.Params))
	case "ping":
		s.writeResult(req.ID, map[string]any{})
	case "tools/list":
		s.writeResult(req.ID, s.toolsListResult())
	case "tools/call":
		s.handleToolsCall(req)
	default:
		s.writeError(req.ID, codeMethodNotFound, "method not found: "+req.Method)
	}
}

func (s *server) initializeResult(params json.RawMessage) map[string]any {
	pv := defaultProtocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err == nil && p.ProtocolVersion != "" {
			pv = p.ProtocolVersion
		}
	}
	return map[string]any{
		"protocolVersion": pv,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
	}
}

// toolsListResult advertises exactly the granted tools in registration
// order, the whole tool surface: what is not here does not exist
// as far as the model can tell.
func (s *server) toolsListResult() map[string]any {
	tools := make([]map[string]any, 0, len(s.order))
	for _, name := range s.order {
		d := s.tools[name]
		tools = append(tools, map[string]any{
			"name":        d.name,
			"description": d.description,
			"inputSchema": d.schema,
		})
	}
	return map[string]any{"tools": tools}
}

func (s *server) handleToolsCall(req rpcRequest) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
		s.writeError(req.ID, codeInvalidParams, "tools/call params must include a tool name")
		return
	}
	d, ok := s.tools[p.Name]
	if !ok {
		// An ungranted-but-known tool answers identically to a
		// nonexistent one, on purpose: refusal would confirm the tool
		// exists behind the grant.
		s.writeError(req.ID, codeInvalidParams, "unknown tool: "+p.Name)
		return
	}
	// A panicking tool must cost one errored call, not the server — the
	// gateway treats a dead server as a dead run (exit 13).
	defer func() {
		if r := recover(); r != nil {
			s.logf("tool %s panicked: %v", p.Name, r)
			s.recordCall(p.Name, false)
			s.writeError(req.ID, codeInternal, fmt.Sprintf("tool %s failed internally", p.Name))
		}
	}()
	s.callPersonsInput = false
	text, isErr := s.callTool(d, p.Arguments)
	content := map[string]any{"type": "text", "text": text}
	if (d.personsInput || s.callPersonsInput) && !isErr && !s.callStubbed {
		content = inputResult(text)
	}
	s.writeResult(req.ID, map[string]any{
		"content": []map[string]any{content},
		"isError": isErr,
	})
}

// callTool runs one granted tool and records the call. Every result fits
// the harness's limit (page.go): the tools that page already do, and this
// holds the rest, errors included. The person's stored secrets are masked
// in every result first, whichever tool read them back: a program can leave
// one in a file another tool reads.
func (s *server) callTool(d toolDef, args json.RawMessage) (string, bool) {
	s.callFault, s.callStubbed = false, false
	text, isErr := d.call(args)
	s.recordCall(d.name, !isErr)
	text = s.spec.Limits.budget().fitResult(d.name, s.scrubStoredSecrets(text))
	if !isErr {
		text, s.callStubbed = s.reread(d.name, args, text)
	}
	return text, isErr
}

// InputResourceURI names the embedded resource an input tool's answer
// travels in. The answer itself says which file and page it is.
const InputResourceURI = "agentry://inputs"

// inputResult carries a successful answer of an input tool (read_input,
// search_inputs) as an embedded text resource instead of a text item.
//
// The person's inputs reach the agent unchanged, credentials included:
// a review of a Dockerfile must be able to read
// the key it is looking for. The gateway's --block-secrets (pinned on)
// refuses any tool result whose text items look like a
// secret, with no exemption by server or tool, and it reads only the text
// items of a result (pkg/interceptors/block_secrets.go in the pinned
// v0.44.3). An embedded resource is past it, and the harness hands its
// text to the model as a text item's (docker-agent's processMCPContent).
// Every other tool's result, an input tool's error, and the arguments of
// every call (a report quoting a found key included) stay scanned.
func inputResult(text string) map[string]any {
	return map[string]any{
		"type": "resource",
		"resource": map[string]any{
			"uri":      InputResourceURI,
			"mimeType": "text/plain",
			"text":     text,
		},
	}
}

func (s *server) writeResult(id json.RawMessage, result any) {
	s.writeMsg(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *server) writeError(id json.RawMessage, code int, msg string) {
	s.writeMsg(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

// writeMsg emits one response as one line. The mutex is insurance for a
// future that handles calls concurrently: two interleaved half-lines
// would corrupt the whole stream, so the invariant is enforced where the
// bytes leave, not where the calls are scheduled.
func (s *server) writeMsg(resp rpcResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		s.logf("marshal response: %v", err)
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if _, err := s.out.Write(append(b, '\n')); err != nil {
		s.logf("write response: %v", err)
	}
}

func (s *server) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(s.logw, "toolsrv: "+format+"\n", args...)
}
