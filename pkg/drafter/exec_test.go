package drafter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// execManifest is a manifest declaring exec, with or without input files.
func execManifest(exec string, files bool) json.RawMessage {
	guide := ``
	if files {
		guide = `"guide": {"overview": "Scans a project.", "files": "Add the project as a .tar.gz."},`
	}
	return json.RawMessage(`{"schemaVersion": "2", "name": "vuln-scan", ` + guide +
		`"capabilities": {"exec": ` + exec + `}}`)
}

// TestExecProblems is the Drafter's rule on exec (agentry-cli#595): only
// the commands every machine provides, and an archive reader only in a
// QuickAgent that takes input files (guide.files, or an intake question of
// type file).
func TestExecProblems(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
		want []string
	}{
		{"exec none", execManifest(`"none"`, false), nil},
		{"no exec at all", json.RawMessage(`{"name": "x"}`), nil},
		{"a manifest that does not parse", json.RawMessage(`{`), nil},
		{"base commands", execManifest(`{"commands": ["grep", "sha256sum"]}`, false), nil},
		{"tar with input files", execManifest(`{"commands": ["tar", "grep"]}`, true), nil},
		{"tar with a file question", json.RawMessage(`{"intake": [{"id": "src", "type": "file"}], "capabilities": {"exec": {"commands": ["unzip"]}}}`), nil},
		{"tar without input files", execManifest(`{"commands": ["tar", "grep"]}`, false), []string{"capabilities.exec: tar: reads archives"}},
		{"the operator's case", execManifest(`{"commands": ["tar", "trivy", "curl"]}`, true), []string{"capabilities.exec: trivy, curl: not a command every machine provides"}},
		{"both", execManifest(`{"commands": ["sh", "unzip"]}`, false), []string{"capabilities.exec: sh: not a command", "capabilities.exec: unzip: reads archives"}},
	} {
		got := execProblems(tc.raw)
		if len(got) != len(tc.want) {
			t.Errorf("%s: %q, want %d line(s)", tc.name, got, len(tc.want))
			continue
		}
		for i, w := range tc.want {
			if !strings.HasPrefix(got[i], w) {
				t.Errorf("%s: line %d = %q, want it to start %q", tc.name, i, got[i], w)
			}
		}
	}
	for _, a := range archiveReaders {
		if !slices.Contains(policy.BaseExecCommands(), a) {
			t.Errorf("archive reader %q is not a base command", a)
		}
	}
}

// TestTheGuideListsTheBaseCommands: the Drafter is told exactly the
// commands a run provides, by its guide's page on commands since the list
// moved there (agentry-cli#659), so the list it reads and the runner's
// cannot drift apart.
func TestTheGuideListsTheBaseCommands(t *testing.T) {
	b := heldText(t, "manifest/exec.md")
	m := regexp.MustCompile(`which every run provides \(a run refuses any other\):(.*?)\. No shell`).FindStringSubmatch(b)
	if m == nil {
		t.Fatal("manifest/exec.md does not list the commands every run provides")
	}
	var listed []string
	for _, c := range regexp.MustCompile("`([a-z0-9]+)`").FindAllStringSubmatch(m[1], -1) {
		listed = append(listed, c[1])
	}
	if want := policy.BaseExecCommands(); !slices.Equal(listed, want) {
		t.Fatalf("manifest/exec.md lists %v,\nthe runner provides %v", listed, want)
	}
	// The archive readers' rule moved with the list.
	mustSay(t, map[string][]string{"manifest/exec.md": {"only when the QuickAgent takes input files", "A command can write only under `/tmp`"}})
}

// TestADraftDeclaringAnUnprovidedCommandGetsTheRepairTurn: the post-step
// sends a draft whose command a run would refuse to its repair turn with
// the rule's line, and a second answer that still declares one says so in
// the notes.
func TestADraftDeclaringAnUnprovidedCommandGetsTheRepairTurn(t *testing.T) {
	out := t.TempDir()
	passing := strings.Replace(postDraft, `"intake": [{"id": "format", "prompt": "Which format?", "type": "choice"}],`, "", 1)
	withTar := strings.Replace(passing, `"exec": "none"`, `"exec": {"commands": ["tar"]}`, 1)
	stillTar := strings.Replace(postRepaired, `"exec": "none"`, `"exec": {"commands": ["curl"]}`, 1)
	var tasks []string
	p := PostStep{
		OutDir: out, Description: "Scan my project.", Task: "Scan my project.",
		Scan: webscan.Result{Verdict: webscan.Pass}, Ref: Namespace + "/" + Name + "@" + Version, Checker: &postChecker{},
		Repair: func(_ context.Context, rt RepairTurn) (*receipt.Envelope, error) {
			tasks = append(tasks, rt.Task)
			if err := os.MkdirAll(filepath.Join(rt.OutDir, "reports"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rt.OutDir, "reports", instructionsReport), []byte("# Scan\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return &receipt.Envelope{Status: "ok", Result: json.RawMessage(stillTar)}, nil
		},
	}
	d, _, err := p.Run(context.Background(), firstTurn(t, out, withTar), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || !strings.Contains(tasks[0], "- capabilities.exec: tar: reads archives, but the QuickAgent takes no input files") {
		t.Fatalf("the repair task(s):\n%.800s", strings.Join(tasks, "\n---\n"))
	}
	// Both answers break the rule, so the second stands, and its notes
	// say a run would refuse it.
	if !d.IsRepaired() || !strings.Contains(d.Notes, "## Commands to fix before a run") || !strings.Contains(d.Notes, "curl: not a command every machine provides") {
		t.Fatalf("the draft: repaired %v, notes %q", d.IsRepaired(), d.Notes)
	}
	// A repair that adds an unprovided command to a draft that had none is
	// not kept.
	clean := &Draft{Validation: json.RawMessage(`{"passed":true}`), Instructions: "# X\n", Manifest: execManifest(`{"commands": ["grep"]}`, false)}
	broken := &Draft{Validation: json.RawMessage(`{"passed":true}`), Instructions: "# X\n", Manifest: execManifest(`{"commands": ["curl"]}`, false)}
	if !brokeARule(clean, broken) || brokeARule(broken, clean) {
		t.Fatal("brokeARule does not keep a first answer the exec rule passed over a second it refuses, or keeps a refused one over its mend")
	}
}
