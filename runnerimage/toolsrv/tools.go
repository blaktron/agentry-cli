// The six §R3 tool implementations and their JSON Schemas. Everything the
// model can do to the workspace, the network, or a process goes through
// one of these functions, so every enforcement promise the run plan makes
// (paths grants, host allowlists, the exec shim, byte caps) has its
// server-side half here — the container-level mounts and --block-network
// are the other half, and the two must agree rather than trust each other.
//
// Design rule for error returns: a tool failure is an isError=true text
// the MODEL reads and can react to, never a protocol error — with one
// deliberate exception: a non-zero exit code from run_command is a normal
// result, because an agent that cannot see failing exit codes cannot do
// its job.

package toolsrv

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits accessors: zero values in the compiled spec mean "package
// default" (spec.go), so the compiler never has to hardcode numbers this
// server also knows.

func (l Limits) readCap() int64 {
	if l.MaxReadBytes > 0 {
		return l.MaxReadBytes
	}
	return DefaultMaxReadBytes
}

func (l Limits) writeCap() int64 {
	if l.MaxWriteBytes > 0 {
		return l.MaxWriteBytes
	}
	return DefaultMaxWriteBytes
}

func (l Limits) fetchCap() int64 {
	if l.MaxFetchBytes > 0 {
		return l.MaxFetchBytes
	}
	return DefaultMaxFetchBytes
}

func (l Limits) execOutCap() int64 {
	if l.MaxExecOutputBytes > 0 {
		return l.MaxExecOutputBytes
	}
	return DefaultMaxExecOutputBytes
}

func (l Limits) execTimeoutSec() int {
	if l.ExecTimeoutSeconds > 0 {
		return l.ExecTimeoutSeconds
	}
	return DefaultExecTimeoutSeconds
}

// toolDef is one registered tool: what tools/list advertises plus the
// function tools/call dispatches to.
type toolDef struct {
	name        string
	description string
	schema      map[string]any
	call        func(args json.RawMessage) (text string, isError bool)
}

// buildTools registers exactly the granted subset, iterating
// GrantedTools() so the registration order — and therefore tools/list
// order — is the one the compiler and the gateway allowlist share
// (RUNNER-PLAN §R11 invariant 4: an undeclared capability's tool is
// absent, not refusing).
func (s *server) buildTools() {
	add := func(name, desc string, schema map[string]any, call func(json.RawMessage) (string, bool)) {
		s.order = append(s.order, name)
		s.tools[name] = toolDef{name: name, description: desc, schema: schema, call: call}
	}
	for _, name := range s.spec.GrantedTools() {
		switch name {
		case ToolFSRead:
			add(name, "Read a file from the agent workspace. Returns the file text, capped by the server's byte limit; use offset and maxBytes to page through larger files.",
				fsReadSchema(), s.toolFSRead)
		case ToolFSList:
			add(name, "List one directory level of the agent workspace: directories as \"name/\", files as \"name<TAB>size\". Omit path (or pass \".\") for the workspace root.",
				fsListSchema(), s.toolFSList)
		case ToolFSWrite:
			add(name, "Write a file in the agent workspace, creating parent directories. Content larger than the server's byte limit is refused — split large writes.",
				fsWriteSchema(), s.toolFSWrite)
		case ToolFetch:
			add(name, "Make an HTTP(S) request to an allowlisted host. Credentials go via the auth parameter, which injects a granted secret server-side; Authorization/Cookie headers are refused.",
				fetchSchema(), s.toolFetch)
		case ToolRunCommand:
			add(name, s.runCommandDescription(), runCommandSchema(), s.toolRunCommand)
		case ToolListInputs:
			add(name, "List the read-only input files provided for this run, one a line: its path, the type of file it is, its size in bytes, whether it could be read (read, or unread with the reason), the size of its text and the views read_input can read it in. A document, PDF or image was turned into text before the run; code, markup and data are as uploaded. Omit path (or pass \".\") for every input, or pass a directory for what is under it; empty when no inputs were provided.",
				listInputsSchema(), s.toolListInputs)
		case ToolReadInput:
			add(name, "Read one of the run's provided input files (read-only). By default a document, PDF or image is read as the text decoded from it before the run, and any other file as it was uploaded. view \"original\" reads a document as uploaded too (a binary file as a page of base64, with its size and type); view \"text\" reads HTML as Markdown. Capped by the server's byte limit; use offset and maxBytes to page through larger files.",
				readInputSchema(), s.toolReadInput)
		case ToolSearchInputs:
			add(name, "Search the run's input files for a word or phrase, without regard to case, each in its default view unless view says which (a document's text, other files as uploaded). Returns each matching line as path:line: excerpt; read the file with read_input for more.",
				searchInputsSchema(), s.toolSearchInputs)
		case ToolAIReview:
			add(name, aiReviewDescription, aiReviewSchema(), s.toolAIReview)
		case ToolKnowledgeList:
			add(name, s.knowledgeListDescription(), knowledgeListSchema(), s.toolKnowledgeList)
		case ToolKnowledgeSearch:
			add(name, "Search an attached knowledge pack for a word or phrase over its pages' titles, summaries and text (or its source documents). Returns the matching pages, best first, each with the line that matched.",
				knowledgeSearchSchema(), s.toolKnowledgeSearch)
		case ToolKnowledgeRead:
			add(name, "Read a page of an attached knowledge pack (read-only), by the path its index gives, or a source document by a citation's doc:<path>. Capped by the server's byte limit; use offset and maxBytes to page through.",
				knowledgeReadSchema(), s.toolKnowledgeRead)
		case ToolVoiceRead:
			add(name, voiceReadDescription, objSchema(map[string]any{}), s.toolVoiceRead)
		case ToolWebSearch:
			add(name, "Search the web. AI Review reads every result before you see it; a result it flagged, or could not scan, is shown as withheld with its origin only.",
				webSearchSchema(), s.toolWebSearch)
		case ToolWebRead:
			desc := "Read a public web page as text (GET only; at most 32 KiB of text). AI Review reads the page before you do; a page it flagged, or could not scan, is withheld and you get only its address and the reason."
			if s.spec.Web.SearchedOnly {
				desc += " It reads only an address one of your web_search results gave, exactly as given."
			}
			add(name, desc, webReadSchema(), s.toolWebRead)
		case ToolRegistrySearch:
			add(name, "Ask the Agentry registry: search its published QuickAgents (query), read one QuickAgent's manifest (ref), or read its vulnerability check of an exact MCP server version (ecosystem, package, version). AI Review reads each answer before you do.",
				registrySearchSchema(), s.toolRegistrySearch)
		case ToolWriteReport:
			add(name, "Write a report file under the run's output folder (reports/). This is the only file channel that survives the run; set append to extend an existing report.",
				writeReportSchema(), s.toolWriteReport)
		case ToolEndBlocked:
			add(name, endBlockedDescription, endBlockedSchema(), s.toolEndBlocked)
		}
	}
}

// Schema helpers: MCP inputSchema is plain JSON Schema; building the maps
// through these keeps the tool schemas below readable at a glance.

func objSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// viewProp is the input tools' choice of view (agentry-cli#631).
func viewProp() map[string]any {
	return map[string]any{"type": "string", "enum": []string{ViewText, ViewOriginal},
		"description": "which view of the file: \"text\" (a document's decoded text, HTML as Markdown) or \"original\" (the file as uploaded, a binary one as base64); default: a document's text, anything else as uploaded. list_inputs says which views each file has"}
}

func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func fsReadSchema() map[string]any {
	return objSchema(map[string]any{
		"path":     strProp("workspace-relative slash path of the file"),
		"offset":   intProp("byte offset to start reading from (default 0)"),
		"maxBytes": intProp("cap on returned bytes; the server limit still applies"),
		"view":     viewProp(),
	}, "path")
}

func fsListSchema() map[string]any {
	return objSchema(map[string]any{
		"path": strProp("workspace-relative directory to list; empty or \".\" is the workspace root"),
	})
}

func fsWriteSchema() map[string]any {
	return objSchema(map[string]any{
		"path":    strProp("workspace-relative slash path of the file to write"),
		"content": strProp("full file content (text)"),
	}, "path", "content")
}

func fetchSchema() map[string]any {
	return objSchema(map[string]any{
		"url":    strProp("http(s) URL on an allowlisted host"),
		"method": strProp("HTTP method (GET, HEAD, POST, PUT, PATCH, DELETE); default GET"),
		"headers": map[string]any{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "string"},
			"description":          "extra request headers; Authorization, Proxy-Authorization, and Cookie are refused — use auth",
		},
		"body": strProp("request body"),
		"auth": map[string]any{
			"type":        "object",
			"description": "inject a granted secret as an auth header; the secret value never enters model context",
			"properties": map[string]any{
				"env":    strProp("name of a granted secret environment variable"),
				"scheme": map[string]any{"type": "string", "enum": []string{"bearer", "header"}, "description": "bearer (default) sets Authorization: Bearer <value>; header sets the named header to the raw value"},
				"header": strProp("header name to set when scheme is \"header\""),
			},
			"required": []string{"env"},
		},
	}, "url")
}

func runCommandSchema() map[string]any {
	return objSchema(map[string]any{
		"command": strProp("a declared command name (bare name, no path)"),
		"args": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "command arguments",
		},
		"stdin":          strProp("text piped to the command's stdin"),
		"timeoutSeconds": intProp("kill the command after this many seconds; clamped to the server limit"),
	}, "command")
}

func listInputsSchema() map[string]any {
	return objSchema(map[string]any{
		"path": strProp("inputs-relative directory to list; empty or \".\" is the inputs root"),
	})
}

func readInputSchema() map[string]any {
	return objSchema(map[string]any{
		"path":     strProp("inputs-relative slash path of the file"),
		"offset":   intProp("byte offset to start reading from (default 0)"),
		"maxBytes": intProp("cap on returned bytes; the server limit still applies"),
	}, "path")
}

func writeReportSchema() map[string]any {
	return objSchema(map[string]any{
		"path":    strProp("report path relative to the reports/ output folder"),
		"content": strProp("report content (text)"),
		"append":  boolProp("append to the report instead of replacing it"),
	}, "path", "content")
}

// unmarshalArgs decodes tool arguments leniently: unknown fields are
// tolerated (schema drift on the client side should degrade, not break),
// absent arguments mean an empty object, and a non-string parameter that
// arrives as a JSON-encoded string — "[\"a\"]" for an array, "200" for a
// number, "true" for a bool, or the whole object as one string — is taken
// as the value it encodes, since models do send exactly that and every
// call would otherwise fail (agentry-cli#113). A string parameter is never
// reinterpreted; a string that does not encode the wanted type is refused
// naming the parameter, what arrived and what was wanted. Malformed JSON
// errors as before.
func unmarshalArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		// The whole arguments object arrived as one JSON string.
		var inner string
		if err := json.Unmarshal(raw, &inner); err != nil {
			return fmt.Errorf("invalid arguments: %w", err)
		}
		raw = []byte(inner)
	}
	err := json.Unmarshal(raw, v)
	if err == nil {
		return nil
	}
	var te *json.UnmarshalTypeError
	if !errors.As(err, &te) || te.Value != "string" {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	coerced, cerr := coerceStringFields(raw, v)
	if cerr != nil {
		return cerr
	}
	if err := json.Unmarshal(coerced, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// coerceStringFields rewrites, in the arguments object, every JSON string
// whose target struct field is not a string into the JSON it encodes.
func coerceStringFields(raw json.RawMessage, v any) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return raw, nil
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Type.Kind() == reflect.String {
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			name = f.Name
		}
		r, ok := obj[name]
		if !ok || len(r) == 0 || r[0] != '"' {
			continue
		}
		var inner string
		if err := json.Unmarshal(r, &inner); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		probe := reflect.New(f.Type).Interface()
		if !json.Valid([]byte(inner)) || json.Unmarshal([]byte(inner), probe) != nil {
			return nil, fmt.Errorf("invalid arguments: %s: got the string %q, want %s", name, inner, f.Type)
		}
		obj[name] = json.RawMessage(inner)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	return out, nil
}

// execDir is run_command's working directory: the workspace when it is
// mounted, /tmp — the one writable non-mount in the container — otherwise.
func (s *server) execDir() string {
	if fi, err := os.Stat(s.spec.WorkspaceDir); err == nil && fi.IsDir() {
		return s.spec.WorkspaceDir
	}
	return os.TempDir()
}

// containsString is the small allowlist-membership helper the exec and
// fetch gates share; exact match only, by design.
func containsString(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// ---- fs_read ----

func (s *server) toolFSRead(raw json.RawMessage) (string, bool) {
	var a struct {
		Path     string `json:"path"`
		Offset   int64  `json:"offset"`
		MaxBytes int64  `json:"maxBytes"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "fs_read: " + err.Error(), true
	}
	if a.Path == "" {
		return "fs_read: path is required", true
	}
	if a.Offset < 0 {
		return "fs_read: offset must be >= 0", true
	}
	// Registration already gates on the grant; this re-check is the
	// belt-and-braces rule spec.go states for the mount flags too.
	if !s.spec.FSRead.Enabled {
		return "fs_read: filesystem read is not granted", true
	}
	if !allowedByGrant(s.spec.FSRead.Paths, a.Path) {
		return fmt.Sprintf("fs_read: %q is outside the granted paths", path.Clean(a.Path)), true
	}
	abs, err := resolveUnder(s.spec.WorkspaceDir, a.Path)
	if err != nil {
		return "fs_read: " + err.Error(), true
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "fs_read: " + err.Error(), true
	}
	if fi.IsDir() {
		return fmt.Sprintf("fs_read: %q is a directory (use fs_list)", path.Clean(a.Path)), true
	}
	limit := s.spec.Limits.readCap()
	if a.MaxBytes > 0 && a.MaxBytes < limit {
		limit = a.MaxBytes
	}
	f, err := os.Open(abs)
	if err != nil {
		return "fs_read: " + err.Error(), true
	}
	defer func() { _ = f.Close() }()
	if a.Offset > 0 {
		if _, err := f.Seek(a.Offset, io.SeekStart); err != nil {
			return "fs_read: " + err.Error(), true
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return "fs_read: " + err.Error(), true
	}
	total := fi.Size()
	truncated := a.Offset+int64(len(data)) < total
	if truncated {
		// The cap can split a multibyte rune; a split rune must not make
		// a text file look binary, so shave at most an incomplete tail.
		data = trimPartialRune(data)
	}
	if !utf8.Valid(data) {
		// Feeding raw bytes to the model wastes context and invites
		// mojibake loops; say what the file is instead.
		return fmt.Sprintf("binary file; %d bytes", total), true
	}
	text := string(data)
	if truncated {
		text += fmt.Sprintf("\n[truncated at %d bytes of %d]", len(data), total)
	}
	return text, false
}

// trimPartialRune drops at most utf8.UTFMax-1 trailing bytes when they are
// what stands between the buffer and UTF-8 validity — the signature of a
// rune split by a byte cap, not of a binary file.
func trimPartialRune(b []byte) []byte {
	for i := 0; i < utf8.UTFMax-1 && len(b) > 0; i++ {
		if utf8.Valid(b) {
			return b
		}
		b = b[:len(b)-1]
	}
	return b
}

// ---- fs_list ----

func (s *server) toolFSList(raw json.RawMessage) (string, bool) {
	var a struct {
		Path string `json:"path"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "fs_list: " + err.Error(), true
	}
	if !s.spec.FSRead.Enabled {
		return "fs_list: filesystem read is not granted", true
	}
	rel := a.Path
	if rel == "" {
		rel = "."
	}
	rel = path.Clean(rel)
	if rel == "." && len(s.spec.FSRead.Paths) > 0 {
		// Paths mode: the workspace root itself is not granted, but the
		// model still needs a way to discover what is — so listing the
		// root lists the grant entries themselves (§R3).
		var b strings.Builder
		for i, entry := range s.spec.FSRead.Paths {
			if i > 0 {
				b.WriteByte('\n')
			}
			e := path.Clean(entry)
			line := e
			if abs, err := resolveUnder(s.spec.WorkspaceDir, e); err == nil {
				if fi, err := os.Stat(abs); err == nil {
					if fi.IsDir() {
						line = e + "/"
					} else {
						line = e + "\t" + strconv.FormatInt(fi.Size(), 10)
					}
				}
			}
			b.WriteString(line)
		}
		return b.String(), false
	}
	if rel != "." && !allowedByGrant(s.spec.FSRead.Paths, rel) {
		return fmt.Sprintf("fs_list: %q is outside the granted paths", rel), true
	}
	abs, err := resolveUnder(s.spec.WorkspaceDir, rel)
	if err != nil {
		return "fs_list: " + err.Error(), true
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "fs_list: " + err.Error(), true
	}
	if len(entries) == 0 {
		return "(empty directory)", false
	}
	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteByte('\n')
		}
		if e.IsDir() {
			b.WriteString(e.Name() + "/")
			continue
		}
		fi, err := e.Info()
		if err != nil {
			b.WriteString(e.Name())
			continue
		}
		b.WriteString(e.Name() + "\t" + strconv.FormatInt(fi.Size(), 10))
	}
	return b.String(), false
}

// ---- fs_write ----

func (s *server) toolFSWrite(raw json.RawMessage) (string, bool) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "fs_write: " + err.Error(), true
	}
	if a.Path == "" {
		return "fs_write: path is required", true
	}
	// The workspace mount is :ro unless this grant exists; checking here
	// too keeps the layers in agreement instead of in trust (spec.go).
	if !s.spec.FSWrite.Enabled {
		return "fs_write: filesystem write is not granted", true
	}
	if !allowedByGrant(s.spec.FSWrite.Paths, a.Path) {
		return fmt.Sprintf("fs_write: %q is outside the granted paths", path.Clean(a.Path)), true
	}
	if limit := s.spec.Limits.writeCap(); int64(len(a.Content)) > limit {
		// Refuse rather than truncate: a silently clipped file is worse
		// than an error the model can react to by splitting the write.
		return fmt.Sprintf("fs_write: content is %d bytes, over the %d-byte limit — split the write", len(a.Content), limit), true
	}
	abs, err := resolveUnder(s.spec.WorkspaceDir, a.Path)
	if err != nil {
		return "fs_write: " + err.Error(), true
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "fs_write: " + err.Error(), true
	}
	s.ownBetween(s.spec.WorkspaceDir, filepath.Dir(abs))
	if err := os.WriteFile(abs, []byte(a.Content), 0o644); err != nil {
		return "fs_write: " + err.Error(), true
	}
	s.own(abs)
	return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), path.Clean(a.Path)), false
}

// ---- fetch ----

// FetchTimeout is how long fetch waits for one request, redirects
// included, before it gives up. The Drafter's guide states it (its runtime
// facts are generated from it).
const FetchTimeout = 60 * time.Second

func (s *server) toolFetch(raw json.RawMessage) (string, bool) {
	var a struct {
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
		Auth    *struct {
			Env    string `json:"env"`
			Scheme string `json:"scheme"`
			Header string `json:"header"`
		} `json:"auth"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "fetch: " + err.Error(), true
	}
	if a.URL == "" {
		return "fetch: url is required", true
	}
	if !s.spec.Fetch.Enabled {
		return "fetch: network egress is not granted", true
	}
	u, err := url.Parse(a.URL)
	if err != nil {
		return "fetch: invalid url: " + err.Error(), true
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Sprintf("fetch: scheme %q is not allowed (http and https only)", u.Scheme), true
	}
	if !s.fetchHostAllowed(u.Hostname()) {
		return fmt.Sprintf("fetch: host %q is not in the egress allowlist", u.Hostname()), true
	}
	method := strings.ToUpper(a.Method)
	if method == "" {
		method = http.MethodGet
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return fmt.Sprintf("fetch: method %q is not allowed", method), true
	}
	var body io.Reader
	if a.Body != "" {
		body = strings.NewReader(a.Body)
	}
	req, err := http.NewRequest(method, a.URL, body)
	if err != nil {
		return "fetch: " + err.Error(), true
	}
	for k, v := range a.Headers {
		switch strings.ToLower(k) {
		case "authorization", "proxy-authorization", "cookie":
			// Credential headers only via auth: that path reads the value
			// from a granted env var, so the secret never has to pass
			// through model context to be used (§R3).
			return fmt.Sprintf("fetch: header %q is refused — pass credentials via the auth parameter", k), true
		}
		req.Header.Set(k, v)
	}
	if a.Auth != nil {
		if a.Auth.Env == "" {
			return "fetch: auth.env is required", true
		}
		if !containsString(s.spec.Fetch.SecretEnv, a.Auth.Env) {
			return fmt.Sprintf("fetch: %q is not a granted secret env", a.Auth.Env), true
		}
		v := os.Getenv(a.Auth.Env)
		if v == "" {
			return fmt.Sprintf("fetch: secret %s not set", a.Auth.Env), true
		}
		switch a.Auth.Scheme {
		case "", "bearer":
			req.Header.Set("Authorization", "Bearer "+v)
		case "header":
			if a.Auth.Header == "" {
				return "fetch: auth.header is required when auth.scheme is \"header\"", true
			}
			req.Header.Set(a.Auth.Header, v)
		default:
			return fmt.Sprintf("fetch: unknown auth.scheme %q (want bearer or header)", a.Auth.Scheme), true
		}
	}
	client := &http.Client{
		Timeout: FetchTimeout,
		// Redirects re-run the host gate hop by hop: an allowlisted host
		// must not be usable as a bounce toward one that is not. This is
		// the only place redirects are validated (spec.go Fetch comment);
		// the container-level allowHosts backstops whatever slips.
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if r.URL.Scheme != "http" && r.URL.Scheme != "https" {
				return fmt.Errorf("redirect to scheme %q refused", r.URL.Scheme)
			}
			if !s.fetchHostAllowed(r.URL.Hostname()) {
				return fmt.Errorf("redirect to host %q refused: not in the egress allowlist", r.URL.Hostname())
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return s.scrubSecrets("fetch: " + err.Error()), true
	}
	defer func() { _ = resp.Body.Close() }()
	limit := s.spec.Limits.fetchCap()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return s.scrubSecrets("fetch: read response: " + err.Error()), true
	}
	truncated := int64(len(data)) > limit
	if truncated {
		data = data[:limit]
	}
	text := fmt.Sprintf("HTTP %s\ncontent-type: %s\n\n%s", resp.Status, resp.Header.Get("Content-Type"), data)
	if truncated {
		if resp.ContentLength >= 0 {
			text += fmt.Sprintf("\n[truncated at %d bytes of %d]", limit, resp.ContentLength)
		} else {
			text += fmt.Sprintf("\n[truncated at %d bytes]", limit)
		}
	}
	// A 4xx/5xx status is a normal result — the agent must see it to
	// react; only transport and policy failures are isError above.
	return s.scrubSecrets(text), false
}

// fetchHostAllowed is the server-side half of the egress control: exact,
// case-insensitive hostname membership (or the policy-gated wildcard).
// The gateway enforces the same list at container level; neither layer
// assumes the other held.
func (s *server) fetchHostAllowed(host string) bool {
	if s.spec.Fetch.AllowAll {
		return true
	}
	for _, h := range s.spec.Fetch.Hosts {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

// scrubSecrets replaces every granted secret's value in text with a
// [redacted:ENV] marker. Cheap defense in depth: an upstream that echoes
// the credential (auth debuggers, error pages) must not teach it to the
// model. The gateway's --block-secrets does the same at its layer.
func (s *server) scrubSecrets(text string) string {
	envs := append(append([]string(nil), s.spec.Fetch.SecretEnv...), webKeysForScrub(s.spec.Web)...)
	if s.spec.Review.Enabled {
		envs = append(envs, ReviewSecretEnv()...)
	}
	for _, env := range envs {
		v := os.Getenv(env)
		if v == "" {
			continue
		}
		text = strings.ReplaceAll(text, v, "[redacted:"+env+"]")
	}
	return text
}

// ---- run_command ----

// buildShims prepares the exec PATH jail once at server start: a fresh
// temp dir holding one symlink per declared-and-present command. Children
// run with PATH set to only this dir, so even a shell among the declared
// commands can only resolve declared names (§R3; DR-1 records that the
// rest of the image's binaries still exist — argv0 + PATH is the v1
// control, not binary absence).
func (s *server) buildShims() error {
	if len(s.spec.Exec.Commands) == 0 {
		return nil
	}
	// MkdirTemp rather than a fixed name: several servers in one process
	// (tests) must not share or clobber each other's shims. The pid keeps
	// the name attributable when inspecting a live container.
	dir, err := os.MkdirTemp(os.TempDir(), fmt.Sprintf("agentry-shim-%d-", os.Getpid()))
	if err != nil {
		return fmt.Errorf("create exec shim dir: %w", err)
	}
	// A command that runs as nobody (confinement.go) must still find its
	// shim; the directory holds only links to the image's own programs.
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // traversable by the unprivileged command, written by no one
		return fmt.Errorf("open the exec shim dir to the unprivileged user: %w", err)
	}
	s.shimDir = dir
	for _, name := range s.spec.Exec.Commands {
		if name == "" || strings.ContainsAny(name, `/\`) {
			// A name with a separator can never pass the call-time gate,
			// so it gets no shim; declaring it was a manifest mistake.
			s.logf("declared command %q is not a bare name; treating as absent", name)
			continue
		}
		resolved, err := exec.LookPath(name)
		if err != nil {
			// Recorded by omission from s.shims; calling it reports
			// "not present in image" rather than a confusing exec error.
			s.logf("declared command %q not present in image: %v", name, err)
			continue
		}
		link := filepath.Join(dir, name)
		if err := os.Symlink(resolved, link); err != nil {
			return fmt.Errorf("shim %s: %w", name, err)
		}
		s.shims[name] = link
	}
	return nil
}

// execPath is a declared command's PATH: the shim directory alone, or, in
// Unsafe Mode (Exec.ImagePath), the shims first and then the image's own
// PATH.
func (s *server) execPath() string {
	if p := s.imagePath(); p != "" {
		return s.shimDir + string(os.PathListSeparator) + p
	}
	return s.shimDir
}

// imagePath is the PATH a declared command gets after the shims: the
// image's own in Unsafe Mode, which is this server's (the image's ENV;
// nothing in the run sets it), and empty otherwise. execPath and
// run_command's description both read it, so they cannot disagree.
func (s *server) imagePath() string {
	if !s.spec.Exec.ImagePath {
		return ""
	}
	return os.Getenv("PATH")
}

// runCommandDescription says the PATH execPath gives a declared command
// (agentry-cli#502): with the image's PATH (RUNNER-DEVIATIONS DR-53), the
// agent is not told a narrower posture than the one enforced.
// knowledgeListDescription names the packs attached to this run, which the
// System Prompt cannot: it is compiled before a run attaches any, and
// attaching one must not move the plan (DR-38; agentry-cli#520). The tool
// exists only while a pack is attached, so its description is where the
// model learns that one is, and that it comes first.
func (s *server) knowledgeListDescription() string {
	names := make([]string, 0, len(s.spec.Knowledge))
	for _, p := range s.spec.Knowledge {
		names = append(names, p.Name)
	}
	return "Knowledge is attached to this run: " + strings.Join(names, ", ") + ". Start here, before your own knowledge or any other tool. " +
		"Lists a knowledge pack: its index (every page, one line each, by section), or one section's pages. With several packs attached and none named, lists the packs."
}

func (s *server) runCommandDescription() string {
	const returns = " Returns \"exit code N\" plus combined output; a non-zero exit is a result, not an error."
	writes := " " + s.confine.describe()
	if s.imagePath() != "" {
		return "Run one declared command in the workspace. Only a declared command can be called; it runs with the image's own PATH after the declared commands, so it can start the programs the image holds." + writes + returns
	}
	return "Run one declared command in the workspace with a PATH restricted to the declared commands." + writes + returns
}

func (s *server) toolRunCommand(raw json.RawMessage) (string, bool) {
	var a struct {
		Command        string   `json:"command"`
		Args           []string `json:"args"`
		Stdin          string   `json:"stdin"`
		TimeoutSeconds int      `json:"timeoutSeconds"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "run_command: " + err.Error(), true
	}
	if a.Command == "" {
		return "run_command: command is required", true
	}
	if len(s.spec.Exec.Commands) == 0 {
		return "run_command: exec is not granted", true
	}
	if strings.ContainsAny(a.Command, `/\`) {
		return fmt.Sprintf("run_command: %q is not a bare command name (path separators are refused)", a.Command), true
	}
	if !containsString(s.spec.Exec.Commands, a.Command) {
		return fmt.Sprintf("run_command: %q is not in the declared exec allowlist", a.Command), true
	}
	shim, ok := s.shims[a.Command]
	if !ok {
		return fmt.Sprintf("run_command: command %q not present in image", a.Command), true
	}
	if s.confine.refused != "" {
		return "run_command: " + s.confine.refused, true
	}
	limitSec := s.spec.Limits.execTimeoutSec()
	if a.TimeoutSeconds > 0 && a.TimeoutSeconds < limitSec {
		limitSec = a.TimeoutSeconds
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(limitSec)*time.Second)
	defer cancel()
	// The command is held to the places it may write (agentry-cli#596):
	// through the confine trampoline, or as nobody (confinement.go).
	name, argv := s.confine.argv(shim, a.Args)
	cmd := exec.CommandContext(ctx, name, argv...)
	if s.confine.asNobody {
		runAs(cmd, unprivilegedID)
	}
	// The command runs in the workspace when the contract mounted one;
	// exec and filesystem are independent grants, so a QuickAgent that
	// declares exec and no filesystem access (it works on /inputs) gets a
	// scratch directory instead of a failed chdir (agentry-cli#120).
	cmd.Dir = s.execDir()
	// A minimal, explicit environment: PATH is the shim directory (with,
	// in Unsafe Mode, the image's PATH after it); HOME and LANG exist
	// because plenty of tools crash without them, and /tmp is the only
	// writable non-mount in the container.
	cmd.Env = []string{"PATH=" + s.execPath(), "HOME=/tmp", "LANG=C.UTF-8"}
	if a.Stdin != "" {
		cmd.Stdin = strings.NewReader(a.Stdin)
	}
	out := &capWriter{max: s.spec.Limits.execOutCap()}
	cmd.Stdout, cmd.Stderr = out, out
	// Without a WaitDelay, a grandchild holding the output pipe keeps
	// Run blocked past the kill; five seconds of grace, then move on.
	cmd.WaitDelay = 5 * time.Second
	failed, err := s.confine.run(cmd)
	if failed != "" {
		return "run_command: " + failed, true
	}
	// The output passes the same scrub as fetch's (agentry-cli#246): the
	// child cannot read this process's environment (hideEnvironment), but a
	// granted secret that reached it some other way must still not be
	// echoed to the model verbatim.
	text := s.scrubSecrets(out.text())
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("run_command: %q timed out after %ds\n%s", a.Command, limitSec, text), true
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return "exit code 0\n" + text, false
	case errors.As(err, &exitErr):
		// Deliberately NOT isError: the exit code is information the
		// agent acts on (§R3), not a tool malfunction.
		return fmt.Sprintf("exit code %d\n%s", exitErr.ExitCode(), text), false
	default:
		return "run_command: " + err.Error(), true
	}
}

// capWriter keeps the first max bytes and counts the rest, so a chatty
// command costs bounded memory and the note below can still say how much
// was dropped. It never returns an error: erroring the pipe would kill
// the command over its own verbosity.
type capWriter struct {
	max   int64
	buf   bytes.Buffer
	total int64
}

func (w *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.total += int64(n)
	if room := w.max - int64(w.buf.Len()); room > 0 {
		if int64(len(p)) > room {
			p = p[:room]
		}
		w.buf.Write(p)
	}
	return n, nil
}

func (w *capWriter) text() string {
	t := w.buf.String()
	if w.total > int64(w.buf.Len()) {
		t += fmt.Sprintf("\n[output truncated at %d bytes of %d]", w.buf.Len(), w.total)
	}
	return t
}

// ---- list_inputs / read_input (RUNNER-PLAN-2 §P4) ----
//
// The inputs channel is operator-provided task material, mounted :ro and
// exposed read-only here — a second read root beside the workspace, with
// no grant list because handing the files to the run WAS the grant. The
// same path safety and byte caps as the fs tools apply.

func (s *server) toolListInputs(raw json.RawMessage) (string, bool) {
	var a struct {
		Path string `json:"path"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return s.failed("list_inputs", err)
	}
	if s.spec.InputsDir == "" {
		return "list_inputs: no inputs channel configured", true
	}
	rel := a.Path
	if rel == "" {
		rel = "."
	}
	abs, err := resolveUnder(s.spec.InputsDir, path.Clean(rel))
	if err != nil {
		return s.failed("list_inputs", err)
	}
	idx, err := s.inputsIndex()
	if err != nil {
		return s.failed("list_inputs", err)
	}
	if idx != nil {
		return listIndexed(idx, path.Clean(rel))
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return s.failed("list_inputs", err)
	}
	if len(entries) == 0 {
		return "(no input files provided for this run)", false
	}
	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteByte('\n')
		}
		if e.IsDir() {
			b.WriteString(e.Name() + "/")
			continue
		}
		fi, err := e.Info()
		if err != nil {
			b.WriteString(e.Name())
			continue
		}
		b.WriteString(e.Name() + "\t" + strconv.FormatInt(fi.Size(), 10))
	}
	return b.String(), false
}

func (s *server) toolReadInput(raw json.RawMessage) (string, bool) {
	var a struct {
		Path     string `json:"path"`
		Offset   int64  `json:"offset"`
		MaxBytes int64  `json:"maxBytes"`
		View     string `json:"view"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return s.failed("read_input", err)
	}
	if a.View != "" && a.View != ViewText && a.View != ViewOriginal {
		return fmt.Sprintf("read_input: view must be %q or %q, not %q", ViewText, ViewOriginal, a.View), true
	}
	if a.Path == "" {
		return "read_input: path is required", true
	}
	if a.Offset < 0 {
		return "read_input: offset must be >= 0", true
	}
	if s.spec.InputsDir == "" {
		return "read_input: no inputs channel configured", true
	}
	abs, err := resolveUnder(s.spec.InputsDir, a.Path)
	if err != nil {
		return s.failed("read_input", err)
	}
	// With the decode step's index, an input is read in the view asked
	// for, or its default; one that could not be read says why. A path
	// the index does not name (a directory) is answered from /inputs as
	// before, as is every input without an index, where the file is both
	// its views.
	idx, err := s.inputsIndex()
	if err != nil {
		return s.failed("read_input", err)
	}
	view, mime := a.View, ""
	if idx != nil {
		if e, ok := idx.lookup(path.Clean(a.Path)); ok {
			if e.Unread != "" {
				return fmt.Sprintf("read_input: %s could not be read: %s", e.Path, e.Unread), true
			}
			if e.Unpacked {
				return fmt.Sprintf("read_input: %s is an archive; list_inputs with path %q lists what was unpacked from it, each read by its own path", e.Path, e.Path), true
			}
			if view == "" {
				view = e.defaultView()
			}
			mime = e.MIME
			if abs, err = s.viewOf(e, view); err != nil {
				return s.failed("read_input", err)
			}
		}
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return s.failed("read_input", err)
	}
	if fi.IsDir() {
		return fmt.Sprintf("read_input: %q is a directory (use list_inputs)", path.Clean(a.Path)), true
	}
	if !s.spec.Review.Enabled {
		if view == ViewOriginal && isBinary(abs) {
			// A binary original is answered, not refused (D3), every page
			// of it: the file decides, not a page that happens to decode.
			text, err := readBase64Page(abs, a.Offset, a.MaxBytes, s.spec.Limits.readCap(), mime)
			if err != nil {
				return s.failed("read_input", err)
			}
			return text, false
		}
		text, isErr, err := readPaged(abs, a.Offset, a.MaxBytes, s.spec.Limits.readCap())
		if err != nil {
			return s.failed("read_input", err)
		}
		return text, isErr
	}
	// Under the review grant a file reaches the model only after AI Review
	// passed it (review.go): the model gets a withheld file's path and the
	// reason instead, which is an answer, not a tool failure. Each view is
	// reviewed as its own text, and a binary original stays refused, since
	// no review can read it.
	if instead, isErr, ok := s.reviewedInput(path.Clean(a.Path), view, abs, fi); !ok {
		return s.scrubSecrets(instead), isErr
	}
	text, isErr, err := readPaged(abs, a.Offset, a.MaxBytes, s.spec.Limits.readCap())
	if err != nil {
		return s.failed("read_input", err)
	}
	return s.scrubSecrets(text), isErr
}

// readPaged is the paging read_input and the knowledge tools share: up to
// the server cap (or the caller's tighter maxBytes) from offset, cut at a
// rune boundary with a note saying where, and a binary file refused by
// size. A non-nil error is the caller's to prefix with its tool name; the
// binary refusal is a tool error with no prefix, as read_input has always
// said it.
func readPaged(abs string, offset, maxBytes, serverCap int64) (string, bool, error) {
	fi, err := os.Stat(abs)
	if err != nil {
		return "", true, err
	}
	limit := serverCap
	if maxBytes > 0 && maxBytes < limit {
		limit = maxBytes
	}
	f, err := os.Open(abs) //nolint:gosec // resolved under a tool root by the caller
	if err != nil {
		return "", true, err
	}
	defer func() { _ = f.Close() }()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return "", true, err
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return "", true, err
	}
	total := fi.Size()
	truncated := offset+int64(len(data)) < total
	if truncated {
		data = trimPartialRune(data)
	}
	if !utf8.Valid(data) {
		return fmt.Sprintf("binary file; %d bytes", total), true, nil
	}
	text := string(data)
	if truncated {
		text += fmt.Sprintf("\n[truncated at %d bytes of %d]", len(data), total)
	}
	return text, false, nil
}

// isBinary says whether a file is not text, as the decode step tells it:
// its first 64 KiB hold a NUL or are not UTF-8 (a rune cut at the end
// aside). A file that cannot be read is not called binary; the read that
// follows says why.
func isBinary(abs string) bool {
	data, more, err := readUpTo(abs, 64<<10)
	if err != nil {
		return false
	}
	if more {
		data = trimPartialRune(data)
	}
	return bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data)
}

// readBase64Page answers one page of a binary original (agentry-cli#631):
// its size, its detected type and the page's bytes as base64, sized so the
// encoding stays within the server cap (or the caller's tighter maxBytes,
// counted in raw bytes).
func readBase64Page(abs string, offset, maxBytes, serverCap int64, mime string) (string, error) {
	f, err := os.Open(abs) //nolint:gosec // resolved under a tool root by the caller
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	total := fi.Size()
	if offset >= total {
		return fmt.Sprintf("binary file; %d bytes; offset %d is past its end", total, offset), nil
	}
	limit := serverCap / 4 * 3
	if maxBytes > 0 && maxBytes < limit {
		limit = maxBytes
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return "", err
	}
	if mime == "" {
		mime = "a type not detected"
	}
	end := offset + int64(len(data))
	next := ""
	if end < total {
		next = fmt.Sprintf("; pass offset %d for the next page", end)
	}
	return fmt.Sprintf("binary file; %d bytes, detected as %s; bytes %d to %d as base64%s:\n%s",
		total, mime, offset, end, next, base64.StdEncoding.EncodeToString(data)), nil
}

// ---- write_report ----

func (s *server) toolWriteReport(raw json.RawMessage) (string, bool) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Append  bool   `json:"append"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return s.failed("write_report", err)
	}
	if a.Path == "" {
		return "write_report: path is required", true
	}
	if limit := s.spec.Limits.writeCap(); int64(len(a.Content)) > limit {
		return fmt.Sprintf("write_report: content is %d bytes, over the %d-byte limit — append in parts", len(a.Content), limit), true
	}
	// Reports root under the out mount (RQ3): created lazily so a run
	// that writes no report leaves no empty reports/ dir behind.
	root := filepath.Join(s.spec.OutDir, "reports")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return s.failed("write_report", err)
	}
	s.own(root)
	abs, err := resolveUnder(root, a.Path)
	if err != nil {
		return s.failed("write_report", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return s.failed("write_report", err)
	}
	s.ownBetween(root, filepath.Dir(abs))
	defer s.own(abs)
	if a.Append {
		f, err := os.OpenFile(abs, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return s.failed("write_report", err)
		}
		_, werr := f.Write([]byte(a.Content))
		cerr := f.Close()
		if werr != nil {
			return s.failed("write_report", werr)
		}
		if cerr != nil {
			return s.failed("write_report", cerr)
		}
	} else {
		if err := os.WriteFile(abs, []byte(a.Content), 0o644); err != nil {
			return s.failed("write_report", err)
		}
	}
	return fmt.Sprintf("report written: reports/%s (%d bytes)", path.Clean(a.Path), len(a.Content)), false
}

// own gives path to the host user when the spec names one (agentry-cli#55).
// Best-effort: a mount that forbids chown (a non-Linux Docker Desktop
// share, a uid-mapped mount) leaves the file as it is rather than failing
// the tool — the write itself succeeded, and the receipt shows the result.
func (s *server) own(path string) {
	if s.spec.Owner == nil || path == "" {
		return
	}
	_ = os.Chown(path, s.spec.Owner.UID, s.spec.Owner.GID)
}

// ownBetween chowns every directory below root down to dir (inclusive) —
// the directories MkdirAll may have just created for a nested path.
func (s *server) ownBetween(root, dir string) {
	if s.spec.Owner == nil {
		return
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		s.own(cur)
	}
}
