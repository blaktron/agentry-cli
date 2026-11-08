package drafter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
	"github.com/blaktron/agentry/pkg/manifest"
	"github.com/blaktron/agentry/pkg/toolgrant"
)

// persistentManifest is a persistent agent's manifest as a draft declares
// it: the keys, the persona and the subagents are the arguments, "" leaving
// one out.
func persistentManifest(keys, persona, agents, include string) json.RawMessage {
	caps := `"network": {"egress": "none"}, "filesystem": {"read": "none", "write": "none"}, "exec": "none"`
	if keys != "" {
		caps += `, "persistent": ` + keys
	}
	extra := ""
	if persona != "" {
		extra += `, "persona": ` + persona
	}
	if agents != "" {
		extra += `, "dependencies": {"agents": ` + agents + `}`
	}
	if include == "" {
		include = `["agent.yaml", "AGENT.md", "README.md", "LICENSE"]`
	}
	return json.RawMessage(`{"schemaVersion": "2", "name": "pip-notes-keeper", "namespace": "draft", "version": "0.1.0",` +
		` "summary": "Keeps notes and reminds of tasks.", "license": "Apache-2.0", "modes": ["persistent"],` +
		` "entrypoints": {"instruction": "AGENT.md"}, "capabilities": {` + caps + `}` + extra +
		`, "publish": {"include": ` + include + `}}`)
}

const (
	pipInstructions = "# Pip\n\n## Role\n\nKeeps the person's notes.\n\n## Inputs\n\nWhat the person says.\n\n## Steps\n\n" +
		"1. Keep a note with the date, and find it again with recall.\n2. Add a reminder with task_add and list them with task_list.\n\n" +
		"## Output\n\nA short reply.\n\n## Background\n\nA task has a what and a when.\n\n## Checks\n\nKeep nothing unasked.\n"
	pipAgents  = `[{"ref": "acme/pdf-summary", "version": "^1.4.0", "why": "Summarises the PDFs you put in the share folder."}]`
	pipPersona = "# Pip\n\nBrisk and plain. Greets the person by name and never pretends to be one.\n"
)

// TestAPersistentAgentsToolsNeedTheirKeys: the rules read a persistent
// agent's instructions with its own tools, so a tool of a key the manifest
// lacks is a line for the repair turn that says to add the key or take the
// tool out, and the subagent tools without a subagent say so; with the keys
// declared, and for a release that is not persistent, there is none.
func TestAPersistentAgentsToolsNeedTheirKeys(t *testing.T) {
	d := &Draft{Validation: json.RawMessage(`{"passed":true}`), Manifest: persistentManifest(`{"memory": true}`, "", "", ""), Instructions: pipInstructions}
	lines, refused := rulesReport(d)
	if !refused || len(lines) != 1 {
		t.Fatalf("the rules' report: %q, %v; want one line on the clock", lines, refused)
	}
	for _, want := range []string{"task_add (line 14), task_list (line 14) need capabilities.persistent.clock", "Add capabilities.persistent.clock",
		"manifest/persistent.md", "or take the tools that need it out of the instructions"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the line %q does not say %q", lines[0], want)
		}
	}
	if strings.Contains(lines[0], "recall") {
		t.Errorf("the line %q names recall, whose key is declared", lines[0])
	}

	d.Manifest = persistentManifest(`{"memory": true, "clock": true}`, "", "", "")
	if lines, refused := rulesReport(d); refused || len(lines) != 0 {
		t.Errorf("with both keys declared, the rules' report: %q, %v", lines, refused)
	}

	// The subagent tools need a subagent; the repair turn has no registry
	// search, so the line says to take them out unless one is in the draft.
	d.Instructions += "\nStart the summariser with subagent_run and read it with subagent_result.\n"
	lines, _ = rulesReport(d)
	if len(lines) != 1 || !strings.Contains(lines[0], "need dependencies.agents") || !strings.Contains(lines[0], "otherwise take the subagent tools out") {
		t.Fatalf("the rules' report: %q", lines)
	}
	d.Manifest = persistentManifest(`{"memory": true, "clock": true}`, "", pipAgents, "")
	if lines, refused := rulesReport(d); refused || len(lines) != 0 {
		t.Errorf("with a subagent declared, the rules' report: %q, %v", lines, refused)
	}

	// The same words in a release that is not persistent are words.
	ordinary := &Draft{Validation: json.RawMessage(`{"passed":true}`), Instructions: pipInstructions,
		Manifest: json.RawMessage(`{"schemaVersion": "2", "name": "pip", "modes": ["autonomous"], "capabilities": {"network": {"egress": "none"}}}`)}
	if lines, refused := rulesReport(ordinary); refused || len(lines) != 0 {
		t.Errorf("an autonomous release's rules' report: %q, %v", lines, refused)
	}
}

// TestThePersistentPageNamesTheCoresKeysAndTools holds the guide's page on
// persistent agents to the core: every key of capabilities.persistent is in
// its table, each key's tools are the core's table of that grant, the
// subagent tools are named, and the bounds are the rules'.
func TestThePersistentPageNamesTheCoresKeysAndTools(t *testing.T) {
	text := string(guideFile(t, "manifest/persistent.md"))
	rows := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if cells := strings.Split(line, "|"); len(cells) == 5 && strings.HasPrefix(strings.TrimSpace(cells[1]), "`") {
			rows[strings.Trim(strings.TrimSpace(cells[1]), "`")] = cells[3]
		}
	}
	var keys []string
	for _, c := range manifest.PersistentCapabilities() {
		keys = append(keys, c.Key)
		tools, ok := rows[c.Key]
		if !ok {
			t.Errorf("the page has no row for the key %s", c.Key)
			continue
		}
		var named []string
		for _, m := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(tools, -1) {
			named = append(named, m[1])
		}
		slices.Sort(named)
		want := toolgrant.ToolsOf(toolgrant.Grant("persistent." + c.Key))
		if c.Key == "coder" {
			// The coding toolchain holds the grants of exec, fetch, a browser
			// and target hosts rather than tools of its own.
			want = nil
			named = nil
		}
		if !slices.Equal(named, want) {
			t.Errorf("the page gives %s the tools %q, the core's table %q", c.Key, named, want)
		}
	}
	if len(rows) != len(keys) {
		t.Errorf("the page's table has %d rows, the core has %d keys", len(rows), len(keys))
	}
	held := strings.Join(strings.Fields(text), " ")
	for _, tool := range toolgrant.ToolsOf(toolgrant.GrantSubagents) {
		if !strings.Contains(held, "`"+tool+"`") {
			t.Errorf("the page does not name %s", tool)
		}
	}
	mustSay(t, map[string][]string{
		"manifest/persistent.md": {
			fmt.Sprintf("at most %d bytes", manifest.MaxPersonaNameBytes),
			fmt.Sprintf("at most %d KiB", manifest.MaxPersonaFileBytes>>10),
			fmt.Sprintf("At most %d subagents", manifest.MaxSubagents),
			fmt.Sprintf("at most %d characters", manifest.MaxSubagentWhyChars),
			fmt.Sprintf("are at most %d environment variable names", manifest.MaxPersistentSecrets),
			"Propose a subagent; never add one silently.",
		},
		"manifest/manifest.md": {
			fmt.Sprintf("`persona.name`: one plain line of at most %d bytes", manifest.MaxPersonaNameBytes),
			fmt.Sprintf("`dependencies.agents`: at most %d subagents", manifest.MaxSubagents),
			fmt.Sprintf("at most %d characters", manifest.MaxSubagentWhyChars),
			fmt.Sprintf("`secrets` at most %d environment variable names", manifest.MaxPersistentSecrets),
		},
		instructionFile: {
			"## A persistent agent", "`modes: [\"persistent\"]`", "**Read `manifest/persistent.md` first.**",
			"**A subagent you did not find and read is never declared**", "`write_report`, `path` `PERSONA.md`",
		},
	})
}

// TestAPersistentDraftRendersItsPersona: the persona's text is written
// beside the instruction under the file the manifest names, passes the
// registry's rules with the file matched by publish.include, and is not
// written for a manifest without a persona or to a path outside the
// package's root.
func TestAPersistentDraftRendersItsPersona(t *testing.T) {
	d := &Draft{Manifest: persistentManifest(`{"memory": true, "clock": true}`, `{"name": "Pip"}`, pipAgents,
		`["agent.yaml", "AGENT.md", "PERSONA.md", "README.md", "LICENSE"]`), Instructions: pipInstructions, PersonaText: pipPersona}
	dir := filepath.Join(t.TempDir(), "package")
	if _, err := Render(d, dir); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "PERSONA.md")); err != nil || string(b) != pipPersona {
		t.Fatalf("PERSONA.md: %q, %v", b, err)
	}
	p := PostStep{OutDir: t.TempDir()}
	d.ManifestError, d.Validation = p.renderAndValidate(d)
	if !d.Passed() {
		t.Fatalf("the persistent package does not pass: %q %s", d.ManifestError, d.Validation)
	}

	// A text with no persona in the manifest has no file to go in.
	d.Manifest = persistentManifest(`{"memory": true}`, "", "", "")
	dir = filepath.Join(t.TempDir(), "package")
	if _, err := Render(d, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "PERSONA.md")); err == nil {
		t.Error("a persona text was written for a manifest with no persona")
	}

	for _, file := range []string{"../PERSONA.md", "sub/PERSONA.md", "AGENT.md", "README.md", "PERSONA.sh"} {
		d.Manifest = persistentManifest(`{"memory": true}`, `{"name": "Pip", "file": "`+file+`"}`, "", "")
		out := filepath.Join(t.TempDir(), "package")
		if _, err := Render(d, out); err == nil {
			t.Errorf("a persona file %q was rendered", file)
		}
		if _, err := os.Stat(out); err == nil {
			t.Errorf("a package was written for the persona file %q", file)
		}
	}
}

// TestThePersonaIsPartOfThePackageAChecksReads: the check of the finished
// draft is sent the persona's file with the instruction.
func TestThePersonaIsPartOfThePackageAChecksReads(t *testing.T) {
	d := &Draft{Manifest: persistentManifest(`{"memory": true}`, `{"name": "Pip"}`, "", `["agent.yaml", "AGENT.md", "PERSONA.md", "README.md", "LICENSE"]`),
		Instructions: pipInstructions, PersonaText: pipPersona}
	dir := filepath.Join(t.TempDir(), "package")
	if _, err := Render(d, dir); err != nil {
		t.Fatal(err)
	}
	pk, err := CheckPackage("An assistant named Pip.", dir)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range pk.Files {
		paths = append(paths, f.Path)
	}
	if !slices.Contains(paths, "PERSONA.md") {
		t.Errorf("the check is sent %v, without the persona", paths)
	}
}

// TestCorrectIncludesThePersonasFile: a persona the include list does not
// match gets its file added, the default one or the one it names; a list
// that matches it already, by a pattern, is left alone; a manifest with no
// persona is left alone.
func TestCorrectIncludesThePersonasFile(t *testing.T) {
	for _, tc := range []struct {
		name, persona, include string
		added                  string
	}{
		{"the default file", `{"name": "Pip"}`, `["agent.yaml", "AGENT.md", "README.md", "LICENSE"]`, "PERSONA.md"},
		{"a named file", `{"name": "Pip", "file": "pip.md"}`, `["agent.yaml", "AGENT.md", "README.md", "LICENSE"]`, "pip.md"},
		{"matched by a pattern", `{"name": "Pip"}`, `["agent.yaml", "AGENT.md", "*.md", "LICENSE"]`, ""},
		{"matched exactly", `{"name": "Pip"}`, `["agent.yaml", "AGENT.md", "PERSONA.md"]`, ""},
		{"no persona", ``, `["agent.yaml", "AGENT.md"]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, did := Correct(persistentManifest(`{"memory": true}`, tc.persona, "", tc.include))
			var m struct {
				Publish struct {
					Include []string `json:"include"`
				} `json:"publish"`
			}
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatal(err)
			}
			if tc.added == "" {
				if len(did) != 0 {
					t.Fatalf("corrected %q", did)
				}
				return
			}
			if got := m.Publish.Include[len(m.Publish.Include)-1]; got != tc.added || len(did) != 1 || !strings.Contains(did[0], "publish.include: "+tc.added+" was added") {
				t.Fatalf("include %v, corrections %q; want %s added", m.Publish.Include, did, tc.added)
			}
		})
	}
}

// TestAPersistentRepairIsGivenTheRulesReportAndThePersona runs the post-step
// over a persistent draft whose instructions call task_add with no clock: the
// repair turn is told how to fix it, is given the persona's text and the
// persistent framing, and an ordinary draft's repair is given neither. The
// second answer, which adds the key, is rendered with the persona and passes,
// and the notes list the subagent.
func TestAPersistentRepairIsGivenTheRulesReportAndThePersona(t *testing.T) {
	out := t.TempDir()
	mf := string(persistentManifest(`{"memory": true}`, `{"name": "Pip"}`, pipAgents, `["agent.yaml", "AGENT.md", "PERSONA.md", "README.md", "LICENSE"]`))
	fixed := string(persistentManifest(`{"memory": true, "clock": true}`, `{"name": "Pip"}`, pipAgents, `["agent.yaml", "AGENT.md", "PERSONA.md", "README.md", "LICENSE"]`))
	first, err := json.Marshal(map[string]any{"manifest": json.RawMessage(mf), "instructions": pipInstructions})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(out, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "reports", personaReport), []byte(pipPersona), 0o644); err != nil { //nolint:gosec // a test's own folder
		t.Fatal(err)
	}
	var task string
	p := PostStep{
		OutDir: out, Description: "An assistant named Pip.", Task: "An assistant named Pip.", Scan: webscan.Result{Verdict: webscan.Pass},
		Ref: "agentry/drafter@1.21.0", Build: "test", Budgets: spec.Budgets{TimeoutSeconds: 900, MaxTurns: 60}, Checker: &postChecker{},
		Repair: func(_ context.Context, rt RepairTurn) (*receipt.Envelope, error) {
			task = rt.Task
			return &receipt.Envelope{Status: "ok", RunID: rt.RunID, Result: json.RawMessage(`{"manifest": ` + fixed + `}`)}, nil
		},
	}
	d, _, err := p.Run(context.Background(), firstTurn(t, out, string(first)), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"need capabilities.persistent.clock", "This draft is a persistent agent:", "`path` `PERSONA.md`",
		"## Your persona (PERSONA.md)\n\n" + pipPersona} {
		if !strings.Contains(task, want) {
			t.Errorf("the repair task lacks %q:\n%s", want, task)
		}
	}
	if !d.IsRepaired() || !d.Passed() {
		t.Fatalf("the draft: repaired %v, passed %v, %q %s", d.IsRepaired(), d.Passed(), d.ManifestError, d.Validation)
	}
	if d.PersonaText != pipPersona {
		t.Errorf("the persona text %q", d.PersonaText)
	}
	if _, err := os.Stat(filepath.Join(out, "package", "PERSONA.md")); err != nil {
		t.Error("the repaired package has no PERSONA.md")
	}
	for _, want := range []string{subagentsHeading, "`acme/pdf-summary` at `^1.4.0`: Summarises the PDFs you put in the share folder."} {
		if !strings.Contains(d.Notes, want) {
			t.Errorf("the notes lack %q:\n%s", want, d.Notes)
		}
	}

	// An ordinary draft's repair task carries nothing of this.
	plain, err := RepairTask(&Draft{Manifest: websiteTestManifest(""), Instructions: "# X\n"}, RepairReport{Rules: []string{"the instructions: shorten them"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "persistent") || strings.Contains(plain, "PERSONA") {
		t.Errorf("an ordinary repair task mentions persistent agents:\n%s", plain)
	}
}

// TestARepairReadsTheRewrittenPersonaBack: the repair turn's PERSONA.md
// report replaces the persona, and a turn that wrote none keeps it.
func TestARepairReadsTheRewrittenPersonaBack(t *testing.T) {
	first := &Draft{Manifest: persistentManifest(`{"memory": true}`, `{"name": "Pip"}`, "", ""), Instructions: pipInstructions, PersonaText: pipPersona}
	r := RepairReport{Rules: []string{"V17 persona.file: the persona's file is not in the repo"}}
	answer := &receipt.Envelope{Result: json.RawMessage(`{"manifest": ` + string(first.Manifest) + `}`)}

	dir := t.TempDir()
	d, err := Repaired(first, answer, dir, r)
	if err != nil || d.PersonaText != pipPersona {
		t.Fatalf("a turn that wrote no persona: %q, %v", d.PersonaText, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reports", personaReport), []byte("# Pip\n\nA new voice.\n"), 0o644); err != nil { //nolint:gosec // a test's own folder
		t.Fatal(err)
	}
	if d, err = Repaired(first, answer, dir, r); err != nil || d.PersonaText != "# Pip\n\nA new voice.\n" {
		t.Fatalf("a turn that wrote the persona: %q, %v", d.PersonaText, err)
	}
}

// TestARepairNeverDropsAPersistentAgentsReachUnnamed: the mode, a key, the
// persona and a subagent are what the task needs, so a second answer that
// drops one the report did not name is not kept; one the report named by its
// path may be.
func TestARepairNeverDropsAPersistentAgentsReachUnnamed(t *testing.T) {
	first := &Draft{Manifest: persistentManifest(`{"memory": true, "clock": true}`, `{"name": "Pip"}`, pipAgents, "")}
	bare := &Draft{Manifest: json.RawMessage(`{"schemaVersion": "2", "name": "pip-notes-keeper", "modes": ["autonomous"], "capabilities": {"network": {"egress": "none"}}}`)}
	other := RepairReport{Rules: []string{"the instructions: shorten them"}}
	got := droppedUnnamed(first, bare, other)
	for _, want := range []string{"the run mode persistent", "the persistent capability memory", "the persistent capability clock", "the persona Pip", "the subagent acme/pdf-summary"} {
		if !slices.Contains(got, want) {
			t.Errorf("droppedUnnamed = %q, want it to name %q", got, want)
		}
	}
	named := RepairReport{Rules: []string{
		"V16 capabilities.persistent: coder is never declared beside web",
		"V17 persona.name: the persona's name must not be empty",
		"V18 dependencies.agents[0].why: a subagent needs a why",
		"V16 capabilities: add persistent to modes, or remove the block"}}
	if got := droppedUnnamed(first, bare, named); len(got) != 0 {
		t.Errorf("droppedUnnamed with a report that names each = %q, want none", got)
	}
	// A key the second answer keeps is not dropped, and a draft that is not
	// persistent has nothing to drop.
	if got := droppedUnnamed(first, first, other); len(got) != 0 {
		t.Errorf("droppedUnnamed over itself = %q", got)
	}
	if got := droppedUnnamed(bare, first, other); len(got) != 0 {
		t.Errorf("droppedUnnamed of an ordinary draft = %q", got)
	}
}

// TestTheNotesListASubagentOnlyForAPersistentDraft: a dependency of an
// autonomous release is informational and is not proposed as a subagent.
func TestTheNotesListASubagentOnlyForAPersistentDraft(t *testing.T) {
	d := &Draft{Manifest: persistentManifest(`{"memory": true}`, "", pipAgents, "")}
	d.noteSubagents()
	if !strings.Contains(d.Notes, subagentsHeading) || !strings.Contains(d.Notes, "remove its entry from `dependencies.agents`") {
		t.Errorf("a persistent draft's notes:\n%s", d.Notes)
	}
	plain := &Draft{Manifest: json.RawMessage(`{"schemaVersion": "2", "name": "x", "modes": ["autonomous"],` +
		` "dependencies": {"agents": ` + pipAgents + `}, "capabilities": {"network": {"egress": "none"}}}`)}
	plain.noteSubagents()
	if plain.Notes != "" {
		t.Errorf("an autonomous draft's notes:\n%s", plain.Notes)
	}
}

// TestThePersonaReportIsOptionalAndBounded: FromReports reads PERSONA.md
// when it is there, leaves the field empty when it is not, and refuses one
// past its bound.
func TestThePersonaReportIsOptionalAndBounded(t *testing.T) {
	dir := t.TempDir()
	d := &Draft{Instructions: "x", Notes: "y"}
	if err := FromReports(d, dir); err != nil || d.PersonaText != "" {
		t.Fatalf("no report: %q, %v", d.PersonaText, err)
	}
	if err := os.WriteFile(filepath.Join(dir, personaReport), []byte(pipPersona), 0o644); err != nil { //nolint:gosec // a test's own folder
		t.Fatal(err)
	}
	if err := FromReports(d, dir); err != nil || d.PersonaText != pipPersona {
		t.Fatalf("a report: %q, %v", d.PersonaText, err)
	}
	if err := os.WriteFile(filepath.Join(dir, personaReport), []byte(strings.Repeat("x", maxPersonaText+1)), 0o644); err != nil { //nolint:gosec // a test's own folder
		t.Fatal(err)
	}
	if err := FromReports(&Draft{Instructions: "x", Notes: "y"}, dir); err == nil {
		t.Fatal("a persona past its bound was read")
	}
}

// TestTheShapeCheckHoldsAPersistentDraftToItsKeys: the fixture shape names
// the keys exactly, the persona and the subagents, and an ordinary draft
// declares none of them.
func TestTheShapeCheckHoldsAPersistentDraftToItsKeys(t *testing.T) {
	parse := func(raw json.RawMessage) *manifest.Manifest {
		yml, err := manifestYAML(raw)
		if err != nil {
			t.Fatal(err)
		}
		m, err := manifest.Parse(yml)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	good := &Draft{Manifest: persistentManifest(`{"memory": true, "clock": true}`, `{"name": "Pip"}`, pipAgents, ""),
		Instructions: pipInstructions, PersonaText: pipPersona}
	want := Expect{Modes: []string{"persistent"}, Persistent: []string{"clock", "memory"}, Persona: true, Subagents: true}
	if got := CheckShape(parse(good.Manifest), good, want); len(got) != 0 {
		t.Fatalf("the expected shape was flagged: %v", got)
	}
	got := strings.Join(CheckShape(parse(good.Manifest), good, Expect{Modes: []string{"persistent"}, Persistent: []string{"memory"}}), "; ")
	for _, w := range []string{"capabilities.persistent declares [clock memory], want [memory]", "a persona the description does not ask for", "a subagent the description does not ask for"} {
		if !strings.Contains(got, w) {
			t.Errorf("shape check missed %q: %s", w, got)
		}
	}
	bare := &Draft{Manifest: persistentManifest(`{"memory": true, "clock": true}`, "", "", ""), Instructions: pipInstructions}
	got = strings.Join(CheckShape(parse(bare.Manifest), bare, want), "; ")
	for _, w := range []string{"no persona", "no subagent"} {
		if !strings.Contains(got, w) {
			t.Errorf("shape check missed %q: %s", w, got)
		}
	}
	noText := &Draft{Manifest: good.Manifest, Instructions: pipInstructions}
	if got := strings.Join(CheckShape(parse(noText.Manifest), noText, want), "; "); !strings.Contains(got, "a persona with no text") {
		t.Errorf("shape check missed the persona with no text: %s", got)
	}
	weak := &Draft{Manifest: persistentManifest(`{"memory": true}`, "", "", ""), Instructions: pipInstructions}
	if got := strings.Join(CheckShape(parse(weak.Manifest), weak, Expect{Modes: []string{"persistent"}, Persistent: []string{"memory"}}), "; "); !strings.Contains(got, "need capabilities.persistent.clock") {
		t.Errorf("shape check missed the tool of an undeclared key: %s", got)
	}
}

// TestARevisionIsGivenAPersistentAgentsPersona: the persona's file is
// staged beside the manifest and the instruction and named in the task, and
// a release with no persona stages the two files alone.
func TestARevisionIsGivenAPersistentAgentsPersona(t *testing.T) {
	src := t.TempDir()
	yml := strings.Replace(releaseYAML, "modes: [autonomous]", "modes: [persistent]", 1)
	yml = strings.Replace(yml, "publish:", "persona:\n  name: Pip\npublish:", 1)
	yml = strings.Replace(yml, "include: [agent.yaml,", "include: [agent.yaml, PERSONA.md,", 1)
	for name, body := range map[string]string{"agent.yaml": yml, "AGENT.md": "# Pip\n", "PERSONA.md": "PERSONA-MARKER\n"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dest := filepath.Join(t.TempDir(), ReviseInput)
	r, err := ReadRelease(src, dest, "", "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	if r.PersonaFile != "PERSONA.md" {
		t.Fatalf("%+v", r)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "PERSONA.md")); err != nil || string(b) != "PERSONA-MARKER\n" {
		t.Fatalf("the staged persona: %q, %v", b, err)
	}
	if err := r.WithTexts("Make Pip briefer.", ""); err != nil {
		t.Fatal(err)
	}
	task, err := RevisionTask(r)
	if err != nil || !strings.Contains(task, "its persona file is /inputs/revise/PERSONA.md") || strings.Contains(task, "PERSONA-MARKER") {
		t.Fatalf("the task: %v\n%s", err, task)
	}

	// A persona file the release names but does not carry is an error, not
	// a silent loss of the persona.
	if err := os.Remove(filepath.Join(src, "PERSONA.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRelease(src, filepath.Join(t.TempDir(), ReviseInput), "", "sha256:abc"); err == nil {
		t.Fatal("a release whose persona file is missing was read")
	}
}
