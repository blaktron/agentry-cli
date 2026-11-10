package compile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

const skillsYAML = `schemaVersion: "2"
name: triage
namespace: demo
modes: [autonomous, interactive]
version: 1.0.0
summary: "Triages issues with the skills it ships"
capabilities:
  network:
    egress: none
  skills: true
publish:
  include: [agent.yaml, AGENT.md, skills/**]
`

// skillsFiles are a release's two shipped skills, one with a file beside
// its SKILL.md.
var skillsFiles = map[string]string{
	"skills/triage/SKILL.md":    "---\nname: triage\ndescription: Sort an issue into bug, feature or question\n---\nRead the issue. Pick a label from labels.md.\n",
	"skills/triage/labels.md":   "bug, feature, question\n",
	"skills/reproduce/SKILL.md": "---\nname: reproduce\ndescription: Reproduce a reported bug\n---\nWrite the steps.\n",
}

func toolSpecIn(t *testing.T, planDir string) toolsrv.Spec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(planDir, "gateway", "toolspec.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ts toolsrv.Spec
	if err := json.Unmarshal(raw, &ts); err != nil {
		t.Fatal(err)
	}
	return ts
}

// TestARunOfAReleaseThatShipsSkillsHasThem: the plan lists each skill the
// release ships, by its folder's name, in order; the tools server is given
// skill_read and the skills' folder, mounted read-only at /skills; and the
// System Prompt lists each skill and the rule to read one before its step,
// in both modes.
func TestARunOfAReleaseThatShipsSkillsHasThem(t *testing.T) {
	m := parseManifest(t, skillsYAML)
	ws := workspaceFor(t, m, skillsFiles)
	for _, mode := range []manifest.Mode{manifest.ModeAutonomous, manifest.ModeInteractive} {
		plan, dir := compileFixtureMode(t, m, ws, mode)
		want := []spec.Skill{{Name: "reproduce", Description: "Reproduce a reported bug"}, {Name: "triage", Description: "Sort an issue into bug, feature or question"}}
		if !slices.Equal(plan.Capabilities.ShippedSkills, want) {
			t.Fatalf("%s: the plan lists %+v", mode, plan.Capabilities.ShippedSkills)
		}
		ts := toolSpecIn(t, dir)
		if ts.Skills == nil || ts.Skills.Dir != spec.SkillsMount || !slices.Equal(ts.Skills.Names, []string{"reproduce", "triage"}) ||
			!slices.Contains(ts.GrantedTools(), toolsrv.ToolSkillRead) {
			t.Fatalf("%s: the tools spec gives %+v (granted: %v)", mode, ts.Skills, ts.GrantedTools())
		}
		tools, err := os.ReadFile(filepath.Join(dir, "gateway", "catalog.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(tools), spec.PHSkills+":"+spec.SkillsMount+":ro") {
			t.Errorf("%s: the tools server does not mount the skills read-only:\n%s", mode, tools)
		}
		got := renderedInstruction(t, dir, "docker-agent")
		for _, s := range []string{
			"- **Your skills.** This QuickAgent ships skills, read-only in /skills/",
			"read the skill with skill_read before that step",
			"You run no programs, so a skill's scripts are not yours to run",
			`  - reproduce: "Reproduce a reported bug"`,
			`  - triage: "Sort an issue into bug, feature or question"`,
		} {
			if !strings.Contains(got, s) {
				t.Errorf("%s: the System Prompt does not say %q", mode, s)
			}
		}
	}
}

// TestARunWithoutSkillsHearsOfNone: a release that declares no skills has
// no skill_read, no skills mount and no skills line, even with a skills/
// folder in its files, which the runner leaves unread.
func TestARunWithoutSkillsHearsOfNone(t *testing.T) {
	m := parseManifest(t, strings.Replace(skillsYAML, "  skills: true\n", "", 1))
	plan, dir := compileFixture(t, m, workspaceFor(t, m, skillsFiles))
	if len(plan.Capabilities.ShippedSkills) != 0 {
		t.Fatalf("the plan lists skills the release does not declare: %+v", plan.Capabilities.ShippedSkills)
	}
	if ts := toolSpecIn(t, dir); ts.Skills != nil || slices.Contains(ts.GrantedTools(), toolsrv.ToolSkillRead) {
		t.Fatalf("the tools spec gives skills: %+v", ts.Skills)
	}
	got := renderedInstruction(t, dir, "docker-agent")
	if strings.Contains(got, "skill_read") || strings.Contains(got, "Your skills") {
		t.Errorf("a run without skills hears of them:\n%s", got)
	}
	tools, err := os.ReadFile(filepath.Join(dir, "gateway", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(tools), spec.SkillsMount+":ro") {
		t.Error("a run without skills mounts /skills")
	}
}

// TestTheSkillsLineSaysWhoRunsAScript: a run with declared commands hears
// that a skill's scripts run only as one of them; one without, that they
// are not its to run.
func TestTheSkillsLineSaysWhoRunsAScript(t *testing.T) {
	m := parseManifest(t, strings.Replace(skillsYAML, "  skills: true\n", "  skills: true\n  exec: { commands: [grep] }\n", 1))
	_, dir := compileFixtureModeWith(t, m, workspaceFor(t, m, skillsFiles), manifest.ModeAutonomous, func(p *policy.Policy) {})
	got := renderedInstruction(t, dir, "docker-agent")
	if !strings.Contains(got, "Its scripts run with run_command only as one of your commands, from /skills/<name>/.") {
		t.Errorf("a run with commands is not told how a skill's scripts run:\n%s", got)
	}
}

// TestCompileRefusesSkillsTheRunCouldNotList: a release whose skills/
// folder is missing, empty, holds a file beside the folders, or a skill
// whose SKILL.md names another skill or has no description, is refused at
// compile, naming what is wrong.
func TestCompileRefusesSkillsTheRunCouldNotList(t *testing.T) {
	m := parseManifest(t, skillsYAML)
	for name, c := range map[string]struct {
		files map[string]string
		says  string
	}{
		"no folder":           {map[string]string{}, "skills/ folder does not read"},
		"a file beside":       {map[string]string{"skills/README.md": "x", "skills/triage/SKILL.md": skillsFiles["skills/triage/SKILL.md"]}, "skills/README.md, which is not a skill's folder"},
		"a name not its own":  {map[string]string{"skills/triage/SKILL.md": "---\nname: sort\ndescription: Sort\n---\nx"}, `names it "sort"`},
		"no description":      {map[string]string{"skills/triage/SKILL.md": "---\nname: triage\n---\nx"}, "description"},
		"no SKILL.md at all":  {map[string]string{"skills/triage/labels.md": "x"}, "SKILL.md"},
		"an empty skills dir": {map[string]string{"skills/.keep/x": ""}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			ws := workspaceFor(t, m, c.files)
			if name == "an empty skills dir" {
				_ = os.RemoveAll(filepath.Join(ws, "skills", ".keep"))
				c.says = "ships no skill"
			}
			_, err := Compile(Inputs{Manifest: m, Digest: "sha256:0123456789abcdef", Source: "local", WorkspaceDir: ws,
				Policy: testPolicy(), PolicySHA: "fixedpolicysha", RunnerImage: "agentry-runner:test"}, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("compile = %v, want it to say %q", err, c.says)
			}
		})
	}

	// A link where a skill's folder should be is not followed.
	ws := workspaceFor(t, m, nil)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "SKILL.md"), []byte(skillsFiles["skills/triage/SKILL.md"]), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "skills", "triage")); err != nil {
		t.Skip("no symlinks here")
	}
	_, err := Compile(Inputs{Manifest: m, Digest: "sha256:0123456789abcdef", Source: "local", WorkspaceDir: ws,
		Policy: testPolicy(), PolicySHA: "fixedpolicysha", RunnerImage: "agentry-runner:test"}, t.TempDir())
	if err == nil {
		t.Fatal("a skill folder that is a link compiles")
	}
}
