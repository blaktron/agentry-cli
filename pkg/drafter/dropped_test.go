package drafter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
	"github.com/blaktron/agentry/pkg/draftcheck"
)

// scanDraft is a vulnerability scanner's manifest, as the operator's drafts
// on dev declared one: hosts, base commands, a secret and an MCP server
// with its own host and secret.
func scanDraft(egress, exec, secrets, servers string) *Draft {
	return &Draft{Validation: json.RawMessage(`{"passed":true}`), Instructions: "# Scan\n",
		Manifest: json.RawMessage(`{"schemaVersion": "2", "name": "vuln-scan", "capabilities": {"network": {"egress": ` + egress +
			`}, "exec": ` + exec + `, "secrets": ` + secrets + `, "mcpServers": ` + servers + `}}`)}
}

const (
	scanHosts   = `["api.github.com", "nvd.nist.gov"]`
	scanExec    = `{"commands": ["tar", "grep"]}`
	scanSecrets = `["GITHUB_TOKEN"]`
	scanServers = `[{"npm": "@cyanheads/osv-advisory-mcp-server", "version": "0.1.13", "egress": ["api.osv.dev"], "secrets": ["OSV_KEY"]}]`
)

// TestARepairNeverDropsWhatTheReportDoesNotName (agentry-cli#633): a
// second answer that no longer declares a server, a host, a command or a
// secret of the first's, which the report did not name, is not kept, even
// when it passes rules the first broke; one that drops only what the report
// named, or moves or re-pins what it keeps, is.
func TestARepairNeverDropsWhatTheReportDoesNotName(t *testing.T) {
	first := scanDraft(scanHosts, scanExec, scanSecrets, scanServers)
	offTask := RepairReport{Flagged: []string{"AI Review off_task, score 3: an agent that follows the instructions would not do the task the description asks for."}}
	for _, tc := range []struct {
		name    string
		second  *Draft
		report  RepairReport
		dropped []string
	}{
		{"the server, its host and its secret", scanDraft(scanHosts, scanExec, scanSecrets, `[]`), offTask,
			[]string{"the MCP server @cyanheads/osv-advisory-mcp-server", "the egress host api.osv.dev", "the secret OSV_KEY"}},
		{"the hosts", scanDraft(`"none"`, scanExec, scanSecrets, scanServers), offTask,
			[]string{"the egress host api.github.com", "the egress host nvd.nist.gov"}},
		{"every command", scanDraft(scanHosts, `"none"`, scanSecrets, scanServers), offTask,
			[]string{"the command tar", "the command grep"}},
		{"a secret", scanDraft(scanHosts, scanExec, `[]`, scanServers), offTask, []string{"the secret GITHUB_TOKEN"}},
		{"a rubric-shaped report on the text names no capability", scanDraft(`"none"`, `"none"`, `[]`, `[]`),
			RepairReport{Rules: []string{"S4 AGENT.md: the instructions name a credential pattern"}},
			[]string{"the egress host api.github.com", "the egress host nvd.nist.gov", "the command tar", "the command grep",
				"the secret GITHUB_TOKEN", "the MCP server @cyanheads/osv-advisory-mcp-server", "the egress host api.osv.dev", "the secret OSV_KEY"}},
		{"a server the rules named by its path", scanDraft(scanHosts, scanExec, scanSecrets, `[]`),
			RepairReport{Rules: []string{"capabilities.mcpServers[0]: npm:@cyanheads/osv-advisory-mcp-server@0.1.13: it needs Node >=26.0.0"}}, nil},
		{"a list the rules named whole", scanDraft(scanHosts, `"none"`, scanSecrets, scanServers),
			RepairReport{Rules: []string{"V5 capabilities.exec: exec requires a commands list"}}, nil},
		{"a command the exec rule named", scanDraft(scanHosts, `{"commands": ["grep"]}`, scanSecrets, scanServers),
			RepairReport{Rules: []string{"capabilities.exec: tar: reads archives, but the QuickAgent takes no input files; leave it out"}}, nil},
		{"a host the report named at the end of a sentence", scanDraft(`["api.github.com"]`, scanExec, scanSecrets, scanServers),
			RepairReport{Rules: []string{"V5 egress: wildcard-like host nvd.nist.gov."}}, nil},
		{"a host named only inside a longer one", scanDraft(`["api.github.com"]`, scanExec, scanSecrets, scanServers),
			RepairReport{Rules: []string{"egress: x.nvd.nist.gov is not resolvable"}}, []string{"the egress host nvd.nist.gov"}},
		{"a command named only inside a word", scanDraft(scanHosts, `{"commands": ["grep"]}`, scanSecrets, scanServers),
			RepairReport{Rules: []string{"the run could not start"}}, []string{"the command tar"}},
		{"a server re-pinned and a host moved to it", scanDraft(`["api.github.com"]`, scanExec, scanSecrets,
			`[{"npm": "@cyanheads/osv-advisory-mcp-server", "version": "0.1.12", "egress": ["api.osv.dev", "nvd.nist.gov"], "secrets": ["OSV_KEY"]}]`), offTask, nil},
		{"nothing dropped, something added", scanDraft(`["api.github.com", "nvd.nist.gov", "osv.dev"]`, scanExec, scanSecrets, scanServers), offTask, nil},
	} {
		got := droppedUnnamed(first, tc.second, tc.report)
		if !slices.Equal(got, tc.dropped) {
			t.Errorf("%s: dropped %q, want %q", tc.name, got, tc.dropped)
		}
		broke, dropped := keepFirst(first, tc.second, tc.report)
		if broke || !slices.Equal(dropped, tc.dropped) {
			t.Errorf("%s: keepFirst %v %q", tc.name, broke, dropped)
		}
	}

	// A second answer that mends what the first broke is still not kept
	// when it got there by dropping what the report did not name.
	refused := scanDraft(scanHosts, scanExec, scanSecrets, scanServers)
	refused.Validation = json.RawMessage(`{"passed":false}`)
	mended := scanDraft(scanHosts, scanExec, scanSecrets, `[]`)
	if broke, dropped := keepFirst(refused, mended, RepairReport{Rules: []string{"S4 AGENT.md: a credential pattern"}}); broke || len(dropped) != 3 {
		t.Fatalf("a mend by stripping: keepFirst %v %q", broke, dropped)
	}
	// One that keeps them and mends it is kept.
	if broke, dropped := keepFirst(refused, scanDraft(scanHosts, scanExec, scanSecrets, scanServers), RepairReport{Rules: []string{"S4 AGENT.md: x"}}); broke || dropped != nil {
		t.Fatalf("a mend: keepFirst %v %q", broke, dropped)
	}
	// A manifest that does not parse declares nothing, so a second answer
	// that is not one drops everything; a first that is not one drops nothing.
	if got := droppedUnnamed(first, &Draft{Manifest: json.RawMessage(`{`)}, offTask); len(got) != 8 {
		t.Fatalf("a second answer that does not parse dropped %q", got)
	}
	if got := droppedUnnamed(&Draft{Manifest: json.RawMessage(`{`)}, first, offTask); got != nil {
		t.Fatalf("a first answer that does not parse: %q", got)
	}
}

// The repair turn is told never to remove what the report does not name,
// and that a report on the instructions' wording is fixed by rewording.
func TestTheRepairTurnIsToldToKeepWhatTheTaskNeeds(t *testing.T) {
	d := refusedDraft()
	task, err := RepairTask(d, RepairReport{Rules: []string{"V10 intake[0].choices: type choice requires at least one choice"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Never remove a declared MCP server, egress host, command or secret, or a step of the instructions, that the report does not name: " +
			"the task needs them, and an answer that drops one is not kept.",
		"A report about the wording of the instructions is fixed by rewording it.",
	} {
		if !strings.Contains(task, want) {
			t.Errorf("the repair task lacks %q", want)
		}
	}
}

// rubricChecker flags every answer on the release rubric alone, high on C1
// and C6, and passes the draft question.
type rubricChecker struct{ postChecker }

func (c *rubricChecker) Check(ctx context.Context, p draftcheck.Package) (*Check, error) {
	ch, err := c.postChecker.Check(ctx, p)
	ch.Rubric = &CheckRubric{Verdict: CheckFlagged, RubricHash: "sha256:r", Findings: []CheckFinding{
		{ID: "C1_injection", Score: 2, Path: "AGENT.md", Severity: SeverityHigh},
		{ID: "C6_destructive_actions", Score: 3, Path: "AGENT.md", Severity: SeverityHigh}}}
	return ch, err
}

// TestARubricFindingOnADraftIsAdvisory (agentry-cli#633; plan
// quickagents-that-run.md D6): a draft the rules pass and the release
// rubric flags is not sent to the repair turn; the findings are recorded in
// draft.json and named in the notes as advisory, and AI Review still read
// the finished draft.
func TestARubricFindingOnADraftIsAdvisory(t *testing.T) {
	out := t.TempDir()
	passing := withServer(strings.Replace(postDraft, `"intake": [{"id": "format", "prompt": "Which format?", "type": "choice"}],`, "", 1), "0.1.13")
	checker := &rubricChecker{}
	p := PostStep{
		OutDir: out, Description: "Scan my code for vulnerabilities.", Task: "Scan my code for vulnerabilities.",
		Scan: webscan.Result{Verdict: webscan.Pass}, Ref: Namespace + "/" + Name + "@" + Version, Checker: checker,
		Servers: fakeServers(map[string]string{"0.1.13": ">=24.0.0"}),
		Repair: func(context.Context, RepairTurn) (*receipt.Envelope, error) {
			t.Fatal("a rubric finding sent the draft to its repair turn")
			return nil, errors.New("unreachable")
		},
	}
	d, _, err := p.Run(context.Background(), firstTurn(t, out, passing), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := d.AIReview.Check
	if d.IsRepaired() || !d.Passed() || len(checker.asked) != 1 || c == nil || c.Checks != 1 || c.Verdict() != CheckFlagged ||
		len(c.Rubric.Findings) != 2 || c.Rubric.Findings[1].Severity != SeverityHigh {
		t.Fatalf("the draft: repaired %v, passed %v, asked %d, check %+v", d.IsRepaired(), d.Passed(), len(checker.asked), c)
	}
	if !strings.Contains(d.Notes, "The release rubric's findings below are advisory") ||
		!strings.Contains(d.Notes, "(C6_destructive_actions, high)") || strings.Contains(d.Notes, "## Repaired once") {
		t.Fatalf("notes:\n%s", d.Notes)
	}
}

// offTaskChecker flags every answer off_task.
type offTaskChecker struct{ postChecker }

func (c *offTaskChecker) Check(ctx context.Context, p draftcheck.Package) (*Check, error) {
	ch, err := c.postChecker.Check(ctx, p)
	ch.Draft = &CheckDraft{Verdict: CheckFlagged, Score: 3, Scores: map[string]int{"off_task": 3}, Flagged: []string{"off_task"}, QuestionSet: "sha256:d"}
	return ch, err
}

// TestARepairThatStripsTheServerIsNotKept: an off_task draft whose repair
// turn answers without its MCP server keeps its first answer, says so in the
// notes, and its package is the first answer's again, server and all.
func TestARepairThatStripsTheServerIsNotKept(t *testing.T) {
	out := t.TempDir()
	passing := withServer(strings.Replace(postDraft, `"intake": [{"id": "format", "prompt": "Which format?", "type": "choice"}],`, "", 1), "0.1.13")
	checker := &offTaskChecker{}
	p := PostStep{
		OutDir: out, Description: "Scan my code for vulnerabilities.", Task: "Scan my code for vulnerabilities.",
		Scan: webscan.Result{Verdict: webscan.Pass}, Ref: Namespace + "/" + Name + "@" + Version, Checker: checker,
		Servers: fakeServers(map[string]string{"0.1.13": ">=24.0.0"}),
		Repair: func(_ context.Context, rt RepairTurn) (*receipt.Envelope, error) {
			return &receipt.Envelope{Status: "ok", Result: json.RawMessage(postRepaired)}, nil
		},
	}
	d, _, err := p.Run(context.Background(), firstTurn(t, out, passing), nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.IsRepaired() || !d.Passed() || len(checker.asked) != 1 {
		t.Fatalf("the draft: repaired %v, passed %v, asked %d", d.IsRepaired(), d.Passed(), len(checker.asked))
	}
	for _, want := range []string{"## Not repaired", "Its second answer left out what this one declares and the report did not name",
		"- the MCP server @cyanheads/osv-advisory-mcp-server\n", "- the egress host api.osv.dev\n"} {
		if !strings.Contains(d.Notes, want) {
			t.Errorf("the notes lack %q:\n%s", want, d.Notes)
		}
	}
	b, err := os.ReadFile(filepath.Join(out, "package", "agent.yaml"))
	if err != nil || !strings.Contains(string(b), "osv-advisory-mcp-server") {
		t.Fatalf("the package is not the first answer's: %v\n%s", err, b)
	}
}
