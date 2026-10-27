package compile

import (
	"fmt"
	"strconv"
	"strings"

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
	b := new(strings.Builder)
	b.WriteString("- **Your skills.** Read-only, in " + dir + "/ among your folders: each is a folder of instructions for one kind of work. " +
		"When a request matches a skill's description, read " + dir + "/<name>/SKILL.md with fs_read before you start, and follow it; read the other files it names when it says to. ")
	if commands {
		b.WriteString("Its scripts run with run_command, from where they are. ")
	} else {
		b.WriteString("You run no programs, so a skill's scripts are not yours to run: follow its instructions with your own tools. ")
	}
	b.WriteString("A skill guides how you work and gives you nothing you do not already have. The skills:\n")
	for _, s := range skills {
		b.WriteString("  - " + s.Name + ": " + strconv.Quote(s.Description) + "\n")
	}
	return b.String()
}
