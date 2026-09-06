package mcpinstall

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"
)

// The runner half of MCP server vulnerability scanning (agentry#177,
// agentry-cli#58, DR-16): before a declared server is installed, ask the
// registry what its S7 scan and daily sweep recorded for that exact package
// version. The registry is the source of truth — it holds the scanned-server
// registry the sweep refreshes — so the runner never consults a
// vulnerability database itself. The check fails open on an unreachable
// registry (a stale laptop must still run) and closed on a known high or
// critical advisory (unless the operator overrides it), and either way the
// receipt says which.

// VulnAdvisory is one advisory the registry reports for a server version.
type VulnAdvisory struct {
	ID       string   `json:"id"`
	Aliases  []string `json:"aliases,omitempty"`
	Severity string   `json:"severity"`
	Summary  string   `json:"summary"`
	FixedIn  []string `json:"fixedIn,omitempty"`
}

// VulnStatus is the registry's answer for one declared server.
type VulnStatus struct {
	// Ref is "npm:pkg@1.2.3" / "pypi:pkg@1.2.3".
	Ref string `json:"ref"`
	// Status is the registry's word — "safe" (no known advisories at
	// CheckedAt), "vulnerable", "error" (its last lookup failed; Advisories
	// are the last known) — or the runner's: "unknown" (the registry has
	// never seen this version) or "unreachable" (no answer; Err says why).
	Status      string         `json:"status"`
	MaxSeverity string         `json:"maxSeverity,omitempty"`
	Advisories  []VulnAdvisory `json:"advisories,omitempty"`
	CheckedAt   time.Time      `json:"checkedAt,omitempty"`
	Err         string         `json:"error,omitempty"`
}

// Refuses reports whether this status refuses a run by default: a known
// high or critical advisory.
func (s VulnStatus) Refuses() bool {
	if s.Status != "vulnerable" && s.Status != "error" {
		return false
	}
	for _, a := range s.Advisories {
		if a.Severity == "high" || a.Severity == "critical" {
			return true
		}
	}
	return false
}

// FixedIn is the union of the fixed versions the refusing advisories name.
func (s VulnStatus) FixedIn() []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range s.Advisories {
		if a.Severity != "high" && a.Severity != "critical" {
			continue
		}
		for _, f := range a.FixedIn {
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	return out
}

// vulnCheckTimeout bounds the whole check; it runs before every install.
const vulnCheckTimeout = 15 * time.Second

// CheckVulnerabilities asks the registry about each declared server. It
// never returns an error: a registry that cannot answer yields
// "unreachable" statuses, and the caller decides what that means.
func CheckVulnerabilities(ctx context.Context, client *http.Client, registry string, servers []manifest.MCPServer) []VulnStatus {
	if client == nil {
		client = &http.Client{Timeout: vulnCheckTimeout}
	}
	base := strings.TrimRight(registry, "/")
	out := make([]VulnStatus, 0, len(servers))
	for _, srv := range servers {
		st := VulnStatus{Ref: srv.SourceID() + "@" + srv.Version}
		eco, pkg := "npm", srv.NPM
		if srv.NPM == "" {
			eco, pkg = "pypi", srv.PyPI
		}
		q := url.Values{"ecosystem": {eco}, "package": {pkg}, "version": {srv.Version}}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/mcp-servers/scan?"+q.Encode(), nil)
		if err != nil {
			st.Status, st.Err = "unreachable", err.Error()
			out = append(out, st)
			continue
		}
		req.Header.Set("Accept", "application/json")
		res, err := client.Do(req)
		if err != nil {
			st.Status, st.Err = "unreachable", err.Error()
			out = append(out, st)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		_ = res.Body.Close()
		switch res.StatusCode {
		case http.StatusOK:
			var rec struct {
				Status        string         `json:"status"`
				MaxSeverity   string         `json:"maxSeverity"`
				Advisories    []VulnAdvisory `json:"advisories"`
				LastCheckedAt time.Time      `json:"lastCheckedAt"`
				LastError     string         `json:"lastError"`
			}
			if err := json.Unmarshal(body, &rec); err != nil {
				st.Status, st.Err = "unreachable", "registry answered with something that is not a scan record: "+err.Error()
				break
			}
			st.Status, st.MaxSeverity, st.Advisories, st.CheckedAt, st.Err = rec.Status, rec.MaxSeverity, rec.Advisories, rec.LastCheckedAt, rec.LastError
		case http.StatusNotFound:
			st.Status = "unknown"
		default:
			st.Status, st.Err = "unreachable", fmt.Sprintf("registry answered %d", res.StatusCode)
		}
		out = append(out, st)
	}
	return out
}

// Summary is the one-line receipt/log form of a check.
func Summary(statuses []VulnStatus) string {
	if len(statuses) == 0 {
		return "no declared MCP servers"
	}
	parts := make([]string, 0, len(statuses))
	for _, s := range statuses {
		p := s.Ref + " " + s.Status
		if s.Status == "vulnerable" || (s.Status == "error" && len(s.Advisories) > 0) {
			ids := make([]string, 0, len(s.Advisories))
			for _, a := range s.Advisories {
				ids = append(ids, a.ID)
			}
			p += " (" + s.MaxSeverity + ": " + strings.Join(ids, ", ") + ")"
		}
		if s.Err != "" && s.Status == "unreachable" {
			p += " (" + s.Err + ")"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "; ")
}
