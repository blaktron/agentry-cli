package compile

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/skill"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// MaxSkills is how many skills a persistent agent's plan carries.
const MaxSkills = 100

// checkedSkills holds a persistent agent's skills to the bounds, each name
// once: the plan is held to them whoever made its inputs.
func checkedSkills(in []spec.Skill) ([]spec.Skill, error) {
	if len(in) > MaxSkills {
		return nil, &PolicyRefusal{Reason: fmt.Sprintf("a persistent agent has at most %d skills on", MaxSkills)}
	}
	seen := map[string]bool{}
	var out []spec.Skill
	for _, s := range in {
		if !skill.NameValid(s.Name) || seen[s.Name] {
			return nil, &PolicyRefusal{Reason: "a skill's name is lower-case letters, digits and hyphens, at most 64 bytes, each name once: " + strconv.Quote(s.Name)}
		}
		if err := skill.DescriptionValid(s.Description); err != nil {
			return nil, &PolicyRefusal{Reason: err.Error() + ": " + strconv.Quote(s.Name)}
		}
		seen[s.Name] = true
		out = append(out, s)
	}
	return out, nil
}

// skillsLine is the System Prompt's line on a persistent agent's skills:
// each by name with its description quoted, so nothing in a description can
// pass for the line around it. commands says the agent runs programs, and so
// a skill's scripts. It names the folder by the path fs_read takes, beginning
// with the folder's name, as agentFilesLine names the agent's other folders.
func skillsLine(skills []spec.Skill, commands bool) string {
	if len(skills) == 0 {
		return ""
	}
	dir := spec.AgentSkillsDir
	return skillsParagraph("Read-only, in "+dir+"/ among your folders: each is a folder of instructions for one kind of work. "+
		"When a request matches a skill's description, read "+dir+"/<name>/SKILL.md with fs_read before you start, and follow it; read the other files it names when it says to. ",
		"Its scripts run with run_command, from where they are. ", commands, skills)
}

// skillsParagraph is the shape both skills lines share: how to read a
// skill, who runs its scripts, and the list.
func skillsParagraph(howToRead, scripts string, commands bool, skills []spec.Skill) string {
	b := new(strings.Builder)
	b.WriteString("- **Your skills.** " + howToRead)
	if commands {
		b.WriteString(scripts)
	} else {
		b.WriteString("You run no programs, so a skill's scripts are not yours to run: follow its instructions with your own tools. ")
	}
	b.WriteString("A skill guides how you work and gives you nothing you do not already have. The skills:\n")
	writeSkillList(b, skills)
	return b.String()
}

// writeSkillList lists skills one a line, each by name with its description
// quoted.
func writeSkillList(b *strings.Builder, skills []spec.Skill) {
	for _, s := range skills {
		b.WriteString("  - " + s.Name + ": " + strconv.Quote(s.Description) + "\n")
	}
}

// shippedSkillsLine is the System Prompt's line on the skills a release
// ships: read with skill_read, a step at a time, in the shape of a
// persistent agent's line. commands says the run has run_command.
func shippedSkillsLine(skills []spec.Skill, commands bool) string {
	if len(skills) == 0 {
		return ""
	}
	return skillsParagraph("This QuickAgent ships skills, read-only in "+spec.SkillsMount+"/: each is a folder of instructions for one kind of work. "+
		"When a step matches a skill's description, read the skill with skill_read before that step, and follow it; read the other files it names with skill_read's path when it says to. ",
		"Its scripts run with run_command only as one of your commands, from "+spec.SkillsMount+"/<name>/. ", commands, skills)
}

// shippedSkills reads the skills a release ships from its verified files:
// each folder under skills/ in workspaceDir, held to a skill's bounds, its
// SKILL.md naming it as its folder does. The registry refuses a release
// that breaks any of this at submit; a local package hears it here. The
// order is the folders' names, so the plan is the same each compile.
func shippedSkills(workspaceDir string) ([]spec.Skill, error) {
	dir := filepath.Join(workspaceDir, manifest.SkillsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("the release declares capabilities.skills and its %s/ folder does not read: %w", manifest.SkillsDir, err)
	}
	var out []spec.Skill
	for _, e := range entries {
		if !e.IsDir() {
			return nil, fmt.Errorf("the release ships %s/%s, which is not a skill's folder: each skill is %s/<name>/ with its %s", manifest.SkillsDir, e.Name(), manifest.SkillsDir, manifest.SkillFile)
		}
		meta, err := skill.Load(os.DirFS(filepath.Join(dir, e.Name())))
		if err != nil {
			return nil, fmt.Errorf("the skill %s/%s: %w", manifest.SkillsDir, e.Name(), err)
		}
		if meta.Name != e.Name() {
			return nil, fmt.Errorf("the skill %s/%s: its %s names it %q", manifest.SkillsDir, e.Name(), manifest.SkillFile, meta.Name)
		}
		out = append(out, spec.Skill{Name: meta.Name, Description: meta.Description})
	}
	switch {
	case len(out) == 0:
		return nil, fmt.Errorf("the release declares capabilities.skills and ships no skill in %s/", manifest.SkillsDir)
	case len(out) > manifest.MaxShippedSkills:
		return nil, fmt.Errorf("the release ships %d skills; the most is %d", len(out), manifest.MaxShippedSkills)
	}
	return out, nil
}

// MemorySkills are the skills the memory capability brings, which its
// compaction and consolidation turns name: given to any agent that
// declares capabilities.persistent.memory, whether or not it declares
// skills.
var MemorySkills = []string{"memory-compact", "memory-consolidate", "memory-ingest", "memory-query"}

// skillsDeclared refuses skills to a persistent agent that does not
// declare them, but for the memory's own skills to one that declares a
// memory.
func skillsDeclared(skills []spec.Skill, p *manifest.Persistent) error {
	if len(skills) == 0 || p.Declares("skills") {
		return nil
	}
	for _, s := range skills {
		if !p.Declares("memory") || !slices.Contains(MemorySkills, s.Name) {
			return &PolicyRefusal{Reason: "skills are granted to a persistent agent that declares capabilities.persistent.skills, and this one does not (one that declares memory is given the memory's own skills alone)"}
		}
	}
	return nil
}
