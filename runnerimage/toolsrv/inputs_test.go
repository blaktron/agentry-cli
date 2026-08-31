// Run-inputs channel tests (RUNNER-PLAN-2 §P4): list_inputs/read_input
// exist exactly when the spec configures an inputs dir, stay read-only,
// and hold the same path-safety and byte-cap lines as the fs tools — plus
// the wire-plumbing edges (argument decoding, response write failures)
// that no per-tool test exercises.

package toolsrv

import (
	"encoding/json"
	"errors"
	"io"
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
		got, isErr := s.toolListInputs(json.RawMessage(`{"path":42}`))
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
			want: "0123456789\n[truncated at 10 bytes of 20]"},
		{name: "caller cap tighter", args: map[string]any{"path": "big.txt", "maxBytes": 3},
			want: "012\n[truncated at 3 bytes of 20]"},
		{name: "offset pages forward", args: map[string]any{"path": "big.txt", "offset": 15},
			want: "FGHIJ"},
		{name: "offset plus cap", args: map[string]any{"path": "big.txt", "offset": 5, "maxBytes": 4},
			want: "5678\n[truncated at 4 bytes of 20]"},
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

// TestInputsToolsOnTheWire pins the §P4 surface rule end to end: the two
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
		content := res["content"].([]any)[0].(map[string]any)
		if content["text"] != "payload" {
			t.Errorf("read_input text = %v", content["text"])
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
