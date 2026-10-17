// Run-inputs channel tests: list_inputs/read_input
// exist exactly when the spec configures an inputs dir, stay read-only,
// and hold the same path-safety and byte-cap lines as the fs tools — plus
// the wire-plumbing edges (argument decoding, response write failures)
// that no per-tool test exercises.

package toolsrv

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// inputsSpec is baseSpec plus a configured inputs channel.
func inputsSpec(t *testing.T) *Spec {
	t.Helper()
	s := baseSpec(t)
	s.InputsDir = t.TempDir()
	return s
}

func TestListInputs(t *testing.T) {
	spec := inputsSpec(t)
	mustWrite(t, filepath.Join(spec.InputsDir, "incident.json"), []byte(`{"id":42}`))
	mustWrite(t, filepath.Join(spec.InputsDir, "attachments", "a.txt"), []byte("aaaa"))
	s := newTestServer(t, spec)

	t.Run("root lists files and directories", func(t *testing.T) {
		got, isErr := s.toolListInputs(rawArgs(t, map[string]any{}))
		if isErr {
			t.Fatalf("toolListInputs errored: %s", got)
		}
		if !strings.Contains(got, "attachments/") || !strings.Contains(got, "incident.json\t9") {
			t.Errorf("root listing = %q", got)
		}
	})
	t.Run("explicit dot is the root", func(t *testing.T) {
		got, isErr := s.toolListInputs(rawArgs(t, map[string]any{"path": "."}))
		if isErr || !strings.Contains(got, "incident.json") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("subdirectory listing", func(t *testing.T) {
		got, isErr := s.toolListInputs(rawArgs(t, map[string]any{"path": "attachments"}))
		if isErr || got != "a.txt\t4" {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("missing directory is a tool error", func(t *testing.T) {
		got, isErr := s.toolListInputs(rawArgs(t, map[string]any{"path": "nope"}))
		if !isErr || !strings.Contains(got, "list_inputs:") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("traversal refused", func(t *testing.T) {
		got, isErr := s.toolListInputs(rawArgs(t, map[string]any{"path": "../outside"}))
		if !isErr || !strings.Contains(got, "escapes the tool root") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("garbage arguments", func(t *testing.T) {
		got, isErr := s.toolListInputs(json.RawMessage(`{"path":`))
		if !isErr || !strings.Contains(got, "invalid arguments") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("empty inputs dir says so", func(t *testing.T) {
		empty := inputsSpec(t)
		se := newTestServer(t, empty)
		got, isErr := se.toolListInputs(rawArgs(t, map[string]any{}))
		if isErr || got != "(no input files provided for this run)" {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("unconfigured channel refuses", func(t *testing.T) {
		bare := newTestServer(t, baseSpec(t))
		got, isErr := bare.toolListInputs(rawArgs(t, map[string]any{}))
		if !isErr || !strings.Contains(got, "no inputs channel configured") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
}

func TestReadInput(t *testing.T) {
	spec := inputsSpec(t)
	spec.Limits.MaxReadBytes = 10
	mustWrite(t, filepath.Join(spec.InputsDir, "small.txt"), []byte("hello"))
	mustWrite(t, filepath.Join(spec.InputsDir, "big.txt"), []byte("0123456789ABCDEFGHIJ")) // 20 bytes
	mustWrite(t, filepath.Join(spec.InputsDir, "bin.dat"), []byte{0xff, 0xfe, 0x00, 0x01})
	mustWrite(t, filepath.Join(spec.InputsDir, "sub", "nested.txt"), []byte("nested"))
	s := newTestServer(t, spec)

	tests := []struct {
		name    string
		args    map[string]any
		want    string
		wantErr bool
	}{
		{name: "whole file", args: map[string]any{"path": "small.txt"}, want: "hello"},
		{name: "nested file", args: map[string]any{"path": "sub/nested.txt"}, want: "nested"},
		{name: "server cap notes truncation", args: map[string]any{"path": "big.txt"},
			want: "0123456789\n[page: bytes 0–10 of 20; continue with offset 10]"},
		{name: "caller cap tighter", args: map[string]any{"path": "big.txt", "maxBytes": 3},
			want: "012\n[page: bytes 0–3 of 20; continue with offset 3]"},
		{name: "offset pages forward", args: map[string]any{"path": "big.txt", "offset": 15},
			want: "FGHIJ\n[page: bytes 15–20 of 20; end of file]"},
		{name: "offset plus cap", args: map[string]any{"path": "big.txt", "offset": 5, "maxBytes": 4},
			want: "5678\n[page: bytes 5–9 of 20; continue with offset 9]"},
		{name: "binary refused", args: map[string]any{"path": "bin.dat"},
			want: "binary file; 4 bytes", wantErr: true},
		{name: "directory refused", args: map[string]any{"path": "sub"},
			want: "is a directory (use list_inputs)", wantErr: true},
		{name: "traversal refused", args: map[string]any{"path": "../secrets"},
			want: "escapes the tool root", wantErr: true},
		{name: "missing path", args: map[string]any{},
			want: "path is required", wantErr: true},
		{name: "negative offset", args: map[string]any{"path": "small.txt", "offset": -1},
			want: "offset must be >= 0", wantErr: true},
		{name: "absent file", args: map[string]any{"path": "gone.txt"},
			want: "read_input:", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, isErr := s.toolReadInput(rawArgs(t, tc.args))
			if isErr != tc.wantErr {
				t.Fatalf("isErr = %v (%q)", isErr, got)
			}
			if tc.wantErr {
				if !strings.Contains(got, tc.want) {
					t.Errorf("got %q, want it to mention %q", got, tc.want)
				}
			} else if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("garbage arguments", func(t *testing.T) {
		got, isErr := s.toolReadInput(json.RawMessage(`[1,2]`))
		if !isErr || !strings.Contains(got, "invalid arguments") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("unconfigured channel refuses", func(t *testing.T) {
		bare := newTestServer(t, baseSpec(t))
		got, isErr := bare.toolReadInput(rawArgs(t, map[string]any{"path": "x"}))
		if !isErr || !strings.Contains(got, "no inputs channel configured") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
}

// TestInputsToolsOnTheWire pins the surface rule end to end: the two
// inputs tools (schemas included) are advertised exactly when the channel
// is configured, and read_input works over tools/call.
func TestInputsToolsOnTheWire(t *testing.T) {
	t.Run("with inputs channel", func(t *testing.T) {
		spec := inputsSpec(t)
		mustWrite(t, filepath.Join(spec.InputsDir, "in.txt"), []byte("payload"))
		call := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_input","arguments":{"path":"in.txt"}}}`
		resps := runServe(t, spec, initLine, listLine, call)

		names := toolNames(t, resps[1])
		if got := strings.Join(names, ","); got != "list_inputs,read_input,write_report" {
			t.Fatalf("tool surface = %q", got)
		}
		// Schema spot-checks: read_input requires path; list_inputs does not.
		tools := resultOf(t, resps[1])["tools"].([]any)
		for _, raw := range tools {
			tool := raw.(map[string]any)
			schema := tool["inputSchema"].(map[string]any)
			switch tool["name"] {
			case ToolReadInput:
				req, _ := schema["required"].([]any)
				if len(req) != 1 || req[0] != "path" {
					t.Errorf("read_input required = %v", req)
				}
				props := schema["properties"].(map[string]any)
				for _, p := range []string{"path", "offset", "maxBytes"} {
					if _, ok := props[p]; !ok {
						t.Errorf("read_input schema lacks %q", p)
					}
				}
			case ToolListInputs:
				if _, hasReq := schema["required"]; hasReq {
					t.Error("list_inputs must not require arguments")
				}
			}
		}

		res := resultOf(t, resps[2])
		if res["isError"] != false {
			t.Fatalf("read_input over the wire errored: %v", res)
		}
		// The person's input travels as an embedded text resource, past the
		// gateway's secret block.
		content := res["content"].([]any)[0].(map[string]any)
		resource, _ := content["resource"].(map[string]any)
		if content["type"] != "resource" || resource["text"] != "payload" || resource["uri"] != InputResourceURI || resource["mimeType"] != "text/plain" {
			t.Errorf("read_input content = %v", content)
		}
	})
	t.Run("without inputs channel the tools do not exist", func(t *testing.T) {
		call := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_inputs","arguments":{}}}`
		resps := runServe(t, baseSpec(t), initLine, listLine, call)
		for _, n := range toolNames(t, resps[1]) {
			if n == ToolListInputs || n == ToolReadInput {
				t.Fatalf("inputs tool %q advertised without a channel", n)
			}
		}
		// Calling anyway answers exactly like a nonexistent tool.
		e := errorOf(t, resps[2])
		if !strings.Contains(e["message"].(string), "unknown tool") {
			t.Errorf("ungranted call error = %v", e)
		}
	})
}

func TestUnmarshalArgs(t *testing.T) {
	var v struct {
		Path string `json:"path"`
	}
	if err := unmarshalArgs(nil, &v); err != nil || v.Path != "" {
		t.Errorf("nil args: (%+v, %v), want empty defaults", v, err)
	}
	if err := unmarshalArgs(json.RawMessage(`{"path":"x","unknown":true}`), &v); err != nil || v.Path != "x" {
		t.Errorf("unknown fields must be tolerated: (%+v, %v)", v, err)
	}
	if err := unmarshalArgs(json.RawMessage(`{broken`), &v); err == nil || !strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("garbage args: %v", err)
	}
}

// TestUnmarshalArgsCoercesJSONStrings: a model that
// sends a non-string parameter as a JSON-encoded string — "[\"a\"]" for
// an array, "200" for a number, "true" for a bool, or the whole
// arguments object as one string — gets the value it meant; a string
// that is not that type is refused naming the parameter, what arrived and
// what was wanted; a string parameter is never touched.
func TestUnmarshalArgsCoercesJSONStrings(t *testing.T) {
	type args struct {
		Command  string   `json:"command"`
		Args     []string `json:"args"`
		MaxBytes int64    `json:"maxBytes"`
		Append   bool     `json:"append"`
	}
	var v args
	if err := unmarshalArgs(json.RawMessage(`{"command":"git","args":"[\"log\",\"--oneline\"]","maxBytes":"200","append":"true"}`), &v); err != nil {
		t.Fatalf("coercion failed: %v", err)
	}
	if v.Command != "git" || len(v.Args) != 2 || v.Args[1] != "--oneline" || v.MaxBytes != 200 || !v.Append {
		t.Fatalf("coerced = %+v", v)
	}
	v = args{}
	if err := unmarshalArgs(json.RawMessage(`"{\"command\":\"echo\",\"args\":[\"hi\"]}"`), &v); err != nil || v.Command != "echo" || len(v.Args) != 1 {
		t.Fatalf("stringified object: %+v %v", v, err)
	}
	v = args{}
	if err := unmarshalArgs(json.RawMessage(`{"command":"[1]","args":["x"]}`), &v); err != nil || v.Command != "[1]" {
		t.Fatalf("a string parameter must be taken as written: %+v %v", v, err)
	}
	err := unmarshalArgs(json.RawMessage(`{"command":"git","maxBytes":"lots"}`), &v)
	if err == nil || !strings.Contains(err.Error(), "maxBytes") || !strings.Contains(err.Error(), `"lots"`) || !strings.Contains(err.Error(), "int64") {
		t.Fatalf("uncoercible string must be refused naming the parameter, the value and the type: %v", err)
	}
	err = unmarshalArgs(json.RawMessage(`{"command":"git","args":"log --oneline"}`), &v)
	if err == nil || !strings.Contains(err.Error(), "args") || !strings.Contains(err.Error(), "[]string") {
		t.Fatalf("a plain string for an array must be refused: %v", err)
	}
}

// failWriter errors on every write — a gateway that hung up mid-response.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("pipe closed") }

func TestWriteMsgFailuresAreLoggedNotFatal(t *testing.T) {
	t.Run("write failure", func(t *testing.T) {
		var logs strings.Builder
		s := &server{out: failWriter{}, logw: &logs, tools: map[string]toolDef{}}
		s.writeResult(json.RawMessage("1"), map[string]any{})
		if !strings.Contains(logs.String(), "write response") {
			t.Errorf("logs = %q", logs.String())
		}
	})
	t.Run("unmarshalable result", func(t *testing.T) {
		var logs strings.Builder
		s := &server{out: io.Discard, logw: &logs, tools: map[string]toolDef{}}
		s.writeMsg(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("1"), Result: make(chan int)})
		if !strings.Contains(logs.String(), "marshal response") {
			t.Errorf("logs = %q", logs.String())
		}
	})
}

// TestInputsReachTheAgentUnchanged pins that a person's input that holds a
// credential is served byte for byte, as an embedded text resource, which the
// gateway's
// --block-secrets does not scan. Everything else the server answers stays
// a text item, which it does: the listing, and an input tool's error.
func TestInputsReachTheAgentUnchanged(t *testing.T) {
	spec := inputsSpec(t)
	// Built at run time, so the repository's own gitleaks job does not
	// read the test as a leak.
	key := "sk_" + "live_" + strings.Repeat("4eC39HqLyjWDarjtT1zdp7dc", 1)
	dockerfile := "FROM alpine:3.23\nENV API_KEY=" + key + "\nCMD [\"sh\"]\n"
	mustWrite(t, filepath.Join(spec.InputsDir, "Dockerfile"), []byte(dockerfile))
	read := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_input","arguments":{"path":"Dockerfile","view":"original"}}}`
	list := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_inputs","arguments":{}}}`
	missing := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"read_input","arguments":{"path":"nope.txt"}}}`
	resps := runServe(t, spec, initLine, listLine, read, list, missing)

	res := resultOf(t, resps[2])
	content := res["content"].([]any)[0].(map[string]any)
	resource, _ := content["resource"].(map[string]any)
	text, _ := resource["text"].(string)
	if content["type"] != "resource" || res["isError"] != false || !strings.Contains(text, dockerfile) {
		t.Fatalf("read_input of a Dockerfile with a key = %v", res)
	}
	for i, what := range map[int]string{3: "list_inputs", 4: "read_input's error"} {
		c := resultOf(t, resps[i])["content"].([]any)[0].(map[string]any)
		if c["type"] != "text" {
			t.Errorf("%s is %v, want a text item", what, c["type"])
		}
	}

	// Of every tool the server grants, only the two that answer with the
	// person's input content serve it as a resource: fetch, run_command and
	// the rest stay text items under the gateway's block.
	spec.InputsTextDir = t.TempDir()
	spec.Fetch = Fetch{Enabled: true, Hosts: []string{"api.invalid.example"}}
	spec.Exec = Exec{Commands: []string{"cat"}}
	s := newTestServer(t, spec)
	for name, d := range s.tools {
		want := name == ToolReadInput || name == ToolSearchInputs
		if d.personsInput != want {
			t.Errorf("%s personsInput = %v, want %v", name, d.personsInput, want)
		}
	}
	for _, name := range []string{ToolReadInput, ToolSearchInputs, ToolFetch, ToolRunCommand} {
		if _, ok := s.tools[name]; !ok {
			t.Errorf("%s is not granted in the test's spec", name)
		}
	}

	// Under the review grant a file reaches the model only after AI
	// Review, which may answer with a withheld file's reason instead: no
	// tool answers as a resource there.
	rspec := inputsSpec(t)
	rspec.Review = Review{Enabled: true, Ledger: filepath.Join(rspec.OutDir, "receipts", "review.ndjson")}
	for name, d := range newTestServer(t, rspec).tools {
		if d.personsInput {
			t.Errorf("under the review grant %s answers as a resource", name)
		}
	}
}

// TestAReportNeverHoldsARunSecret: the person's inputs are served
// unchanged and may hold a value of the run's own secrets; the gateway's
// block matches only secret-shaped text, so write_report and fs_write
// refuse content that holds one of the server's granted secret values,
// whatever its shape.
func TestAReportNeverHoldsARunSecret(t *testing.T) {
	const env = "AGENTRY_T632_TOKEN"
	value := "plain" + "words-no-shape-" + "7f3a"
	t.Setenv(env, value)
	spec := inputsSpec(t)
	spec.Fetch = Fetch{Enabled: true, Hosts: []string{"api.invalid.example"}, SecretEnv: []string{env}}
	spec.FSWrite = Access{Enabled: true}
	s := newTestServer(t, spec)
	for _, tool := range []string{ToolWriteReport, ToolFSWrite} {
		d, ok := s.tools[tool]
		if !ok {
			t.Fatalf("%s not granted", tool)
		}
		got, isErr := d.call(rawArgs(t, map[string]any{"path": "found.md", "content": "found " + value + " in Dockerfile:2"}))
		if !isErr || !strings.Contains(got, env) || strings.Contains(got, value) {
			t.Errorf("%s with a run secret's value = (%q, %v)", tool, got, isErr)
		}
		got, isErr = d.call(rawArgs(t, map[string]any{"path": "found.md", "content": "a token at Dockerfile:2"}))
		if isErr {
			t.Errorf("%s without one = (%q, %v)", tool, got, isErr)
		}
	}
	// export_pdf reads a file the agent could have written some other way
	// (a declared command in the scratch folder): it renders none that
	// holds a run secret's value, and Typst never sees it.
	spec.ScratchDir = t.TempDir()
	spec.PDF = PDF{Enabled: true}
	s = newTestServer(t, spec)
	s.typst = fakeTypst(t, `printf '%%PDF-1.7\n'`)
	mustWrite(t, filepath.Join(spec.ScratchDir, "found.md"), []byte("# Found\n\nfound "+value+" in Dockerfile:2\n"))
	got, isErr := s.toolExportPDF(rawArgs(t, map[string]any{"source": "scratch/found.md"}))
	if !isErr || !strings.Contains(got, env) || strings.Contains(got, value) {
		t.Errorf("export_pdf of a source with a run secret's value = (%q, %v)", got, isErr)
	}
	if entries, _ := os.ReadDir(filepath.Join(spec.OutDir, "reports")); len(entries) != 1 {
		t.Errorf("reports/ holds %d entries after the refused export, want only the found.md write_report left there, and no PDF", len(entries))
	}
}
