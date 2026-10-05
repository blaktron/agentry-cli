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
// registry (a stale laptop must still run) and closed on an advisory the
// registry's gate rates high or critical (unless the operator overrides
// it), and either way the receipt says which.
//
// The rating is the gate's, not the advisory's word (agentry-cli#501,
// amending DR-16): since ruleset 0.3.1 the registry rates a High advisory
// medium below CVSS 8.5 and publishes the version, so the runner refusing
// on the database's "high" would refuse what the gate let through. A
// registry that does not serve the gate's rating (an older or a
// self-hosted one) is weighed by the advisory's own severity, as before.

// VulnAdvisory is one advisory the registry reports for a server version.
type VulnAdvisory struct {
	ID      string   `json:"id"`
	Aliases []string `json:"aliases,omitempty"`
	// Severity is the advisory's own word (the database's, else the band
	// of its CVSS score).
	Severity string `json:"severity"`
	// GateSeverity is the registry gate's rating of the finding this
	// advisory makes (agentry#774): Critical is critical, High is high
	// from a CVSS base score of 8.5 (or with no usable score) and medium
	// below it, any other its own. Empty from a registry that does not
	// serve it.
	GateSeverity string `json:"gateSeverity,omitempty"`
	// CVSSScore is the advisory's highest CVSS base score, when the
	// registry serves one.
	CVSSScore *float64 `json:"cvssScore,omitempty"`
	Summary   string   `json:"summary"`
	FixedIn   []string `json:"fixedIn,omitempty"`
}

// Rating is the severity the runner weighs this advisory at: the gate's
// rating where the registry serves one the runner knows, else the
// advisory's own. A Critical is critical whatever was served: the gate
// never rates one lower, so a lower answer is a registry fault the runner
// does not inherit.
func (a VulnAdvisory) Rating() string {
	switch {
	case a.Severity == "critical":
		return "critical"
	case a.gateRated():
		return a.GateSeverity
	}
	return a.Severity
}

// gateRated reports whether the registry served a gate rating the runner
// knows; an unknown word is weighed as no rating at all.
func (a VulnAdvisory) gateRated() bool { return severityRank[a.GateSeverity] > 0 }

// Refuses reports whether this advisory refuses a run by default: one
// rated high or critical.
func (a VulnAdvisory) Refuses() bool {
	r := a.Rating()
	return r == "high" || r == "critical"
}

// Label names the advisory for a person: its id and, where the registry
// serves the gate's rating, the advisory's own severity and CVSS score,
// and the gate's rating where it differs, so a High the gate rates medium
// reads as both.
func (a VulnAdvisory) Label() string {
	if !a.gateRated() {
		return a.ID
	}
	l := a.ID + " [advisory " + a.Severity
	if a.CVSSScore != nil {
		l += fmt.Sprintf(", CVSS %.1f", *a.CVSSScore)
	}
	if r := a.Rating(); r != a.Severity {
		l += "; the gate rates it " + r
	}
	return l + "]"
}

var severityRank = map[string]int{"info": 1, "low": 2, "medium": 3, "high": 4, "critical": 5}

// VulnStatus is the registry's answer for one declared server.
type VulnStatus struct {
	// Ref is "npm:pkg@1.2.3" / "pypi:pkg@1.2.3".
	Ref string `json:"ref"`
	// Server is the declared server's id without its version
	// ("npm:pkg"), the name a refusal's reason gives (agentry-cli#628).
	Server string `json:"-"`
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

// Refusing is the advisories that refuse a run by default: those rated
// high or critical, on a record that lists advisories ("vulnerable", or
// "error" with the last known).
func (s VulnStatus) Refusing() []VulnAdvisory {
	if s.Status != "vulnerable" && s.Status != "error" {
		return nil
	}
	var out []VulnAdvisory
	for _, a := range s.Advisories {
		if a.Refuses() {
			out = append(out, a)
		}
	}
	return out
}

// Refuses reports whether this status refuses a run by default.
func (s VulnStatus) Refuses() bool { return len(s.Refusing()) > 0 }

// gateRated reports whether the registry served the gate's rating for
// this record's advisories.
func (s VulnStatus) gateRated() bool {
	for _, a := range s.Advisories {
		if a.gateRated() {
			return true
		}
	}
	return false
}

// Describe is the clause a refusal, doctor and the receipt name advisories
// with: "a known critical vulnerability (GHSA-…)" from a registry that
// serves no rating, "an advisory the gate rates high (GHSA-… [advisory
// high, CVSS 8.7])" from one that does.
func (s VulnStatus) Describe(advs []VulnAdvisory) string {
	labels := make([]string, 0, len(advs))
	for _, a := range advs {
		labels = append(labels, a.Label())
	}
	worst := worstRating(advs)
	list := " (" + strings.Join(labels, ", ") + ")"
	switch {
	case !s.gateRated() && len(advs) == 1:
		return "a known " + worst + " vulnerability" + list
	case !s.gateRated():
		return "known vulnerabilities, the worst " + worst + list
	case len(advs) == 1:
		return "an advisory the gate rates " + worst + list
	}
	return "advisories the gate rates up to " + worst + list
}

// FixedIn is the union of the fixed versions the refusing advisories name.
func (s VulnStatus) FixedIn() []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range s.Refusing() {
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
		st := VulnStatus{Ref: srv.SourceID() + "@" + srv.Version, Server: srv.SourceID()}
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

func worstRating(advs []VulnAdvisory) string {
	worst := ""
	for _, a := range advs {
		if severityRank[a.Rating()] > severityRank[worst] {
			worst = a.Rating()
		}
	}
	return worst
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
			labels := make([]string, 0, len(s.Advisories))
			for _, a := range s.Advisories {
				labels = append(labels, a.Label())
			}
			rating := s.MaxSeverity
			if s.gateRated() {
				rating = "gate-rated " + worstRating(s.Advisories)
			}
			p += " (" + rating + ": " + strings.Join(labels, ", ") + ")"
		}
		if s.Err != "" && s.Status == "unreachable" {
			p += " (" + s.Err + ")"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "; ")
}
