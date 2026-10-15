package drafter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/draftcheck"
	corejudge "github.com/blaktron/agentry/pkg/judge"
	"github.com/blaktron/agentry/pkg/manifest"
)

// hostDraftInstructions is an instruction file for the guide's worked
// target-host draft, in the six headings, written for the host tools.
const hostDraftInstructions = "# Linux server health check\n\n## Role\n\nReport one server's health; change nothing.\n\n" +
	"## Inputs\n\nThe server the person named, and the task's period.\n\n" +
	"## Steps\n\n1. With host_run, read /etc/os-release.\n2. With host_run, run df -h and free -m.\n\n" +
	"## Output\n\nreport.md: what needs attention, worst first.\n\n## Background\n\nThe thresholds.\n\n" +
	"## Checks\n\nNo command changes the server.\n"

// workedHostDraft is the guide's worked draft that takes a target host.
func workedHostDraft(t *testing.T) json.RawMessage {
	t.Helper()
	for _, ex := range regexp.MustCompile("(?s)```json\n(.*?)```").FindAllStringSubmatch(string(guideFile(t, "draft/examples.md")), -1) {
		if strings.Contains(ex[1], `"targetHosts"`) {
			return json.RawMessage(ex[1])
		}
	}
	t.Fatal("draft/examples.md has no worked draft that takes a target host")
	return nil
}

// TestADraftThatTakesATargetHostPasses: a draft with a host question listed
// in capabilities.targetHosts goes through the post-step as any other. The
// corrections leave both halves, the registry's rules and the local scans
// pass it with no finding, the Drafter's rules find nothing in it, and the
// check reads the line among the declared capabilities, where the rubric's
// runner grant names it. Without the line, the registry's rules refuse it.
func TestADraftThatTakesATargetHostPasses(t *testing.T) {
	raw := workedHostDraft(t)
	corrected, did := Correct(raw)
	if len(did) > 0 {
		t.Errorf("the worked draft took corrections: %q", did)
	}
	var m struct {
		Intake       []map[string]any `json:"intake"`
		Capabilities struct {
			TargetHosts []string `json:"targetHosts"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(corrected, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Intake) != 1 || m.Intake[0]["type"] != "host" || len(m.Capabilities.TargetHosts) != 1 || m.Capabilities.TargetHosts[0] != m.Intake[0]["id"] {
		t.Fatalf("the corrected draft lost a half: intake %v, targetHosts %v", m.Intake, m.Capabilities.TargetHosts)
	}

	d := &Draft{Manifest: corrected, Instructions: hostDraftInstructions}
	p := PostStep{OutDir: t.TempDir()}
	p.validate(d)
	if lines, refused := rulesReport(d); refused || len(lines) > 0 || d.ManifestError != "" {
		t.Fatalf("the rules refused a target-host draft: %q (%s)", lines, d.ManifestError)
	}
	if !strings.Contains(string(d.Validation), `"passed":true`) {
		t.Fatalf("validate: %s", d.Validation)
	}

	pkg, err := CheckPackage("Check the health of a Linux server I name.", p.pkg())
	if err != nil {
		t.Fatal(err)
	}
	rd, err := draftcheck.Read(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if got := corejudge.DeclaredCapabilities(rd.ManifestJSON, rd.Ships); !strings.Contains(got, `"targetHosts":["server"]`) {
		t.Errorf("the check's declared capabilities do not name the target host: %s", got)
	}
	if !strings.Contains(corejudge.RunnerGrants, "When declared_capabilities lists targetHosts") {
		t.Error("the rubric's runner grant no longer names targetHosts")
	}

	// Without its line, the host question is one the registry refuses (V14).
	yml, err := os.ReadFile(filepath.Join(p.pkg(), "agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bare, err := manifest.Parse([]byte(strings.Replace(string(yml), "  targetHosts:\n    - server\n", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if len(bare.Capabilities.TargetHosts) != 0 || len(manifest.Blocking(manifest.Validate(bare))) == 0 {
		t.Errorf("a host question no targetHosts line names passed the registry's rules:\n%s", yml)
	}
}

// TestADraftThatMakesItsOwnSSHGoesToTheRepairTurn: a draft that declares an
// SSH client instead of a target host is refused by the Drafter's rules,
// with a line that names the host question and targetHosts, and the notes
// of a finished draft that keeps it say so.
func TestADraftThatMakesItsOwnSSHGoesToTheRepairTurn(t *testing.T) {
	raw := json.RawMessage(`{"schemaVersion": "2", "name": "server-helper", "intake": [{"id": "server", "prompt": "The server", "type": "text"}],` +
		` "capabilities": {"network": {"egress": "none"}, "exec": {"commands": ["ssh", "grep"]}}}`)
	d := &Draft{Manifest: raw, Instructions: hostDraftInstructions}
	lines, refused := rulesReport(d)
	if !refused || len(lines) != 1 || !strings.Contains(lines[0], "capabilities.exec: ssh: makes its own SSH session") ||
		!strings.Contains(lines[0], "intake question of type host") || !strings.Contains(lines[0], "capabilities.targetHosts") {
		t.Fatalf("the rules' report: %q, %v", lines, refused)
	}
	d.noteExecProblems()
	if !strings.Contains(d.Notes, "## Commands to fix before a run") || !strings.Contains(d.Notes, "ssh: makes its own SSH session") {
		t.Errorf("the notes: %q", d.Notes)
	}
}

// TestTheGuideTeachesTargetHosts: the instruction and the guide's pages
// ask for a machine by a host question listed in capabilities.targetHosts,
// declare no SSH client, egress or key for it, write the instructions for
// the host tools, never ask for the sign-in in a question, and say that
// such a QuickAgent runs only on the person's computer.
func TestTheGuideTeachesTargetHosts(t *testing.T) {
	mustSay(t, map[string][]string{
		instructionFile: {
			"is a **target host**: a `host` intake question listed in `capabilities.targetHosts`",
			"**never `ssh`, `scp`, `sftp` or `rsync` in `exec`**",
			"It runs only on the person's computer, which the notes say.",
			"host tools by name (`host_run`, the shell only for a program that asks as it runs)",
			"never locks the person out",
			"a server by a `host` question, whose sign-in the run setup asks for",
			"`manifest/target-hosts.md`",
		},
		"manifest/target-hosts.md": {
			"**Declare a target host when the task works on a machine the person names**",
			"**Every `host` question is listed in `targetHosts`, and every entry names a `host` question**",
			"Never ask for a password, a key or a passphrase in a question of its own",
			"**Never declare `ssh`, `scp`, `sftp` or `rsync` in `exec`**",
			"**The server's own network is outside the run.**",
			"`host_shell_send`'s key `sudo-password`",
			"**Never lock the person out.**",
			"**Only on the person's computer**",
			"Say so in one bullet of the notes",
		},
		"manifest/intake-and-guide.md": {
			"`host`: a machine the person names when the run starts",
			"**Its id is listed in `capabilities.targetHosts`**",
		},
		"manifest/manifest.md": {
			"- `targetHosts`: the ids of the intake's `host` questions",
			"- `capabilities.targetHosts`: at most 4, each the id of a `host` question",
		},
		"manifest/exec.md": {
			"**Never `ssh`, `scp`, `sftp` or `rsync`.**",
		},
		"runtime/how-a-run-works.md": {
			"- **Target hosts.** When the manifest declares one (`capabilities.targetHosts`)",
			"a QuickAgent that declares a target host does not run there at all",
		},
		"guide/start-here.md": {
			"[Target hosts](../manifest/target-hosts.md)",
		},
		"draft/examples.md": {
			"## A target host: checking a server's health",
		},
	})
	if want := fmt.Sprintf("`capabilities.targetHosts`: at most %d,", manifest.MaxTargetHosts); !strings.Contains(heldText(t, "manifest/manifest.md"), want) {
		t.Errorf("manifest/manifest.md does not state the bound on target hosts as %q", want)
	}
}
