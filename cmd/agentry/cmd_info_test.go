// `agentry info` / `agentry search` (§11, §10.4): the info rendering
// distinguishes "nothing published" from "the registry could not answer"
// (the two demand opposite reactions), search forwards its facet filters to
// the wire, and ns/name queries delegate to info.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// infoFixture registers one agent with a published 1.0.0 for info tests.
func infoFixture(t *testing.T) *fakeRegistry {
	t.Helper()
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "skill", "does demo things").
		addVersion(t, "1.0.0", skillFiles("demo", "capabilities:\n  network:\n    egress: [api.example.com]\n"))
	return fr
}

func TestInfoHappyPath(t *testing.T) {
	isolateEnv(t)
	fr := infoFixture(t)

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "info", "acme/demo")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"acme/demo — skill, public",
		"summary: does demo things",
		"latest:  1.0.0 (sha256:",
		"network.egress: api.example.com",
		"install: agentry install acme/demo",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("info output lacks %q:\n%s", want, stdout)
		}
	}
}

func TestInfoNoPublishedVersion(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "empty", "skill", "nothing yet") // no versions

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "info", "acme/empty")
	if err != nil {
		t.Fatalf("an agent with no versions is a fact, not a failure: %v", err)
	}
	if !strings.Contains(stdout, "latest:  no published version") {
		t.Errorf("info output = %s", stdout)
	}
}

// TestInfoTransportFailureIsNonZero: a 5xx during the version lookup must
// exit non-zero AND say UNKNOWN — printing "no published version" for an
// outage is the bug errNoPublishedVersion exists to prevent.
func TestInfoTransportFailureIsNonZero(t *testing.T) {
	isolateEnv(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents/{ns}/{name}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "00000000-0000-0000-0000-000000000001", "namespace": "acme", "name": "demo",
			"kind": "skill", "summary": "s", "visibility": "public", "quarantineState": "none",
			"defaultBranch": "main", "gitHttpUrl": "http://x/git", "createdAt": "2026-01-02T03:04:05Z",
		})
	})
	mux.HandleFunc("GET /api/v1/resolve", func(w http.ResponseWriter, _ *http.Request) {
		writeProblem(w, http.StatusServiceUnavailable, "registry melting", "")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	stdout, _, err := runCLI(t, nil, "--registry", srv.URL, "info", "acme/demo")
	if err == nil {
		t.Fatal("a 503 version lookup exited 0")
	}
	if !strings.Contains(stdout, "latest:  UNKNOWN — the registry could not answer") {
		t.Errorf("info output = %s", stdout)
	}
	if strings.Contains(stdout, "no published version") {
		t.Error("an outage was reported as an empty agent")
	}
}

func TestInfoJSONShape(t *testing.T) {
	isolateEnv(t)
	fr := infoFixture(t)

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "--json", "info", "acme/demo")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Agent  map[string]any `json:"agent"`
		Latest map[string]any `json:"latest"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, stdout)
	}
	if doc.Agent["name"] != "demo" || doc.Latest["version"] != "1.0.0" {
		t.Errorf("json doc = %+v", doc)
	}
}

func TestInfoRejectsBareName(t *testing.T) {
	isolateEnv(t)
	_, _, err := runCLI(t, nil, "--registry", "http://127.0.0.1:1", "info", "demo")
	if err == nil || !strings.Contains(err.Error(), "info wants ns/name") {
		t.Errorf("bare name accepted: %v", err)
	}
}

func TestSearchForwardsFacetsToTheWire(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	installs := 12
	fr.searchResults = []map[string]any{
		{"namespace": "acme", "name": "demo", "kind": "skill", "summary": "does things", "installs30d": installs},
		{"namespace": "acme", "name": "quiet", "kind": "skill", "summary": ""},
	}
	total := 40
	fr.searchTotal = &total

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "search", "report",
		"--kind", "skill", "--capability", "network", "--license", "MIT",
		"--verified", "--sort", "installs", "--limit", "2")
	if err != nil {
		t.Fatal(err)
	}

	// Every facet flag must reach the registry as a §10.4 query parameter.
	if len(fr.searchQueries) != 1 {
		t.Fatalf("search calls = %d", len(fr.searchQueries))
	}
	q := fr.searchQueries[0]
	for param, want := range map[string]string{
		"q": "report", "kind": "skill", "capability": "network",
		"license": "MIT", "verified": "true", "sort": "installs", "limit": "2",
	} {
		if got := q.Get(param); got != want {
			t.Errorf("query %s = %q, want %q", param, got, want)
		}
	}

	if !strings.Contains(stdout, "acme/demo") || !strings.Contains(stdout, "12 installs/30d") {
		t.Errorf("results render = %s", stdout)
	}
	// A blank summary renders a placeholder, and the truncation footer
	// points at --limit.
	if !strings.Contains(stdout, "-") || !strings.Contains(stdout, "2 of 40 results shown (--limit to see more)") {
		t.Errorf("results footer = %s", stdout)
	}
}

func TestSearchEmptyResultNamesTheFilters(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "search", "nada", "--kind", "skill", "--verified")
	if err != nil {
		t.Fatal(err)
	}
	// "no agents match" with the filters spelled out, so an over-filtered
	// query never reads as an empty registry.
	if !strings.Contains(stdout, `no BriefAgents match "nada" with kind=skill, verified`) {
		t.Errorf("empty-result line = %q", stdout)
	}
}

func TestSearchJSONAndDelegation(t *testing.T) {
	isolateEnv(t)
	fr := infoFixture(t)
	fr.searchResults = []map[string]any{{"namespace": "acme", "name": "demo", "kind": "skill", "summary": "s"}}

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "--json", "search", "demo")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil || len(doc.Results) != 1 {
		t.Errorf("--json search = %v, %v", doc, err)
	}

	// A query containing "/" is a ref, not a search: it renders info.
	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "search", "acme/demo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "acme/demo — skill, public") {
		t.Errorf("ns/name search did not delegate to info: %s", stdout)
	}
}

func TestDescribeFacets(t *testing.T) {
	if got := describeFacets(searchOpts{}); got != "" {
		t.Errorf("no facets = %q", got)
	}
	got := describeFacets(searchOpts{kind: "skill", capability: "exec", license: "MIT", verified: true})
	if got != " with kind=skill, capability=exec, license=MIT, verified" {
		t.Errorf("facets = %q", got)
	}
}
