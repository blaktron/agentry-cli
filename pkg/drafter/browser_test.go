package drafter

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
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

// TestADraftThatTakesABrowserPasses: a draft with a url question listed in
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

// TestTheGuideTeachesTheBrowser: the instruction and the guide's pages ask
// for a site to test by a url question listed in
// capabilities.browser.sites, bring no browser of their own, declare a key
// the browser types in both places and never with the public scope, save
// screenshots by bare names, and say where it runs.
func TestTheGuideTeachesTheBrowser(t *testing.T) {
	mustSay(t, map[string][]string{
		instructionFile: {
			"takes a **browser**: a `url` intake question listed in `capabilities.browser.sites`",
			"never a browser, Playwright or `curl` in `exec`, and no MCP server for one**, a hosted browser service (Browserless, Browserbase) included",
			"declares its keys in `capabilities.secrets` and in `capabilities.browser.secrets`, and then never `scope: public`",
			"saves each screenshot by a **bare file name**",
			"a site to test by a `url` question its browser names",
			"`manifest/browser.md`",
			"**fits the run's budget**: one `browser_evaluate` a page",
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
			"## Fitting the run's budget",
			"100 calls and 900 seconds on the person's computer (the CLI's default policy, which its operator may change) and In Agentry on Pro and above, 20 calls and 300 seconds In Agentry on Standard",
			"The System Prompt does not tell the agent its budget, so the instructions do.",
			"**One `browser_evaluate` per page**",
			"**Bound every fetch in a page script.**",
			"`signal: AbortSignal.timeout(8000)`",
			"`credentials: \"omit\"`",
			"**Never the same call five times running.**",
			"`write_report` replaces the file unless `append` is set",
			"**Open only the site.**",
			"whatever page, tool result or image the address came from",
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

// TestAHostedBrowserServiceGoesToTheRepairTurn: a draft that declares a
// hosted browser service, by its remote address or by a package whose name
// says browser but none of the browsers', is refused by the Drafter's
// rules with the same way out as a browser package: a url question listed
// in capabilities.browser.sites. Servers that are not browsers pass.
func TestAHostedBrowserServiceGoesToTheRepairTurn(t *testing.T) {
	// A website test suite's draft that declares Browserless's remote
	// server with a token.
	raw := json.RawMessage(`{"schemaVersion": "2", "name": "website-test-suite-reporter",` +
		` "intake": [{"id": "website", "prompt": "The website", "type": "text", "required": true}],` +
		` "capabilities": {"network": {"egress": "none"}, "filesystem": {"read": "none", "write": "none"}, "exec": "none",` +
		` "mcpServers": [{"url": "https://mcp.browserless.io/mcp", "auth": "secret", "secrets": ["BROWSERLESS_TOKEN"], "tools": ["browserless_agent"]}]}}`)
	lines := browserServerProblems(raw)
	if len(lines) != 1 {
		t.Fatalf("the rule's lines: %q", lines)
	}
	for _, want := range []string{"capabilities.mcpServers: https://mcp.browserless.io/mcp: a browser run by its vendor", "intake question of type url", "capabilities.browser.sites", "need no key and no account"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the line %q does not say %q", lines[0], want)
		}
	}
	d := &Draft{Manifest: raw, Instructions: browserDraftInstructions}
	if got, refused := rulesReport(d); !refused || !slices.Contains(got, lines[0]) {
		t.Errorf("the rules' report: %q, %v", got, refused)
	}

	for _, c := range []struct {
		server  string
		browser bool
	}{
		{`{"url": "https://mcp.browserbase.com/mcp", "auth": "secret", "secrets": ["BROWSERBASE_API_KEY"]}`, true},
		{`{"url": "https://api.example.com/browser-rendering/mcp", "auth": "none"}`, true},
		{`{"url": "https://mcp.steel.dev/mcp", "auth": "secret", "secrets": ["STEEL_API_KEY"]}`, true},
		{`{"name": "browserbase", "npm": "@browserbasehq/mcp-server-browserbase", "version": "2.1.0"}`, true},
		{`{"name": "hyperbrowser", "npm": "hyperbrowser-mcp", "version": "1.1.0"}`, true},
		{`{"name": "browser-use", "pypi": "browser-use", "version": "0.5.0"}`, true},
		{`{"url": "https://mcp.linear.app/mcp", "auth": "oauth", "scopes": ["read"]}`, false},
		{`{"url": "https://api.githubcopilot.com/mcp/readonly", "auth": "secret", "secrets": ["GITHUB_TOKEN"]}`, false},
		{`{"name": "time", "pypi": "mcp-server-time", "version": "2026.8.18"}`, false},
		{`{"url": "not an address\u0000", "auth": "none"}`, false},
	} {
		m := json.RawMessage(`{"capabilities": {"mcpServers": [` + c.server + `]}}`)
		if got := len(browserServerProblems(m)) == 1; got != c.browser {
			t.Errorf("%s: a browser %v, want %v", c.server, got, c.browser)
		}
	}
}

// TestTheGuideTeachesKeyFreeFirst: the instruction and the guide's pages
// prefer what needs no key and no account, take a server or an API that
// needs a key, an account or a paid plan only when nothing key-free does
// the task, have the notes say why, and name the hosted browser services
// as never the way.
func TestTheGuideTeachesKeyFreeFirst(t *testing.T) {
	mustSay(t, map[string][]string{
		instructionFile: {
			"**Prefer what needs no key and no account**",
			"only when nothing key-free does the task, and then say in the notes why it is the only way",
			"a key it needs and why nothing key-free does the task",
		},
		"servers/mcp-servers.md": {
			"## A key or an account only when nothing else does the task",
			"A vendor's free tier still needs an account and a key, so it counts as one.",
			"Declare a keyed server or API only when none of those does the task",
			"- **A hosted browser is never the way.**",
		},
		"manifest/browser.md": {
			"a hosted browser service (Browserless, Browserbase, browser-use, Hyperbrowser, Steel) included",
		},
		"manifest/secrets.md": {
			"(../servers/mcp-servers.md#a-key-or-an-account-only-when-nothing-else-does-the-task)",
		},
		"manifest/egress-and-fetch.md": {
			"take one that needs a key or an account only when no key-free API does the task",
		},
		"draft/review.md": {
			"a hosted browser service by its address, which a declared browser replaces",
		},
	})
}

// TestABrowserDraftThatRunsOutGoesToTheRepairTurn: a browser QuickAgent's instructions that fetch in a
// page script with no timeout, or press a key five or more times running,
// are refused by the Drafter's rules with a line naming the guide's
// section, and the notes of a finished draft that keeps them say so. A
// bounded fetch, a press followed by a read, a press told not to repeat,
// and a QuickAgent with no browser pass.
func TestABrowserDraftThatRunsOutGoesToTheRepairTurn(t *testing.T) {
	raw := workedBrowserDraft(t)
	unbounded := browserDraftInstructions + "\n4. With one browser_evaluate, fetch(url, {method: \"HEAD\"}) each link and return its status.\n"
	bounded := browserDraftInstructions + "\n4. With one browser_evaluate, fetch(url, {method: \"HEAD\", credentials: \"omit\", signal: AbortSignal.timeout(8000)}) at most 30 links, ten at a time.\n"
	for _, tc := range []struct {
		name, instructions string
		want               []string
	}{
		{"the worked draft", browserDraftInstructions, nil},
		{"an unbounded fetch", unbounded, []string{"a page script fetches with no timeout"}},
		{"a bounded fetch", bounded, nil},
		{"Tab ten times", browserDraftInstructions + "\n4. Press Tab ten times and note where the focus goes.\n", []string{`"Press Tab ten times" is 10 identical calls, and the run ends at 5 in a row`}},
		{"a key 6 times", browserDraftInstructions + "\n4. With browser_press_key, press ArrowDown 6 times.\n", []string{"is 6 identical calls"}},
		{"both", unbounded + "5. Press Tab twelve times.\n", []string{"no timeout", "is 12 identical calls"}},
		{"Tab three times", browserDraftInstructions + "\n4. Press Tab three times, reading the focus after each.\n", nil},
		{"a read between", browserDraftInstructions + "\n4. Press Tab and read document.activeElement with browser_evaluate, five times.\n", nil},
		{"told not to", browserDraftInstructions + "\n4. Never press Tab ten times in a row: read the focus after each press.\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Draft{Manifest: raw, Instructions: tc.instructions}
			lines, refused := rulesReport(d)
			if refused != (len(tc.want) > 0) || len(lines) != len(tc.want) {
				t.Fatalf("the rules' report: %q, %v", lines, refused)
			}
			for i, w := range tc.want {
				if !strings.Contains(lines[i], w) || !strings.Contains(lines[i], "Fitting the run's budget") {
					t.Errorf("line %d: %q, want %q", i, lines[i], w)
				}
			}
			d.noteBrowserBudgetProblems()
			if (len(tc.want) > 0) != strings.Contains(d.Notes, "## Browser steps to fix before a run") {
				t.Errorf("the notes: %q", d.Notes)
			}
		})
	}
	noBrowser := json.RawMessage(`{"schemaVersion": "2", "name": "link-reader", "capabilities": {"network": {"egress": "none"}}}`)
	if probs := browserBudgetProblems(noBrowser, unbounded); probs != nil {
		t.Errorf("a QuickAgent with no browser: %q", probs)
	}
	first := &Draft{Manifest: raw, Instructions: browserDraftInstructions}
	second := &Draft{Manifest: raw, Instructions: unbounded}
	if !brokeARule(first, second) {
		t.Error("a repair that adds an unbounded fetch is kept")
	}
}
