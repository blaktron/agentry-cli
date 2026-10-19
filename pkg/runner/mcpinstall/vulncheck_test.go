package mcpinstall

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	for _, want := range []string{
		"npm:minimist@1.2.5 known advisories (critical: GHSA-xvch-5gv4-984h, GHSA-low)",
		"server-github@2026.1.2 no known advisories (checked 2026-09-06)",
		"never-seen@1.0.0 not checked: the registry has not scanned this version",
		"broken@0.1 not checked: no answer from the registry (registry answered 502)",
	} {
		if !contains(s, want) {
			t.Fatalf("summary %q lacks %q", s, want)
		}
	}
	if contains(s, "safe") {
		t.Fatalf("summary %q shows the registry's word safe", s)
	}
}

// A person never reads a status word: "safe" is no known advisories at the
// registry's check date, and every other word has plain words too. The
// JSON keeps the wire value.
func TestVulnStatusWords(t *testing.T) {
	at := time.Date(2026, 9, 25, 23, 30, 0, 0, time.FixedZone("x", -3*3600))
	cases := []struct {
		s    VulnStatus
		want string
	}{
		{VulnStatus{Status: "safe", CheckedAt: at}, "no known advisories (checked 2026-09-26)"},
		{VulnStatus{Status: "safe"}, "no known advisories"},
		{VulnStatus{Status: "vulnerable", Advisories: []VulnAdvisory{{ID: "GHSA-1", Severity: "high"}}}, "known advisories"},
		{VulnStatus{Status: "error", Advisories: []VulnAdvisory{{ID: "GHSA-1", Severity: "high"}}}, "the registry's last lookup failed; it last knew advisories"},
		{VulnStatus{Status: "error"}, "the registry's last lookup failed; it knows no advisories"},
		{VulnStatus{Status: "unknown"}, "not checked: the registry has not scanned this version"},
		{VulnStatus{Status: "unreachable", Err: "dial tcp: refused"}, "not checked: no answer from the registry"},
		{VulnStatus{Status: StatusRemote}, "not looked up: a remote server is its vendor's running service, not a package version"},
		{VulnStatus{Status: "fine\x1b[2J"}, `an answer this CLI does not know ("fine\x1b[2J")`},
	}
	for _, c := range cases {
		if got := c.s.Words(); got != c.want {
			t.Errorf("%q: Words() = %q, want %q", c.s.Status, got, c.want)
		}
		if c.s.Status != "safe" && contains(c.s.Words(), "safe") {
			t.Errorf("%q: Words() = %q says safe", c.s.Status, c.s.Words())
		}
	}
	b, err := json.Marshal(VulnStatus{Ref: "npm:x@1", Status: "safe", CheckedAt: at})
	if err != nil || !contains(string(b), `"status":"safe"`) {
		t.Fatalf("the JSON keeps the wire value: %s %v", b, err)
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

// The runner weighs the gate's rating where the registry serves it:
// a High below CVSS 8.5 is medium and runs, a High from
// 8.5 and a Critical refuse. A registry that serves no rating is weighed
// by the advisory's own severity, as before.
func TestVulnRefusalReadsTheGatesRating(t *testing.T) {
	score := func(f float64) *float64 { return &f }
	sdk1252 := VulnStatus{Ref: "npm:@modelcontextprotocol/sdk@1.25.2", Status: "vulnerable", MaxSeverity: "high",
		Advisories: []VulnAdvisory{{ID: "GHSA-71", Severity: "high", GateSeverity: "medium", CVSSScore: score(7.1), FixedIn: []string{"1.26.0"}}}}
	sdk1251 := VulnStatus{Ref: "npm:@modelcontextprotocol/sdk@1.25.1", Status: "vulnerable", MaxSeverity: "high",
		Advisories: []VulnAdvisory{
			{ID: "GHSA-71", Severity: "high", GateSeverity: "medium", CVSSScore: score(7.1), FixedIn: []string{"1.26.0"}},
			{ID: "GHSA-87", Severity: "high", GateSeverity: "high", CVSSScore: score(8.7), FixedIn: []string{"1.25.2"}},
		}}
	critical := VulnStatus{Ref: "npm:@bytebase/dbhub@0.20.0", Status: "error", MaxSeverity: "critical",
		Advisories: []VulnAdvisory{{ID: "GHSA-cr", Severity: "critical", GateSeverity: "critical", CVSSScore: score(9.8)}}}
	unscored := VulnStatus{Ref: "npm:x@1", Status: "vulnerable", MaxSeverity: "high",
		Advisories: []VulnAdvisory{{ID: "GHSA-ns", Severity: "high", GateSeverity: "high"}}}
	legacy := VulnStatus{Ref: "npm:y@1", Status: "vulnerable", MaxSeverity: "high",
		Advisories: []VulnAdvisory{{ID: "GHSA-old", Severity: "high"}}}

	if sdk1252.Refuses() {
		t.Fatalf("a High the gate rates medium must run: %+v", sdk1252)
	}
	if got := sdk1252.Describe(sdk1252.Advisories); got != "an advisory the gate rates medium (GHSA-71 [advisory high, CVSS 7.1; the gate rates it medium])" {
		t.Fatalf("describe medium: %q", got)
	}
	if !sdk1251.Refuses() || len(sdk1251.Refusing()) != 1 || sdk1251.Refusing()[0].ID != "GHSA-87" {
		t.Fatalf("the 8.7 refuses, the 7.1 does not: %+v", sdk1251.Refusing())
	}
	if f := sdk1251.FixedIn(); len(f) != 1 || f[0] != "1.25.2" {
		t.Fatalf("fixed in names the refusing advisory only: %v", f)
	}
	if !critical.Refuses() || critical.Describe(critical.Refusing()) != "an advisory the gate rates critical (GHSA-cr [advisory critical, CVSS 9.8])" {
		t.Fatalf("critical (last known on a failed lookup) refuses: %+v", critical)
	}
	if !unscored.Refuses() || unscored.Describe(unscored.Refusing()) != "an advisory the gate rates high (GHSA-ns [advisory high])" {
		t.Fatalf("unscored high: %q", unscored.Describe(unscored.Refusing()))
	}
	if !legacy.Refuses() || legacy.gateRated() || legacy.Describe(legacy.Refusing()) != "a known high vulnerability (GHSA-old)" {
		t.Fatalf("a registry without the rating keeps today's rule: %q", legacy.Describe(legacy.Refusing()))
	}
	if s := Summary([]VulnStatus{sdk1251, legacy}); !contains(s, "sdk@1.25.1 known advisories (gate-rated high: GHSA-71 [advisory high, CVSS 7.1; the gate rates it medium], GHSA-87 [advisory high, CVSS 8.7])") ||
		!contains(s, "npm:y@1 known advisories (high: GHSA-old)") {
		t.Fatalf("summary: %q", s)
	}
}

// A rating the runner does not know is no rating: the advisory's own
// severity is weighed, so a malformed answer never lets a high through. A
// Critical is critical whatever the registry rates it, as the gate rates
// every Critical.
func TestVulnRatingFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		a    VulnAdvisory
		want string
	}{
		{VulnAdvisory{Severity: "high", GateSeverity: "HIGH"}, "high"},
		{VulnAdvisory{Severity: "high", GateSeverity: "severe"}, "high"},
		{VulnAdvisory{Severity: "critical", GateSeverity: "low"}, "critical"},
		{VulnAdvisory{Severity: "critical", GateSeverity: "Critical"}, "critical"},
		{VulnAdvisory{Severity: "high", GateSeverity: "medium"}, "medium"},
		{VulnAdvisory{Severity: "medium", GateSeverity: ""}, "medium"},
	} {
		if got := tc.a.Rating(); got != tc.want {
			t.Errorf("%+v: rated %q, want %q", tc.a, got, tc.want)
		}
	}
	two := VulnStatus{Status: "vulnerable", Advisories: []VulnAdvisory{{ID: "A", Severity: "high"}, {ID: "B", Severity: "critical"}}}
	if got := two.Describe(two.Refusing()); got != "known vulnerabilities, the worst critical (A, B)" {
		t.Fatalf("two unrated: %q", got)
	}
}

// The registry's JSON carries the rating and the score per advisory.
func TestCheckVulnerabilitiesReadsGateSeverity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"vulnerable","maxSeverity":"high","lastCheckedAt":"2026-10-01T10:00:00Z",` +
			`"advisories":[{"id":"GHSA-71","severity":"high","gateSeverity":"medium","cvssScore":7.1,"summary":"s"}]}`))
	}))
	defer srv.Close()
	got := CheckVulnerabilities(context.Background(), nil, srv.URL, []manifest.MCPServer{{NPM: "@modelcontextprotocol/sdk", Version: "1.25.2"}})
	a := got[0].Advisories[0]
	if a.GateSeverity != "medium" || a.CVSSScore == nil || *a.CVSSScore != 7.1 || got[0].Refuses() {
		t.Fatalf("parsed: %+v", got[0])
	}
}

// A remote server is never looked up: it is its vendor's
// running service, with no package version, and the registry records none
// (S7/remote-server). It never refuses a run.
func TestRemoteServerIsNotLookedUp(t *testing.T) {
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	got := CheckVulnerabilities(context.Background(), srv.Client(), srv.URL, []manifest.MCPServer{
		{URL: "https://mcp.deepwiki.com/mcp", Auth: manifest.AuthNone},
	})
	if asked != 0 {
		t.Fatalf("the registry was asked %d times about a remote server", asked)
	}
	if len(got) != 1 || got[0].Status != StatusRemote || got[0].Ref != "https://mcp.deepwiki.com/mcp" || got[0].Refuses() {
		t.Fatalf("status = %+v", got)
	}
	if s := Summary(got); !strings.Contains(s, "https://mcp.deepwiki.com/mcp not looked up: a remote server") {
		t.Fatalf("summary = %q", s)
	}
}
