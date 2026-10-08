// Wire-level tests for the MCP loop: the tool surface must equal the
// grant exactly (RUNNER-PLAN §R11 invariant 4), and malformed traffic
// must cost one error line, never the server.

package toolsrv

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// runServe feeds lines to Serve and returns the decoded response objects
// in emission order. Input is a plain reader: handling is serial, so all
// requests can be queued up front without deadlocking.
func runServe(t *testing.T, spec *Spec, lines ...string) []map[string]any {
	t.Helper()
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out, logs bytes.Buffer
	if err := Serve(in, &out, &logs, spec); err != nil {
		t.Fatalf("Serve: %v (logs: %s)", err, logs.String())
	}
	var resps []map[string]any
	for _, ln := range strings.Split(out.String(), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("response line %q is not JSON: %v", ln, err)
		}
		resps = append(resps, m)
	}
	return resps
}

func baseSpec(t *testing.T) *Spec {
	t.Helper()
	return &Spec{
		SpecVersion:  SpecVersion,
		WorkspaceDir: t.TempDir(),
		OutDir:       t.TempDir(),
	}
}

func resultOf(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	r, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result object: %v", resp)
	}
	return r
}

func errorOf(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error object: %v", resp)
	}
	return e
}

func toolNames(t *testing.T, listResp map[string]any) []string {
	t.Helper()
	tools, ok := resultOf(t, listResp)["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list result has no tools array: %v", listResp)
	}
	var names []string
	for _, tl := range tools {
		m, ok := tl.(map[string]any)
		if !ok {
			t.Fatalf("tool entry is not an object: %v", tl)
		}
		name, _ := m["name"].(string)
		names = append(names, name)
		if d, _ := m["description"].(string); d == "" {
			t.Errorf("tool %q has no description", name)
		}
		schema, ok := m["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Errorf("tool %q inputSchema is not a JSON Schema object: %v", name, m["inputSchema"])
		}
	}
	return names
}

const initLine = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
const listLine = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`

// TestToolSurfaceMatchesGrant is invariant 4 at the wire: the granted
// capability set and ONLY the granted capability set shows up.
func TestToolSurfaceMatchesGrant(t *testing.T) {
	tests := []struct {
		name  string
		shape func(*Spec)
		want  []string
	}{
		{
			name:  "pure-text agent",
			shape: func(s *Spec) {},
			want:  []string{"write_report"},
		},
		{
			name: "full grant",
			shape: func(s *Spec) {
				s.FSRead = Access{Enabled: true}
				s.FSWrite = Access{Enabled: true}
				s.Fetch = Fetch{Enabled: true, Hosts: []string{"api.example.com"}}
				s.Exec = Exec{Commands: []string{"sh", "echo"}}
			},
			want: []string{"fs_read", "fs_list", "fs_write", "fetch", "run_command", "write_report"},
		},
		{
			name:  "read-only",
			shape: func(s *Spec) { s.FSRead = Access{Enabled: true} },
			want:  []string{"fs_read", "fs_list", "write_report"},
		},
		{
			name:  "fetch-only",
			shape: func(s *Spec) { s.Fetch = Fetch{Enabled: true, AllowAll: true} },
			want:  []string{"fetch", "write_report"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := baseSpec(t)
			tc.shape(spec)
			resps := runServe(t, spec, initLine, listLine)
			if len(resps) != 2 {
				t.Fatalf("got %d responses, want 2: %v", len(resps), resps)
			}
			got := toolNames(t, resps[1])
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("tools/list = %v, want exactly %v", got, tc.want)
			}
		})
	}
}

func TestInitializeHandshake(t *testing.T) {
	spec := baseSpec(t)

	t.Run("echoes requested protocol version", func(t *testing.T) {
		resps := runServe(t, spec, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2099-01-01"}}`)
		r := resultOf(t, resps[0])
		if r["protocolVersion"] != "2099-01-01" {
			t.Fatalf("protocolVersion = %v, want echo of 2099-01-01", r["protocolVersion"])
		}
		info, _ := r["serverInfo"].(map[string]any)
		if info["name"] != "agentry-runner-tools" || info["version"] != "1" {
			t.Fatalf("serverInfo = %v", r["serverInfo"])
		}
		caps, _ := r["capabilities"].(map[string]any)
		if _, ok := caps["tools"]; !ok {
			t.Fatalf("capabilities missing tools: %v", r["capabilities"])
		}
	})

	t.Run("defaults protocol version when absent", func(t *testing.T) {
		resps := runServe(t, spec, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
		if pv := resultOf(t, resps[0])["protocolVersion"]; pv != defaultProtocolVersion {
			t.Fatalf("protocolVersion = %v, want %q", pv, defaultProtocolVersion)
		}
	})
}

func TestServeProtocolEdges(t *testing.T) {
	spec := baseSpec(t)

	t.Run("unknown method with id gets -32601", func(t *testing.T) {
		resps := runServe(t, spec, `{"jsonrpc":"2.0","id":7,"method":"bogus/nope"}`)
		if len(resps) != 1 {
			t.Fatalf("got %d responses, want 1", len(resps))
		}
		if code := errorOf(t, resps[0])["code"]; code != float64(-32601) {
			t.Fatalf("error code = %v, want -32601", code)
		}
	})

	t.Run("parse error line does not kill the server", func(t *testing.T) {
		resps := runServe(t, spec,
			`{this is not json`,
			`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
		)
		if len(resps) != 1 {
			t.Fatalf("got %d responses, want just the ping's: %v", len(resps), resps)
		}
		if resps[0]["id"] != float64(3) {
			t.Fatalf("response id = %v, want 3", resps[0]["id"])
		}
		if r := resultOf(t, resps[0]); len(r) != 0 {
			t.Fatalf("ping result = %v, want {}", r)
		}
	})

	t.Run("invalid request with id gets -32600", func(t *testing.T) {
		resps := runServe(t, spec, `{"jsonrpc":"2.0","id":9}`)
		if code := errorOf(t, resps[0])["code"]; code != float64(-32600) {
			t.Fatalf("error code = %v, want -32600", code)
		}
	})

	t.Run("notifications never get responses", func(t *testing.T) {
		resps := runServe(t, spec,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			`{"jsonrpc":"2.0","method":"notifications/whatever"}`,
			`{"jsonrpc":"2.0","id":4,"method":"ping"}`,
		)
		if len(resps) != 1 {
			t.Fatalf("got %d responses, want only the ping's: %v", len(resps), resps)
		}
	})

	t.Run("empty lines are ignored", func(t *testing.T) {
		resps := runServe(t, spec, ``, `   `, `{"jsonrpc":"2.0","id":5,"method":"ping"}`)
		if len(resps) != 1 {
			t.Fatalf("got %d responses, want 1", len(resps))
		}
	})
}

func TestToolsCallOverTheWire(t *testing.T) {
	t.Run("granted tool runs end to end", func(t *testing.T) {
		spec := baseSpec(t)
		resps := runServe(t, spec,
			initLine,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_report","arguments":{"path":"r.md","content":"hi"}}}`,
		)
		r := resultOf(t, resps[1])
		if r["isError"] != false {
			t.Fatalf("isError = %v, want false (result: %v)", r["isError"], r)
		}
		content, _ := r["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("content = %v, want one text block", r["content"])
		}
		block, _ := content[0].(map[string]any)
		if block["type"] != "text" {
			t.Fatalf("content type = %v, want text", block["type"])
		}
		if text, _ := block["text"].(string); text != "report written: reports/r.md (2 bytes)" {
			t.Fatalf("text = %q", text)
		}
	})

	t.Run("ungranted tool is unknown", func(t *testing.T) {
		spec := baseSpec(t) // pure-text: fs_read not granted
		resps := runServe(t, spec,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fs_read","arguments":{"path":"x"}}}`,
		)
		e := errorOf(t, resps[0])
		if e["code"] != float64(-32602) || !strings.Contains(e["message"].(string), "unknown tool") {
			t.Fatalf("error = %v, want -32602 unknown tool", e)
		}
	})

	t.Run("missing tool name is invalid params", func(t *testing.T) {
		spec := baseSpec(t)
		resps := runServe(t, spec, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}}`)
		if code := errorOf(t, resps[0])["code"]; code != float64(-32602) {
			t.Fatalf("error code = %v, want -32602", code)
		}
	})
}

func TestValidateSpec(t *testing.T) {
	abs := t.TempDir()
	tests := []struct {
		name    string
		spec    *Spec
		wantErr string
	}{
		{name: "nil", spec: nil, wantErr: "nil tool spec"},
		{
			name:    "wrong version",
			spec:    &Spec{SpecVersion: "1", WorkspaceDir: abs, OutDir: abs},
			wantErr: "not supported",
		},
		{
			// A plan compiled for export_pdf meets an image built before it
			// (agentry-cli#730): the server refuses, the probe fails and the
			// run stops (exit 13), never runs with export_pdf missing.
			name:    "the version before export_pdf",
			spec:    &Spec{SpecVersion: "8", WorkspaceDir: abs, OutDir: abs},
			wantErr: "not supported",
		},
		{
			name:    "relative workspace",
			spec:    &Spec{SpecVersion: SpecVersion, WorkspaceDir: "workspace", OutDir: abs},
			wantErr: "workspaceDir",
		},
		{
			name:    "empty out dir",
			spec:    &Spec{SpecVersion: SpecVersion, WorkspaceDir: abs, OutDir: ""},
			wantErr: "outDir",
		},
		{
			name: "valid",
			spec: &Spec{SpecVersion: SpecVersion, WorkspaceDir: abs, OutDir: abs},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSpec(tc.spec)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateSpec: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateSpec = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}
