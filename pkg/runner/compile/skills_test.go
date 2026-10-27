package compile

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// The skills line names the folder by the path fs_read takes, so a model
// that copies it reads the skill rather than being told it is outside its
// grant.
func TestSkillsLineNamesTheFolderAsFSReadTakesIt(t *testing.T) {
	line := skillsLine([]spec.Skill{{Name: "meeting-notes", Description: "Summarise a meeting."}}, false)
	for _, want := range []string{"Read-only, in skills/ among your folders", "read skills/<name>/SKILL.md with fs_read", `meeting-notes: "Summarise a meeting."`} {
		if !strings.Contains(line, want) {
			t.Errorf("skills line %q does not contain %q", line, want)
		}
	}
	if strings.Contains(line, spec.AgentRoot+"/") {
		t.Errorf("skills line %q names the folder by its full path too", line)
	}
	if skillsLine(nil, false) != "" {
		t.Error("no skills on still writes a skills line")
	}
}
