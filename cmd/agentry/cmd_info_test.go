// `agentry info` / `agentry search`: the info rendering distinguishes
// "nothing published" from "the registry could not answer" (the two demand
// opposite reactions), search forwards its facet filters to the wire, and
// ns/name queries delegate to info.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/termtext"
)

// infoFixture registers one agent with a published 1.0.0 for info tests.
func infoFixture(t *testing.T) *fakeRegistry {
	t.Helper()
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "does demo things").
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
		"acme/demo — autonomous, public",
		"summary: does demo things",
		"latest:  1.0.0 (sha256:",
		"network.egress: api.example.com",
		"install: agentry install acme/demo",
		"  git:     " + fr.url() + "/git/acme/demo.git\n",
		"  created: 2026-01-02\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("info output lacks %q:\n%s", want, stdout)
		}
	}

	// An agent with no summary has no summary line.
	fr.addAgent("acme", "plain", "autonomous", "").addVersion(t, "1.0.0", skillFiles("plain", ""))
	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "info", "acme/plain")
	if err != nil || strings.Contains(stdout, "summary:") || !strings.Contains(stdout, "acme/plain — autonomous, public\n  git:") {
		t.Errorf("info of an agent with no summary: %v\n%s", err, stdout)
	}
}

func TestInfoNoPublishedVersion(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "empty", "autonomous", "nothing yet") // no versions

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "info", "acme/empty")
	if err != nil {
		t.Fatalf("an agent with no versions is a fact, not a failure: %v", err)
	}
	if !strings.Contains(stdout, "latest:  no published version") || strings.Contains(stdout, "install:") {
		t.Errorf("info output = %s", stdout)
	}

	// A registry that says the agent is gone (410) says the same.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents/{ns}/{name}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "00000000-0000-0000-0000-000000000001", "namespace": "acme", "name": "yanked",
			"modes": []string{"autonomous"}, "summary": "s", "visibility": "public", "quarantineState": "none",
			"defaultBranch": "main", "gitHttpUrl": "http://x/git", "createdAt": "2026-01-02T03:04:05Z",
		})
	})
	mux.HandleFunc("GET /api/v1/resolve", func(w http.ResponseWriter, _ *http.Request) {
		writeProblem(w, http.StatusGone, "every version yanked", "")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stdout, _, err = runCLI(t, nil, "--registry", srv.URL, "info", "acme/yanked")
	if err != nil || !strings.Contains(stdout, "latest:  no published version") {
		t.Errorf("a gone agent: %v\n%s", err, stdout)
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
			"modes": []string{"autonomous"}, "summary": "s", "visibility": "public", "quarantineState": "none",
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

	// The same in JSON: the agent, the error, and no latest version.
	stdout, _, err = runCLI(t, nil, "--registry", srv.URL, "--json", "info", "acme/demo")
	if err == nil {
		t.Fatal("a 503 version lookup exited 0 under --json")
	}
	var doc map[string]json.RawMessage
	if jerr := json.Unmarshal([]byte(stdout), &doc); jerr != nil || doc["agent"] == nil || !strings.Contains(string(doc["latestError"]), "registry melting") {
		t.Errorf("--json of an outage: %v\n%s", jerr, stdout)
	}
	if _, ok := doc["latest"]; ok {
		t.Errorf("--json of an outage names a latest version: %s", doc["latest"])
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
	for _, ref := range []string{"demo", "acme/", "/demo"} {
		_, _, err := runCLI(t, nil, "--registry", "http://127.0.0.1:1", "info", ref)
		if err == nil || !strings.Contains(err.Error(), "info wants ns/name") {
			t.Errorf("%q accepted: %v", ref, err)
		}
	}
}

func TestSearchForwardsFacetsToTheWire(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	installs := 12
	fr.searchResults = []map[string]any{
		{"namespace": "acme", "name": "demo", "modes": []string{"autonomous"}, "summary": "does things", "installs30d": installs},
		{"namespace": "acme", "name": "quiet", "modes": []string{"autonomous"}, "summary": ""},
	}
	total := 40
	fr.searchTotal = &total

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "search", "report",
		"--mode", "autonomous", "--capability", "network", "--license", "MIT",
		"--verified", "--sort", "installs", "--limit", "2")
	if err != nil {
		t.Fatal(err)
	}

	// Every facet flag must reach the registry as a query parameter.
	if len(fr.searchQueries) != 1 {
		t.Fatalf("search calls = %d", len(fr.searchQueries))
	}
	q := fr.searchQueries[0]
	for param, want := range map[string]string{
		"q": "report", "mode": "autonomous", "capability": "network",
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
	// One result to a line: the name in 40 columns, the modes in 23, the
	// installs in 16, then the summary or its placeholder.
	sp := func(n int) string { return strings.Repeat(" ", n) }
	want := "acme/demo" + sp(31) + " autonomous" + sp(13) + " 12 installs/30d" + sp(1) + " does things\n" +
		"acme/quiet" + sp(30) + " autonomous" + sp(13) + " " + sp(16) + " -\n" +
		"\n2 of 40 results shown (--limit to see more)\n"
	if stdout != want {
		t.Errorf("results:\n%q\nwant\n%q", stdout, want)
	}

	// All of them shown is no cut.
	total = 2
	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "search", "report")
	if err != nil || strings.Contains(stdout, "results shown") {
		t.Errorf("a complete result carries a cut notice: %v\n%s", err, stdout)
	}

	// A mode that is not one is refused before the registry is asked.
	before := len(fr.searchQueries)
	if _, _, err := runCLI(t, nil, "--registry", fr.url(), "search", "report", "--mode", "bogus"); err == nil || err.Error() != `unknown --mode "bogus" (want autonomous or interactive)` || len(fr.searchQueries) != before {
		t.Errorf("an unknown mode: %v (%d request(s) sent)", err, len(fr.searchQueries)-before)
	}
}

func TestSearchEmptyResultNamesTheFilters(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "search", "nada", "--mode", "autonomous", "--verified")
	if err != nil {
		t.Fatal(err)
	}
	// "no agents match" with the filters spelled out, so an over-filtered
	// query never reads as an empty registry.
	if !strings.Contains(stdout, `no QuickAgents match "nada" with mode=autonomous, verified`) {
		t.Errorf("empty-result line = %q", stdout)
	}
	// A filter or limit that was not given is not sent.
	q := fr.searchQueries[0]
	for _, param := range []string{"capability", "license", "sort", "limit"} {
		if q.Has(param) {
			t.Errorf("the query carries %s though none was given: %v", param, q)
		}
	}
	// Nor is an empty query.
	if _, _, err := runCLI(t, nil, "--registry", fr.url(), "search", ""); err != nil {
		t.Fatal(err)
	}
	if fr.searchQueries[1].Has("q") {
		t.Errorf("an empty query was sent: %v", fr.searchQueries[1])
	}
}

func TestSearchJSONAndDelegation(t *testing.T) {
	isolateEnv(t)
	fr := infoFixture(t)
	fr.searchResults = []map[string]any{{"namespace": "acme", "name": "demo", "modes": []string{"autonomous"}, "summary": "s"}}

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
	if !strings.Contains(stdout, "acme/demo — autonomous, public") {
		t.Errorf("ns/name search did not delegate to info: %s", stdout)
	}
}

func TestDescribeFacets(t *testing.T) {
	if got := describeFacets(searchOpts{}); got != "" {
		t.Errorf("no facets = %q", got)
	}
	got := describeFacets(searchOpts{mode: "interactive", capability: "exec", license: "MIT", verified: true})
	if got != " with mode=interactive, capability=exec, license=MIT, verified" {
		t.Errorf("facets = %q", got)
	}
	for opts, want := range map[searchOpts]string{
		{mode: "autonomous"}: " with mode=autonomous",
		{capability: "exec"}: " with capability=exec",
		{license: "MIT"}:     " with license=MIT",
		{verified: true}:     " with verified",
		{sort: "installs"}:   "",
		{limit: 5}:           "",
	} {
		if got := describeFacets(opts); got != want {
			t.Errorf("describeFacets(%+v) = %q, want %q", opts, got, want)
		}
	}
}

// runFixtureCaps is a release that asks one question of each type, takes
// code and data as uploaded, declares keys and carries a guide.
const runFixtureCaps = `capabilities:
  secrets: [API_TOKEN, EXTRA_TOKEN, ACCESS]
  optionalSecrets: [EXTRA_TOKEN]
  targetHosts: [server]
  network:
    egress: [api.example.com]
intake:
  - id: repo
    prompt: The repository to audit
    type: repository
  - id: spec
    prompt: The spec
    type: url
    required: true
  - id: depth
    prompt: How deep
    type: choice
    choices: [quick, full]
  - id: notes
    prompt: Your notes
    type: file
  - id: access
    prompt: The access secret
    type: secret
  - id: server
    prompt: The server
    type: host
  - id: focus
    prompt: "What to focus on\u001b[2J"
inputs:
  kinds: [code, data]
  view: original
guide:
  overview: Audits a repository.
  prepare: [The repository address]
  files: Optional SBOMs.
  task: e.g. Audit everything.
`

func runFixture(t *testing.T) *fakeRegistry {
	t.Helper()
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "audit", "autonomous", "audits").
		addVersion(t, "1.0.0", skillFiles("audit", runFixtureCaps))
	return fr
}

// TestInfoShowsHowToRunIt: each question with how to answer it by its
// type, the files, the keys and the guide, with no control character of
// the publisher's reaching the terminal.
func TestInfoShowsHowToRunIt(t *testing.T) {
	isolateEnv(t)
	fr := runFixture(t)

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "info", "acme/audit")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"how to run it:",
		"questions (answer each with --intake id=value):",
		"repo (repository, optional): The repository to audit",
		"answer: The repository's https address; add #branch, #tag or #commit to pick one. Agentry fetches it for the run: nothing needs downloading.",
		"spec (url, required): The spec",
		"answer: One file's https address.",
		`answer: One of "quick", "full", as written.`,
		"answer: A path to a file or folder on this computer.",
		"access (secret, optional)",
		"answer: The person's own value",
		"server (host, optional)",
		"answer: The person's own target host",
		"focus (text, optional): What to focus on?[2J",
		"        answer: The text it asks for.\n",
		"        answer: The person's own target host, [user@]host[:port], which they set up and sign in to; never give it for them.\n",
		"        answer: One file's https address. Agentry fetches it for the run.\n",
		"Files it takes: code, data; read as uploaded. Give them with --file.",
		"keys: API_TOKEN, EXTRA_TOKEN (optional)",
		"guide (the publisher's words):",
		termtext.Gutter + "Audits a repository.",
		"have ready: The repository address",
		"files: Optional SBOMs.",
		"task: e.g. Audit everything.",
		"install: agentry install acme/audit",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("info output lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "\x1b") {
		t.Error("a publisher's control character reached the terminal")
	}
	if strings.Contains(stdout, "keys: API_TOKEN, EXTRA_TOKEN (optional), ACCESS") {
		t.Error("the secret a secret question carries is listed again as a key")
	}
}

func TestInfoShowsNoQuestions(t *testing.T) {
	isolateEnv(t)
	fr := infoFixture(t)

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "info", "acme/demo")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"questions: none", "Files it takes: any kind; read as text or as uploaded."} {
		if !strings.Contains(stdout, want) {
			t.Errorf("info output lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "guide (") || strings.Contains(stdout, "keys:") {
		t.Errorf("a release with no guide or keys shows one:\n%s", stdout)
	}
}

func TestInfoJSONCarriesHowToRunIt(t *testing.T) {
	isolateEnv(t)
	fr := runFixture(t)

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "--json", "info", "acme/audit")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Run *infoRun `json:"run"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, stdout)
	}
	r := doc.Run
	if r == nil {
		t.Fatalf("no run object:\n%s", stdout)
	}
	if len(r.Intake) != 7 {
		t.Fatalf("intake = %+v", r.Intake)
	}
	repo := r.Intake[0]
	if repo.ID != "repo" || repo.Type != "repository" || repo.Required || repo.PersonsOwn || !strings.Contains(repo.Answer, "#branch") {
		t.Errorf("repository question = %+v", repo)
	}
	if !r.Intake[1].Required || r.Intake[2].Choices[1] != "full" {
		t.Errorf("url or choice question = %+v %+v", r.Intake[1], r.Intake[2])
	}
	if !r.Intake[4].PersonsOwn || !r.Intake[5].PersonsOwn {
		t.Errorf("a secret or host question is not the person's own: %+v %+v", r.Intake[4], r.Intake[5])
	}
	if strings.Join(r.Takes, ",") != "code,data" || r.InputView != "original" {
		t.Errorf("takes = %v, view = %q", r.Takes, r.InputView)
	}
	if len(r.Secrets) != 2 || r.Secrets[0] != (infoSecret{Name: "API_TOKEN"}) || r.Secrets[1] != (infoSecret{Name: "EXTRA_TOKEN", Optional: true}) {
		t.Errorf("secrets = %+v", r.Secrets)
	}
	if strings.Join(r.Modes, ",") != "autonomous" {
		t.Errorf("modes = %v", r.Modes)
	}
	if r.Guide == nil || r.Guide.Overview != "Audits a repository." || r.Guide.Prepare[0] != "The repository address" || r.Guide.Files != "Optional SBOMs." || r.Guide.Task != "e.g. Audit everything." {
		t.Errorf("guide = %+v", r.Guide)
	}
}

// TestInfoJSONHasNoRunWithoutAVersion: run describes the latest version,
// so an agent with nothing published carries none.
func TestInfoJSONHasNoRunWithoutAVersion(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "empty", "autonomous", "nothing yet")

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "--json", "info", "acme/empty")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, `"run"`) {
		t.Errorf("an agent with no version carries a run object:\n%s", stdout)
	}
	// It says outright that there is no latest version.
	if !strings.Contains(stdout, `"latest": null`) {
		t.Errorf("the document does not say the latest version is none:\n%s", stdout)
	}
}

// TestASiteQuestionIsAnsweredWithAnAddressToOpen: a url question the
// browser opens is a site to visit, never a file to fetch.
func TestASiteQuestionIsAnsweredWithAnAddressToOpen(t *testing.T) {
	m := &manifest.Manifest{
		Capabilities: manifest.Capabilities{Browser: &manifest.Browser{Sites: []string{"site"}}},
		Intake:       []manifest.IntakeQuestion{{ID: "site", Prompt: "The site", Type: manifest.IntakeURL}},
	}
	if got := howToRun(m).Intake[0].Answer; !strings.Contains(got, "browser opens") {
		t.Errorf("site answer = %q", got)
	}
}

func TestInfoCutsALongGuideLine(t *testing.T) {
	m := &manifest.Manifest{Guide: &manifest.Guide{Task: strings.Repeat("x", maxInfoLine+50)}}
	got := howToRun(m).Guide.Task
	if len([]rune(got)) != maxInfoLine+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("task cut to %d runes", len([]rune(got)))
	}

	// The bounds are 400 characters for a line and 2,048 for the overview: a
	// text of exactly the bound is kept whole, one character more is cut.
	line := func(n int) string { return strings.Repeat("é", n) }
	for _, tc := range []struct {
		name         string
		in           string
		bound        int
		get          func(*infoRun) string
		wantRunes    int
		wantEllipsis bool
	}{
		{"task at the bound", line(400), 400, func(r *infoRun) string { return r.Guide.Task }, 400, false},
		{"task past the bound", line(401), 400, func(r *infoRun) string { return r.Guide.Task }, 401, true},
		{"files at the bound", line(400), 400, func(r *infoRun) string { return r.Guide.Files }, 400, false},
		{"files past the bound", line(401), 400, func(r *infoRun) string { return r.Guide.Files }, 401, true},
		{"overview at the bound", line(2048), 2048, func(r *infoRun) string { return r.Guide.Overview }, 2048, false},
		{"overview past the bound", line(2049), 2048, func(r *infoRun) string { return r.Guide.Overview }, 2049, true},
	} {
		g := &manifest.Guide{}
		switch {
		case strings.HasPrefix(tc.name, "task"):
			g.Task = tc.in
		case strings.HasPrefix(tc.name, "files"):
			g.Files = tc.in
		default:
			g.Overview = tc.in
		}
		got := tc.get(howToRun(&manifest.Manifest{Guide: g}))
		if len([]rune(got)) != tc.wantRunes || strings.HasSuffix(got, "…") != tc.wantEllipsis {
			t.Errorf("%s: %d runes (cut %v), want %d (cut %v)", tc.name, len([]rune(got)), strings.HasSuffix(got, "…"), tc.wantRunes, tc.wantEllipsis)
		}
	}

	// A line of the guide is trimmed, an item to have ready that is blank is
	// left out and one that is long is cut; a guide of nothing is no guide.
	r := howToRun(&manifest.Manifest{Guide: &manifest.Guide{
		Overview: "  \n Reads it.\n ", Prepare: []string{" ", "", line(401), " the address "},
	}})
	if r.Guide == nil || r.Guide.Overview != "Reads it." || !slices.Equal(r.Guide.Prepare, []string{line(400) + "…", "the address"}) {
		t.Errorf("guide = %+v", r.Guide)
	}
	if r := howToRun(&manifest.Manifest{Guide: &manifest.Guide{Overview: "  ", Prepare: []string{" "}, Files: "\n"}}); r.Guide != nil {
		t.Errorf("a blank guide is reported: %+v", r.Guide)
	}

	// A question's prompt is trimmed and cut at the same bound.
	r = howToRun(&manifest.Manifest{Intake: []manifest.IntakeQuestion{
		{ID: "a", Prompt: "  What?  "}, {ID: "b", Prompt: line(401)}, {ID: "c", Prompt: line(400)},
	}})
	if r.Intake[0].Prompt != "What?" || r.Intake[1].Prompt != line(400)+"…" || r.Intake[2].Prompt != line(400) {
		t.Errorf("prompts = %q %d %d", r.Intake[0].Prompt, len([]rune(r.Intake[1].Prompt)), len([]rune(r.Intake[2].Prompt)))
	}

	// A key is listed once, whichever of the release and its servers declare
	// it, and the key a secret question carries is not listed at all.
	r = howToRun(&manifest.Manifest{
		Capabilities: manifest.Capabilities{
			Secrets:    []string{"A", "B", "ASKS"},
			MCPServers: []manifest.MCPServer{{NPM: "x", Version: "1.0.0", Secrets: []string{"A", "C"}}},
		},
		Intake: []manifest.IntakeQuestion{{ID: "asks", Prompt: "p", Type: manifest.IntakeSecret}},
	})
	var keys []string
	for _, s := range r.Secrets {
		keys = append(keys, s.Name)
	}
	if !slices.Equal(keys, []string{"A", "B", "C"}) {
		t.Errorf("keys = %v, want A B C", keys)
	}

	// A release that runs only as an interactive session says so.
	var buf strings.Builder
	only := &manifest.Manifest{Modes: []manifest.Mode{manifest.ModeInteractive}}
	renderHowToRun(&buf, only, howToRun(only))
	if !strings.Contains(buf.String(), "    it runs only as an interactive session\n") {
		t.Errorf("an interactive-only release: %q", buf.String())
	}
	buf.Reset()
	both := &manifest.Manifest{Modes: []manifest.Mode{manifest.ModeAutonomous, manifest.ModeInteractive}}
	renderHowToRun(&buf, both, howToRun(both))
	if strings.Contains(buf.String(), "interactive session") {
		t.Errorf("a release that runs on its own is said to be interactive only: %q", buf.String())
	}
}
