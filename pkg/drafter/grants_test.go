package drafter

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// websiteTestManifest is a website test plan runner's manifest as a draft
// of one declares it, with browser the capabilities.browser line or "".
func websiteTestManifest(browser string) json.RawMessage {
	caps := `"network": {"egress": "none"}`
	if browser != "" {
		caps += `, "browser": ` + browser
	}
	return json.RawMessage(`{"schemaVersion": "2", "name": "website-test-plan-runner",` +
		` "intake": [{"id": "url", "prompt": "The site to test", "type": "url"}], "capabilities": {` + caps + `}}`)
}

const websiteTestInstructions = "# Website test plan runner\n\n## Steps\n\n" +
	"1. Open the site with browser_navigate and read it with browser_snapshot.\n" +
	"2. Work each test case with browser_click and browser_type.\n" +
	"3. Never call fetch: the browser reads every page.\n"

// TestInstructionsThatCallABrowserTheManifestLacksGoToTheRepairTurn: the
// shape that reached production with browser instructions and no browser
// line is refused by the Drafter's rules, with a line that names the tool
// and the grant to add; with the browser declared, and for a tool the
// instructions turn away, there is no line.
func TestInstructionsThatCallABrowserTheManifestLacksGoToTheRepairTurn(t *testing.T) {
	d := &Draft{Validation: json.RawMessage(`{"passed":true}`), Manifest: websiteTestManifest(""), Instructions: websiteTestInstructions}
	lines, refused := rulesReport(d)
	if !refused || len(lines) != 1 {
		t.Fatalf("the rules' report: %q, %v; want one line on the browser", lines, refused)
	}
	for _, want := range []string{"browser_navigate (line 5), browser_snapshot (line 5), browser_click (line 6), browser_type (line 6) need capabilities.browser", "Add capabilities.browser", "manifest/browser.md"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the line %q does not say %q", lines[0], want)
		}
	}
	if strings.Contains(lines[0], "fetch") {
		t.Errorf("the line %q names fetch, which the instructions turn away", lines[0])
	}

	d.Manifest = websiteTestManifest(`{"sites": ["url"]}`)
	if lines, refused := rulesReport(d); refused || len(lines) != 0 {
		t.Errorf("with the browser declared, the rules' report: %q, %v", lines, refused)
	}
}

// TestEveryUngrantedToolFamilyIsNamed: a command, a host fetch and a
// target host's tools are named as the browser's are, each with its grant.
func TestEveryUngrantedToolFamilyIsNamed(t *testing.T) {
	m := json.RawMessage(`{"schemaVersion": "2", "name": "ops-helper", "capabilities": {"network": {"egress": "none"}}}`)
	ins := "# Ops\n\n## Steps\n\n1. Build it with run_command.\n2. Read the status page with the `fetch` tool.\n3. Restart the service with host_run.\n"
	probs := grantProblems(m, ins)
	want := []string{"capabilities.exec", "capabilities.network.egress", "capabilities.targetHosts"}
	if len(probs) != len(want) {
		t.Fatalf("grantProblems = %q, want one line per tool", probs)
	}
	for i, path := range want {
		if !strings.Contains(probs[i], "Add "+path) {
			t.Errorf("line %d = %q, want it to add %s", i, probs[i], path)
		}
	}
	if got := grantProblems(json.RawMessage(`{not a manifest`), ins); got != nil {
		t.Errorf("a manifest that does not parse: %q, want nothing (validate reports it)", got)
	}
}

// TestARepairThatCallsAnUngrantedToolIsNotKept: a second answer whose
// instructions call a tool its manifest lacks, where the first's did not,
// broke a rule the first passed.
func TestARepairThatCallsAnUngrantedToolIsNotKept(t *testing.T) {
	first := &Draft{Validation: json.RawMessage(`{"passed":true}`), Manifest: websiteTestManifest(""), Instructions: "# Plan\n\n## Steps\n\n1. Read the plan.\n"}
	second := &Draft{Validation: json.RawMessage(`{"passed":true}`), Manifest: websiteTestManifest(""), Instructions: websiteTestInstructions}
	if broke, dropped := keepFirst(first, second, RepairReport{Rules: []string{"the instructions: shorten them"}}); !broke || len(dropped) != 0 {
		t.Errorf("keepFirst = %v, %q; want the first kept for the rule the second broke", broke, dropped)
	}
	mended := &Draft{Validation: json.RawMessage(`{"passed":true}`), Manifest: websiteTestManifest(`{"sites": ["url"]}`), Instructions: websiteTestInstructions}
	if broke, dropped := keepFirst(second, mended, RepairReport{Rules: grantProblems(second.Manifest, second.Instructions)}); broke || len(dropped) != 0 {
		t.Errorf("a repair that adds the browser: keepFirst = %v, %q; want it kept", broke, dropped)
	}
}

// TestARepairNeverDropsTheBrowserOrATargetHostUnnamed: the browser and a
// target host are capabilities a repair turn must not drop without the
// report naming them, as a server or a secret is.
func TestARepairNeverDropsTheBrowserOrATargetHostUnnamed(t *testing.T) {
	withBoth := &Draft{Manifest: json.RawMessage(`{"schemaVersion": "2", "name": "site-and-server",` +
		` "intake": [{"id": "url", "type": "url"}, {"id": "server", "type": "host"}],` +
		` "capabilities": {"network": {"egress": "none"}, "browser": {"sites": ["url"]}, "targetHosts": ["server"]}}`)}
	without := &Draft{Manifest: websiteTestManifest("")}
	got := droppedUnnamed(withBoth, without, RepairReport{Rules: []string{"the instructions: shorten them"}})
	for _, want := range []string{"the browser at capabilities.browser", "the target host server"} {
		if !slices.Contains(got, want) {
			t.Errorf("droppedUnnamed = %q, want it to name %q", got, want)
		}
	}
	named := RepairReport{Rules: []string{"capabilities.browser.sites[0]: url is not a url question", "capabilities.targetHosts[0]: server is not a host question"}}
	if got := droppedUnnamed(withBoth, without, named); len(got) != 0 {
		t.Errorf("droppedUnnamed with a report that names both = %q, want none", got)
	}
}
