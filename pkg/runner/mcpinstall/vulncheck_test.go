package mcpinstall

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"
)

func TestCheckVulnerabilities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/mcp-servers/scan" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		switch q.Get("ecosystem") + ":" + q.Get("package") + "@" + q.Get("version") {
		case "npm:minimist@1.2.5":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "vulnerable", "maxSeverity": "critical", "lastCheckedAt": "2026-09-06T10:00:00Z",
				"advisories": []map[string]any{
					{"id": "GHSA-xvch-5gv4-984h", "aliases": []string{"CVE-2021-44906"}, "severity": "critical", "summary": "Prototype Pollution", "fixedIn": []string{"1.2.6"}},
					{"id": "GHSA-low", "severity": "low", "summary": "minor"},
				},
			})
		case "npm:@modelcontextprotocol/server-github@2026.1.2":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "safe", "advisories": []any{}, "lastCheckedAt": "2026-09-06T10:00:00Z"})
		case "pypi:mcp-server-fetch@0.6.2":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "vulnerable", "maxSeverity": "medium",
				"advisories": []map[string]any{{"id": "PYSEC-1", "severity": "medium", "summary": "meh"}}})
		case "pypi:never-seen@1.0.0":
			http.Error(w, `{"title":"Not found"}`, http.StatusNotFound)
		default:
			http.Error(w, "boom", http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	servers := []manifest.MCPServer{
		{NPM: "minimist", Version: "1.2.5"},
		{NPM: "@modelcontextprotocol/server-github", Version: "2026.1.2"},
		{PyPI: "mcp-server-fetch", Version: "0.6.2"},
		{PyPI: "never-seen", Version: "1.0.0"},
		{PyPI: "broken", Version: "0.1"},
	}
	got := CheckVulnerabilities(context.Background(), nil, srv.URL+"/", servers)
	if len(got) != 5 {
		t.Fatalf("statuses: %d", len(got))
	}
	if got[0].Status != "vulnerable" || !got[0].Refuses() || got[0].FixedIn()[0] != "1.2.6" || got[0].Ref != "npm:minimist@1.2.5" {
		t.Fatalf("minimist: %+v", got[0])
	}
	if got[1].Status != "safe" || got[1].Refuses() {
		t.Fatalf("server-github: %+v", got[1])
	}
	if got[2].Status != "vulnerable" || got[2].Refuses() {
		t.Fatalf("medium-only must not refuse: %+v", got[2])
	}
	if got[3].Status != "unknown" || got[3].Refuses() {
		t.Fatalf("never seen: %+v", got[3])
	}
	if got[4].Status != "unreachable" || got[4].Err == "" || got[4].Refuses() {
		t.Fatalf("502: %+v", got[4])
	}
	s := Summary(got)
	for _, want := range []string{"npm:minimist@1.2.5 vulnerable (critical: GHSA-xvch-5gv4-984h, GHSA-low)", "server-github@2026.1.2 safe", "never-seen@1.0.0 unknown", "broken@0.1 unreachable (registry answered 502)"} {
		if !contains(s, want) {
			t.Fatalf("summary %q lacks %q", s, want)
		}
	}
}

func TestCheckVulnerabilitiesUnreachableRegistry(t *testing.T) {
	got := CheckVulnerabilities(context.Background(), nil, "http://127.0.0.1:1", []manifest.MCPServer{{NPM: "x", Version: "1"}})
	if len(got) != 1 || got[0].Status != "unreachable" || got[0].Refuses() {
		t.Fatalf("unreachable: %+v", got)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
