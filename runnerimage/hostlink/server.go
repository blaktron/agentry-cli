package hostlink

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// The MCP wire loop: newline-delimited JSON-RPC 2.0 on stdio, the framing
// the gateway speaks to the servers it launches, and the subset a tools
// server needs (initialize, ping, tools/list, tools/call), as the tools
// server's (toolsrv/mcp.go). Calls are handled one at a time.

const (
	maxLineBytes           = 4 << 20
	defaultProtocolVersion = "2025-06-18"
	serverVersion          = "1"

	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// startCheckTimeout bounds the sign-in check each target host gets before
// the host link serves anything.
const startCheckTimeout = 60 * time.Second

// RefusedLine is the start failure's one line on stderr, which the gateway
// relays into its log ("- agentry-hostlink: …") and the engine reads to
// say why the run stopped: the host's id and one of the Refused* details.
func RefusedLine(id, detail, why string) string {
	return fmt.Sprintf("target host %s refused (%s): %s", id, detail, why)
}

// The start check's refusals, as the receipt names them (host-refused).
const (
	RefusedAuth    = "host-auth"
	RefusedHostKey = "host-key-changed"
	RefusedReach   = "host-unreachable"
)

// Runner runs one program to its end: the ssh and sftp the host link
// calls, or a test's stand-in.
type Runner func(ctx context.Context, argv, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)

// Config is where a host link finds its files, and how it runs ssh.
type Config struct {
	SpecPath, HostsDir, LogDir, FilesDir, TmpDir string
	Run                                          Runner
	// Lookup finds the relay of a target host's address (relayLookup).
	Lookup func(address string) (string, error)
	Now    func() time.Time
	// Shell starts a shell session's ssh (startTerminal); ShellIdle and
	// ShellSettle are a session's idle time and the quiet that ends a
	// wait for its output (defaultShellIdle, defaultSettle).
	Shell                  func(argv, env []string) (Terminal, error)
	ShellIdle, ShellSettle time.Duration
}

// DefaultConfig is the host link as its catalog entry mounts it.
func DefaultConfig() Config {
	return Config{
		SpecPath: SpecMount, HostsDir: HostsMount, LogDir: LogMount, FilesDir: FilesMount,
		TmpDir: os.TempDir(), Run: execRunner, Lookup: relayLookup, Now: time.Now, Shell: startTerminal,
	}
}

// host is one target host, ready: its target, its folder, its relay, its
// sudo password, and its log.
type host struct {
	t     Target
	dir   string
	relay string
	sudo  string
	log   *os.File
}

type server struct {
	cfg    Config
	spec   *Spec
	hosts  map[string]*host
	order  []string
	limits toolsrv.Limits
	scrub  *scrubber
	out    io.Writer
	outMu  sync.Mutex
	logw   io.Writer
	// outputs keeps host_run's last outputs for host_run's continue.
	outputs map[int][]byte
	calls   int
	shells  *shells
}

// maxKeptOutputs is how many host_run outputs are kept for paging.
const maxKeptOutputs = 16

// Serve readies every target host, checks that each takes the run's
// credential, then serves the MCP loop until in closes. A host that will
// not take its credential, or whose key is not the one checked, ends the
// server before it serves: the run is not the one the person started
// (RefusedLine says why on logw).
func Serve(ctx context.Context, in io.Reader, out, logw io.Writer, cfg Config) error {
	s, err := newServer(cfg, out, logw)
	if err != nil {
		return err
	}
	defer s.close()
	if err := s.checkHosts(ctx); err != nil {
		return err
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	for sc.Scan() {
		if line := bytes.TrimSpace(sc.Bytes()); len(line) > 0 {
			s.handleLine(ctx, line)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read mcp stream: %w", err)
	}
	return nil
}

func newServer(cfg Config, out, logw io.Writer) (*server, error) {
	if logw == nil {
		logw = io.Discard
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	b, err := os.ReadFile(cfg.SpecPath)
	if err != nil {
		return nil, fmt.Errorf("read the host link spec: %w", err)
	}
	spec, err := ParseSpec(b)
	if err != nil {
		return nil, err
	}
	s := &server{
		cfg: cfg, spec: spec, hosts: map[string]*host{}, out: out, logw: logw,
		limits:  toolsrv.Limits{MaxResultBytes: spec.MaxResultBytes, MaxResultLines: spec.MaxResultLines},
		outputs: map[int][]byte{},
		shells:  newShells(cfg),
	}
	var creds []string
	for _, id := range spec.Hosts {
		h, c, err := loadHost(cfg, id)
		if err != nil {
			s.close()
			return nil, err
		}
		s.hosts[id] = h
		s.order = append(s.order, id)
		creds = append(creds, c...)
	}
	s.scrub = newScrubber(creds...)
	return s, nil
}

// loadHost reads a target host's folder: target.json, and the credentials
// it names, which are returned for the scrub and never kept otherwise but
// for the sudo password, which the host link types itself.
func loadHost(cfg Config, id string) (*host, []string, error) {
	dir := filepath.Join(cfg.HostsDir, id)
	b, err := os.ReadFile(filepath.Join(dir, TargetFile))
	if err != nil {
		return nil, nil, fmt.Errorf("target host %s: %w", id, err)
	}
	t, err := ParseTarget(b)
	if err != nil {
		return nil, nil, fmt.Errorf("target host %s: %w", id, err)
	}
	if t.ID != id {
		return nil, nil, fmt.Errorf("target host %s: its target.json names %q", id, t.ID)
	}
	var creds []string
	read := func(name string) (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("target host %s: %w", id, err)
		}
		creds = append(creds, string(b))
		return string(b), nil
	}
	switch t.Auth {
	case AuthKey:
		if _, err := read(KeyFile); err != nil {
			return nil, nil, err
		}
		if t.Passphrase {
			if _, err := read(PassphraseFile); err != nil {
				return nil, nil, err
			}
		}
	case AuthPassword:
		if _, err := read(PasswordFile); err != nil {
			return nil, nil, err
		}
	}
	h := &host{t: t, dir: path.Join(cfg.HostsDir, id)}
	if t.Sudo {
		if h.sudo, err = read(SudoFile); err != nil {
			return nil, nil, err
		}
	}
	if cfg.LogDir != "" {
		f, err := os.OpenFile(filepath.Join(cfg.LogDir, LogFile(id)), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("target host %s: its log: %w", id, err)
		}
		h.log = f
	}
	return h, creds, nil
}

func (s *server) close() {
	if s.shells != nil {
		s.shells.closeAll()
	}
	for _, h := range s.hosts {
		if h.log != nil {
			_ = h.log.Close()
		}
	}
}

// checkHosts finds each host's relay and signs in once with a command that
// does nothing, so that a credential the host refuses, or a key that is not
// the one the engine checked, stops the run before the agent starts.
func (s *server) checkHosts(ctx context.Context) error {
	for _, id := range s.order {
		h := s.hosts[id]
		relay, err := s.cfg.Lookup(h.t.Address)
		if err != nil {
			s.refuse(id, RefusedReach, err.Error())
			return fmt.Errorf("target host %s: %w", id, err)
		}
		h.relay = relay
		cctx, cancel := context.WithTimeout(ctx, startCheckTimeout)
		var stderr bytes.Buffer
		code, err := s.cfg.Run(cctx, sshArgv(h.t, h.dir, relay, "true", false), sshEnv(h.t, h.dir), nil, io.Discard, &stderr)
		cancel()
		if err == nil && code == 0 {
			s.logf("target host %s: signed in as %s through its relay", id, h.t.User)
			continue
		}
		detail, why := startRefusal(stderr.String(), code, err)
		// ssh names the relay it dialled; the person knows the host.
		why = strings.ReplaceAll(why, relay, h.t.Named)
		s.refuse(id, detail, s.scrub.String(why))
		return fmt.Errorf("target host %s: %s", id, detail)
	}
	return nil
}

func (s *server) refuse(id, detail, why string) {
	_, _ = fmt.Fprintln(s.logw, RefusedLine(id, detail, oneLine(why)))
}

// startRefusal reads why the start check's ssh failed, by OpenSSH's own
// words.
func startRefusal(stderr string, code int, err error) (string, string) {
	why := strings.TrimSpace(stderr)
	switch {
	case strings.Contains(stderr, "Permission denied"), strings.Contains(stderr, "Too many authentication failures"):
		return RefusedAuth, "the host did not accept the sign-in: " + why
	case strings.Contains(stderr, "HOST IDENTIFICATION HAS CHANGED"), strings.Contains(stderr, "Host key verification failed"),
		strings.Contains(stderr, "host key is known"), strings.Contains(stderr, "no matching host key type"):
		return RefusedHostKey, "the host's key is not the one checked at the run's start: " + why
	}
	if err != nil {
		why = err.Error() + " " + why
	}
	return RefusedReach, fmt.Sprintf("the host could not be reached (ssh exit %d): %s", code, why)
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}

func (s *server) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(s.logw, "agentry-hostlink: "+format+"\n", args...)
}

// writeLog appends one entry to the host's log, masked of the run's host
// credentials (the engine masks it again with the run's redactor).
func (s *server) writeLog(h *host, e LogEntry) {
	if h.log == nil {
		return
	}
	e.Time = s.cfg.Now().UTC().Format(time.RFC3339)
	e.Command, e.Path, e.To, e.Error = s.scrub.String(e.Command), s.scrub.String(e.Path), s.scrub.String(e.To), s.scrub.String(e.Error)
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	_, _ = h.log.Write(append(b, '\n'))
}

// --- the wire ---

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

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

func (s *server) handleLine(ctx context.Context, line []byte) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.logf("dropping an unparseable line: %v", err)
		return
	}
	if req.Method == "" {
		if len(req.ID) != 0 {
			s.writeError(req.ID, codeInvalidRequest, "invalid request: missing method")
		}
		return
	}
	if len(req.ID) == 0 {
		return
	}
	switch req.Method {
	case "initialize":
		pv := defaultProtocolVersion
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &p) == nil && p.ProtocolVersion != "" {
			pv = p.ProtocolVersion
		}
		s.writeResult(req.ID, map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": ServerName, "version": serverVersion},
		})
	case "ping":
		s.writeResult(req.ID, map[string]any{})
	case "tools/list":
		s.writeResult(req.ID, map[string]any{"tools": s.toolList()})
	case "tools/call":
		s.handleCall(ctx, req)
	default:
		s.writeError(req.ID, codeMethodNotFound, "method not found: "+req.Method)
	}
}

func (s *server) handleCall(ctx context.Context, req rpcRequest) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
		s.writeError(req.ID, codeInvalidParams, "tools/call params must include a tool name")
		return
	}
	call, ok := s.tool(p.Name)
	if !ok {
		s.writeError(req.ID, codeInvalidParams, "unknown tool: "+p.Name)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.logf("tool %s panicked: %v", p.Name, r)
			s.writeError(req.ID, codeInternal, fmt.Sprintf("tool %s failed internally", p.Name))
		}
	}()
	text, isErr := call(ctx, p.Arguments)
	text = s.limits.FitResult(p.Name, s.scrub.String(text))
	// A shell tool's error can carry what the host printed (a shell that
	// ended as it opened, the last output of one that is over), so every
	// shell answer travels as the resource (D9), its isError kept.
	asResource := !isErr || slices.Contains(shellTools, p.Name)
	s.writeResult(req.ID, map[string]any{
		"content": []map[string]any{content(text, !asResource)},
		"isError": isErr,
	})
}

// ResourceURI names the embedded resource a host tool's answer travels in.
const ResourceURI = "agentry://hosts"

// content is a host tool's answer. A successful one travels as an embedded
// text resource, which the gateway's --block-secrets does not read and the
// harness hands the model as text (DR-78's way): what the host prints
// reaches the agent unchanged (plan runtime-hosts.md D9), the run's own
// host credentials alone masked (D19). An error, which is the host link's
// own words, stays a text item the gateway scans.
func content(text string, isErr bool) map[string]any {
	if isErr {
		return map[string]any{"type": "text", "text": text}
	}
	return map[string]any{"type": "resource", "resource": map[string]any{
		"uri": ResourceURI, "mimeType": "text/plain", "text": text,
	}}
}

func (s *server) writeResult(id json.RawMessage, result any) {
	s.writeMsg(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *server) writeError(id json.RawMessage, code int, msg string) {
	s.writeMsg(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *server) writeMsg(resp rpcResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		s.logf("marshal a response: %v", err)
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if _, err := s.out.Write(append(b, '\n')); err != nil {
		s.logf("write a response: %v", err)
	}
}

func exitCode(cmd *exec.Cmd, err error) (int, error) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return cmd.ProcessState.ExitCode(), nil
}
