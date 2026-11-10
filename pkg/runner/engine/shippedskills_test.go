package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// TestARunIsGivenTheReleasesOwnSkillsFolder: a plan that lists shipped
// skills is given the skills/ folder of the verified tree its workspace is
// copied from, never the run's copy; a plan with none is given no folder;
// a skills/ that is a link, or missing, refuses the run.
func TestARunIsGivenTheReleasesOwnSkillsFolder(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "skills", "triage"), 0o755); err != nil {
		t.Fatal(err)
	}
	plan := &spec.Plan{Capabilities: spec.CapSummary{ShippedSkills: []spec.Skill{{Name: "triage", Description: "Sort an issue"}}}}

	rd := &runDirs{workspace: filepath.Join(t.TempDir(), "workspace")}
	if err := preparePersistentFolders(Params{Plan: plan, WorkspaceSrc: src}, rd); err != nil {
		t.Fatal(err)
	}
	if rd.skills != filepath.Join(src, "skills") {
		t.Fatalf("the run's skills folder is %q, want the verified tree's", rd.skills)
	}

	rd = &runDirs{}
	if err := preparePersistentFolders(Params{Plan: &spec.Plan{}, WorkspaceSrc: src}, rd); err != nil || rd.skills != "" {
		t.Fatalf("a plan without skills is given %q (%v)", rd.skills, err)
	}

	linked := t.TempDir()
	if err := os.Symlink(filepath.Join(src, "skills"), filepath.Join(linked, "skills")); err == nil {
		if err := preparePersistentFolders(Params{Plan: plan, WorkspaceSrc: linked}, &runDirs{}); err == nil {
			t.Error("a skills folder reached through a link is given")
		}
	}
	if err := preparePersistentFolders(Params{Plan: plan, WorkspaceSrc: t.TempDir()}, &runDirs{}); err == nil {
		t.Error("a release with no skills folder is given one")
	}
	if err := preparePersistentFolders(Params{Plan: plan}, &runDirs{}); err == nil {
		t.Error("a plan with skills runs with no release files")
	}
}
