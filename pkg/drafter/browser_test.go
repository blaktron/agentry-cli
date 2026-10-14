package drafter

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/draftcheck"
	corejudge "github.com/blaktron/agentry/pkg/judge"
	"github.com/blaktron/agentry/pkg/manifest"
)

// browserDraftInstructions is an instruction file for the guide's worked
// browser draft, in the six headings, written for the browser tools.
const browserDraftInstructions = "# Site link and layout check\n\n## Role\n\nCheck a site's links and phone layout; change nothing.\n\n" +
	"## Inputs\n\nThe site the person named, and the task's page limit.\n\n" +
	"## Steps\n\n1. With browser_navigate, open the site.\n2. With browser_snapshot, list its links.\n" +
	"3. With browser_resize, try 390 by 844, and browser_take_screenshot as home-phone.png.\n\n" +
	"## Output\n\nreport.md: broken links and layout problems, worst first.\n\n## Background\n\nWhat a broken link is.\n\n" +
	"## Checks\n\nNo form submitted, nothing bought or posted.\n"

// workedBrowserDraft is the guide's worked draft that takes a browser.
func workedBrowserDraft(t *testing.T) json.RawMessage {
	t.Helper()
	for _, ex := range regexp.MustCompile("(?s)```json\n(.*?)```").FindAllStringSubmatch(string(guideFile(t, "draft/examples.md")), -1) {
		if strings.Contains(ex[1], `"browser"`) {
			return json.RawMessage(ex[1])
		}
	}
	t.Fatal("draft/examples.md has no worked draft that takes a browser")
	return nil
}

// TestADraftThatTakesABrowserPasses (browser testing M8, agentry-cli#916;
// plan browser-testing.md D6, D9): a draft with a url question listed in
// capabilities.browser.sites goes through the post-step as any other. The
// corrections leave both halves, the registry's rules and the local scans
// pass it, the Drafter's rules find nothing in it, and the check reads the
// line among the declared capabilities. Its signed-in form, a key in
// capabilities.secrets and capabilities.browser.secrets, passes too; the
// registry refuses a site no url question carries, and keys with the
// public scope.
func TestADraftThatTakesABrowserPasses(t *testing.T) {
	raw := workedBrowserDraft(t)
	corrected, did := Correct(raw)
	if len(did) > 0 {
		t.Errorf("the worked draft took corrections: %q", did)
	}
	var m struct {
		Intake       []map[string]any `json:"intake"`
		Capabilities struct {
			Browser struct {
				Sites []string `json:"sites"`
			} `json:"browser"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(corrected, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Intake) != 1 || m.Intake[0]["type"] != "url" || len(m.Capabilities.Browser.Sites) != 1 || m.Capabilities.Browser.Sites[0] != m.Intake[0]["id"] {
		t.Fatalf("the corrected draft lost a half: intake %v, browser %v", m.Intake, m.Capabilities.Browser)
	}

	d := &Draft{Manifest: corrected, Instructions: browserDraftInstructions}
	p := PostStep{OutDir: t.TempDir()}
	p.validate(d)
	if lines, refused := rulesReport(d); refused || len(lines) > 0 || d.ManifestError != "" {
		t.Fatalf("the rules refused a browser draft: %q (%s)", lines, d.ManifestError)
	}
	if !strings.Contains(string(d.Validation), `"passed":true`) {
		t.Fatalf("validate: %s", d.Validation)
	}
	pkg, err := CheckPackage("Check my website for broken links and pages that look wrong on a phone.", p.pkg())
	if err != nil {
		t.Fatal(err)
	}
	rd, err := draftcheck.Read(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if got := corejudge.DeclaredCapabilities(rd.ManifestJSON, rd.Ships); !strings.Contains(got, `"browser":{"sites":["site"]}`) {
		t.Errorf("the check's declared capabilities do not name the browser: %s", got)
	}

	// variant is the worked draft with its capabilities edited.
	variant := func(edit func(caps, browser map[string]any)) string {
		var doc map[string]any
		if err := json.Unmarshal(corrected, &doc); err != nil {
			t.Fatal(err)
		}
		caps := doc["capabilities"].(map[string]any)
		edit(caps, caps["browser"].(map[string]any))
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	keys := []any{"SITE_USER", "SITE_PASSWORD"}
	signedIn := variant(func(caps, b map[string]any) { caps["secrets"], b["secrets"] = keys, keys })
	public := variant(func(caps, b map[string]any) { caps["secrets"], b["secrets"], b["scope"] = keys, keys, "public" })
	elsewhere := variant(func(_, b map[string]any) { b["sites"] = []any{"shop"} })
	for _, c := range []struct {
		name, manifest string
		passes         bool
	}{
		{"signed in", signedIn, true},
		{"keys with the public scope", public, false},
		{"a site no url question carries", elsewhere, false},
	} {
		d := &Draft{Manifest: json.RawMessage(c.manifest), Instructions: browserDraftInstructions}
		p := PostStep{OutDir: t.TempDir()}
		p.validate(d)
		passed := strings.Contains(string(d.Validation), `"passed":true`)
		if passed != c.passes {
			t.Errorf("%s: passed %v, want %v (%s %s)", c.name, passed, c.passes, d.Validation, d.ManifestError)
		}
	}
}

// TestADraftThatBringsItsOwnBrowserGoesToTheRepairTurn: a draft that
// declares a browser of its own, as a command or an MCP server, instead of
// a browser line is refused by the Drafter's rules, with a line that names
// the url question and capabilities.browser.sites; the notes of a finished
// draft that keeps the command say so.
func TestADraftThatBringsItsOwnBrowserGoesToTheRepairTurn(t *testing.T) {
	raw := json.RawMessage(`{"schemaVersion": "2", "name": "site-tester", "intake": [{"id": "site", "prompt": "The site", "type": "text"}],` +
		` "capabilities": {"network": {"egress": "none"}, "exec": {"commands": ["chromium-browser", "grep"]},` +
		` "mcpServers": [{"name": "playwright", "npm": "@playwright/mcp", "version": "0.0.83"}, {"name": "github", "npm": "@modelcontextprotocol/server-github", "version": "2025.4.8"}]}}`)
	d := &Draft{Manifest: raw, Instructions: browserDraftInstructions}
	lines, refused := rulesReport(d)
	if !refused || len(lines) != 2 {
		t.Fatalf("the rules' report: %q, %v", lines, refused)
	}
	for i, want := range []string{"capabilities.exec: chromium-browser: drives a browser of its own", "capabilities.mcpServers: playwright (@playwright/mcp): a browser of its own"} {
		if !strings.Contains(lines[i], want) || !strings.Contains(lines[i], "intake question of type url") || !strings.Contains(lines[i], "capabilities.browser.sites") {
			t.Errorf("line %d: %q, want %q", i, lines[i], want)
		}
	}
	d.noteExecProblems()
	if !strings.Contains(d.Notes, "chromium-browser: drives a browser of its own") {
		t.Errorf("the notes: %q", d.Notes)
	}
	for _, c := range []string{"playwright", "/usr/bin/google-chrome", "geckodriver", "selenium-server"} {
		if !drivesABrowser(c) {
			t.Errorf("%s does not drive a browser", c)
		}
	}
	for _, c := range []string{"grep", "sha256sum", "chromaprint"} {
		if drivesABrowser(c) {
			t.Errorf("%s drives a browser", c)
		}
	}
}

// TestTheGuideTeachesTheBrowser (browser testing M8, agentry-cli#916): the
// instruction and the guide's pages ask for a site to test by a url
// question listed in capabilities.browser.sites, bring no browser of their
// own, declare a key the browser types in both places and never with the
// public scope, save screenshots by bare names, and say where it runs.
func TestTheGuideTeachesTheBrowser(t *testing.T) {
	mustSay(t, map[string][]string{
		instructionFile: {
			"takes a **browser**: a `url` intake question listed in `capabilities.browser.sites`",
			"never a browser, Playwright or `curl` in `exec`, and no MCP server for one",
			"declares its keys in `capabilities.secrets` and in `capabilities.browser.secrets`, and then never `scope: public`",
			"saves each screenshot by a **bare file name**",
			"a site to test by a `url` question its browser names",
			"`manifest/browser.md`",
		},
		"manifest/browser.md": {
			"**Declare a browser when the task tests a website as a person sees it**",
			"A `url` question listed here is the site the browser opens, never a file the runner downloads first.",
			"**Use `site`.**",
			"**The browser is not egress.**",
			"**in both places**",
			"**A QuickAgent that types keys never has `scope: public`**",
			"**A sign-up test needs no key.**",
			"**A file name is a bare name**",
			"**Change nothing the person did not ask for.**",
			"In Agentry** too since CLI v0.6.118, for a public site on port 80 or 443 only",
		},
		"manifest/intake-and-guide.md": {
			"**A `url` question listed in `capabilities.browser.sites` is the site a browser opens instead**",
		},
		"manifest/manifest.md": {
			"- `browser`: a headless browser for a QuickAgent that tests a website the person names",
		},
		"manifest/exec.md": {
			"**Never a browser of its own**",
		},
		"manifest/egress-and-fetch.md": {
			"**A website to test in a browser is not egress.**",
		},
		"manifest/secrets.md": {
			"is declared here **and** in `capabilities.browser.secrets`",
		},
		"runtime/how-a-run-works.md": {
			"- **A browser.** When the manifest declares one (`capabilities.browser`)",
			"a browser there opens public sites on ports 80 and 443 only",
		},
		"guide/start-here.md": {
			"[A browser](../manifest/browser.md)",
		},
		"draft/examples.md": {
			"## A browser: checking a site's links and layout",
		},
	})
	for _, want := range []string{
		fmt.Sprintf("`capabilities.browser`: at most %d `sites`", manifest.MaxBrowserSites),
		fmt.Sprintf("at most %d `hosts`", manifest.MaxBrowserHosts),
	} {
		if !strings.Contains(heldText(t, "manifest/manifest.md"), want) {
			t.Errorf("manifest/manifest.md does not state %q", want)
		}
	}
}
