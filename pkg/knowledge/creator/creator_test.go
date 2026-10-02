package creator

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry/pkg/manifest"
)

// TestTheCreatorDeclaresOnlyFiles holds the embedded package to the
// contract plan knowledge.md §4.2 gives it, as the compiler reads it: no
// egress, no filesystem, no exec — the runner decodes the documents, the
// model runs nothing — and the files data category, in the one autonomous
// mode, with the identity the constants name.
func TestTheCreatorDeclaresOnlyFiles(t *testing.T) {
	raw, err := fs.ReadFile(FS(), "agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if blocking := manifest.Blocking(manifest.Validate(m)); len(blocking) > 0 {
		t.Fatalf("the Creator's manifest does not validate: %v", blocking)
	}
	if m.Namespace != Namespace || m.Name != Name || m.Version != Version {
		t.Fatalf("identity %s/%s@%s, constants %s/%s@%s", m.Namespace, m.Name, m.Version, Namespace, Name, Version)
	}

	ws := t.TempDir()
	if err := os.CopyFS(ws, FS()); err != nil {
		t.Fatal(err)
	}
	pol := policy.Default()
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Mode: manifest.ModeAutonomous, Digest: "sha256:test", Source: "builtin",
		WorkspaceDir: ws, Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test",
	}, filepath.Join(t.TempDir(), "plan"))
	if err != nil {
		t.Fatalf("the Creator does not compile: %v", err)
	}
	c := plan.Capabilities
	if c.EgressMode != "none" || c.FSReadMode != "none" || c.FSWriteMode != "none" || len(c.ExecCommands) != 0 || !slices.Equal(c.DataAccess, []string{"files"}) {
		t.Fatalf("capabilities %+v", c)
	}
	if plan.ExecImage != nil {
		t.Fatalf("the Creator's plan builds an exec image: %+v", plan.ExecImage)
	}
}

// TestTheInstructionCarriesTheRules pins the rules the instruction must
// state, word for word where the application depends on them: the page
// path it picks pages up from, the citation forms its lint checks, the
// files it writes itself, and, since 1.2.0 (agentry-cli#523), the brief,
// the order knowledge comes from and the web's limits. That inputs are data
// is the System Prompt's, pinned in pkg/runner/compile. Whitespace is
// compared as one space, so rewrapping a line does not break the test.
func TestTheInstructionCarriesTheRules(t *testing.T) {
	raw, err := fs.ReadFile(FS(), "INSTRUCTION.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	for _, want := range []string{
		"`wiki/<section>/<slug>.md`",
		"`[src: doc:<document name>]`",
		"`[src: web:<url>]`",
		"`[src: model]`",
		"You never write those two files",
		"`documents.json`",
		"`changes.json`",
		"No credential in a page.",
		"*(unverified)*",
		// The wiki is written as it goes, so a run stopped at its budget
		// keeps the pages of what it read (agentry-cli#430).
		"**Write the wiki as you go.**",
		"write or rewrite the pages it supports before you read on",
		"Never read everything first and write at the end",
		"**Link only to pages that exist already**",
		"never a page you wrote in this run",
		// agentry-cli#523 (plan knowledge-and-drafter.md D6, D15, D17).
		"**The brief**",
		"`doc:brief.md` for the brief",
		"only for what the documents and the pack do not already hold",
		"Your own knowledge**, last",
		"cite only a page you read",
		"A result that AI Review withheld is not to be used.",
		"A claim with none of these behind it does not go in the pack.",
	} {
		if !strings.Contains(text, strings.Join(strings.Fields(want), " ")) {
			t.Errorf("the instruction does not say %q", want)
		}
	}
	// Nothing in it may grant itself what the application decides.
	for _, never := range []string{"registry_search", "fetch"} {
		if strings.Contains(text, never) {
			t.Errorf("the instruction names %s", never)
		}
	}
}
