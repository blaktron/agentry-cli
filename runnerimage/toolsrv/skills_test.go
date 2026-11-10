package toolsrv

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// skillsSpec is a spec whose run ships two skills, triage with a file
// beside its SKILL.md and reproduce, under a folder of their own.
func skillsSpec(t *testing.T) *Spec {
	t.Helper()
	spec := baseSpec(t)
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"triage/" + SkillFile:    "---\nname: triage\ndescription: Sort an issue\n---\n\nRead the issue. Pick a label from labels.md.\n",
		"triage/labels.md":       "bug, feature, question\n",
		"reproduce/" + SkillFile: "Write the steps.\n",
		"other/" + SkillFile:     "---\nname: other\ndescription: Not listed\n---\nx\n",
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	spec.Skills = &Skills{Dir: dir, Names: []string{"reproduce", "triage"}}
	return spec
}

// TestSkillReadFollowsItsGrant: skill_read exists exactly when the spec
// lists a skill, and its description names each.
func TestSkillReadFollowsItsGrant(t *testing.T) {
	if slices.Contains(baseSpec(t).GrantedTools(), ToolSkillRead) {
		t.Fatal("no skills, but skill_read is granted")
	}
	s := newTestServer(t, skillsSpec(t))
	d, ok := s.tools[ToolSkillRead]
	if !ok {
		t.Fatal("skill_read is granted but not registered")
	}
	if !strings.Contains(d.description, "reproduce, triage") {
		t.Errorf("the description does not name the skills: %q", d.description)
	}
}

// TestSkillReadReadsASkillAndItsFiles: a name alone gives SKILL.md without
// its frontmatter; a path gives a file in the skill's folder; a skill with
// no frontmatter is given whole.
func TestSkillReadReadsASkillAndItsFiles(t *testing.T) {
	s := newTestServer(t, skillsSpec(t))
	got, isErr := call(t, s, ToolSkillRead, map[string]any{"name": "triage"})
	if isErr || !strings.HasPrefix(got, "Read the issue.") || strings.Contains(got, "description:") {
		t.Fatalf("skill_read triage = %q (error %v)", got, isErr)
	}
	got, isErr = call(t, s, ToolSkillRead, map[string]any{"name": "triage", "path": "labels.md"})
	if isErr || !strings.HasPrefix(got, "bug, feature, question") {
		t.Fatalf("skill_read triage labels.md = %q (error %v)", got, isErr)
	}
	got, isErr = call(t, s, ToolSkillRead, map[string]any{"name": "reproduce"})
	if isErr || !strings.HasPrefix(got, "Write the steps.") {
		t.Fatalf("skill_read reproduce = %q (error %v)", got, isErr)
	}
}

// TestSkillReadRefusesWhatIsNotTheRunsSkill: a name the run does not list
// (even one whose folder is there), a path out of the skill's folder, a
// folder, and a link out of it are refused.
func TestSkillReadRefusesWhatIsNotTheRunsSkill(t *testing.T) {
	spec := skillsSpec(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("not a skill"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink(outside, filepath.Join(spec.Skills.Dir, "triage", "link.md"))
	s := newTestServer(t, spec)
	for _, args := range []map[string]any{
		{"name": "other"},
		{"name": "../triage"},
		{"name": "triage", "path": "../reproduce/" + SkillFile},
		{"name": "triage", "path": "/etc/passwd"},
		{"name": "triage", "path": "link.md"},
		{"name": "triage", "path": "."},
		{"name": "triage", "path": "missing.md"},
	} {
		got, isErr := call(t, s, ToolSkillRead, args)
		if !isErr || strings.Contains(got, "not a skill") || strings.Contains(got, "Write the steps") {
			t.Errorf("skill_read %v = %q (error %v)", args, got, isErr)
		}
	}
}

// TestTheSkillsGrantIsHeldToItsShape: the grant names a clean absolute
// folder and skills by name, each once.
func TestTheSkillsGrantIsHeldToItsShape(t *testing.T) {
	for _, sk := range []*Skills{
		{Dir: "skills", Names: []string{"a"}},
		{Dir: "/skills/", Names: []string{"a"}},
		{Dir: "/skills"},
		{Dir: "/skills", Names: []string{"../a"}},
		{Dir: "/skills", Names: []string{"a", "a"}},
	} {
		if validateSkills(sk) == nil {
			t.Errorf("skills %+v pass", *sk)
		}
	}
	if err := validateSkills(&Skills{Dir: "/skills", Names: []string{"a", "b-c"}}); err != nil {
		t.Error(err)
	}
}

func TestWithoutFrontmatter(t *testing.T) {
	for in, want := range map[string]string{
		"---\nname: a\n---\nbody\n":             "body\n",
		"---\r\nname: a\r\n---\r\nbody":         "body",
		"\xef\xbb\xbf---\nname: a\n---\n\nbody": "body",
		"no block\n":                            "no block\n",
		"---\nname: a\nnever closed\n":          "---\nname: a\nnever closed\n",
	} {
		if got := withoutFrontmatter(in); got != want {
			t.Errorf("withoutFrontmatter(%q) = %q, want %q", in, got, want)
		}
	}
}
