package toolsrv

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Skills are the skills a release ships, as the tools server reads them:
// Dir holds one folder a skill, each with its SkillFile, mounted read-only,
// and Names are the skills the run lists, the only names skill_read opens.
type Skills struct {
	Dir   string   `json:"dir"`
	Names []string `json:"names"`
}

// SkillFile is the file that makes a folder a skill: its instructions,
// opening with frontmatter that names it and says when to use it.
const SkillFile = "SKILL.md"

// skillName is a skill's name, which is also its folder's: lower-case
// letters and digits in words joined by single hyphens.
var skillName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// validateSkills holds the skills grant to an absolute folder and names
// that are each one folder beneath it, once.
func validateSkills(sk *Skills) error {
	if sk == nil {
		return nil
	}
	if !filepath.IsAbs(sk.Dir) || filepath.Clean(sk.Dir) != sk.Dir || len(sk.Names) == 0 {
		return fmt.Errorf("skills %+v must name a clean absolute folder and at least one skill", *sk)
	}
	seen := map[string]bool{}
	for _, n := range sk.Names {
		if len(n) > 64 || !skillName.MatchString(n) || seen[n] {
			return fmt.Errorf("skills: %q is not a skill's name, or is named twice", n)
		}
		seen[n] = true
	}
	return nil
}

func (s *server) skillReadDescription() string {
	return "Read one of the skills this QuickAgent ships: with name alone, the skill's " + SkillFile +
		" (its instructions, without the frontmatter); with path, a file the instructions name, in the skill's own folder. " +
		"Read a skill before the step its description matches, and follow it. The skills: " +
		strings.Join(s.spec.Skills.Names, ", ") + "."
}

func skillReadSchema() map[string]any {
	return objSchema(map[string]any{
		"name":     strProp("the skill's name, as the list of skills gives it"),
		"path":     strProp("a file in the skill's folder, relative to it, as the skill names it; omit it to read " + SkillFile),
		"offset":   intProp("byte offset to start reading from (default 0)"),
		"maxBytes": intProp("maximum bytes to return (default and cap: the server read cap)"),
	}, "name")
}

func (s *server) toolSkillRead(raw json.RawMessage) (string, bool) {
	var a struct {
		Name     string `json:"name"`
		Path     string `json:"path"`
		Offset   int64  `json:"offset"`
		MaxBytes int64  `json:"maxBytes"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return ToolSkillRead + ": " + err.Error(), true
	}
	if a.Offset < 0 {
		return ToolSkillRead + ": offset must be >= 0", true
	}
	sk := s.spec.Skills
	if !slices.Contains(sk.Names, a.Name) {
		return fmt.Sprintf("%s: %q is not one of this QuickAgent's skills (%s)", ToolSkillRead, a.Name, strings.Join(sk.Names, ", ")), true
	}
	rel := a.Path
	if rel == "" {
		rel = SkillFile
	}
	abs, err := resolveUnder(filepath.Join(sk.Dir, a.Name), rel)
	if err != nil {
		return ToolSkillRead + ": " + err.Error(), true
	}
	s.checked(sk.Dir)
	if fi, err := os.Stat(abs); err != nil {
		return fmt.Sprintf("%s: the skill %s has no file %s", ToolSkillRead, a.Name, path.Clean(rel)), true
	} else if fi.IsDir() {
		return fmt.Sprintf("%s: %s is a folder in the skill %s; name a file", ToolSkillRead, path.Clean(rel), a.Name), true
	}
	text, isErr, err := readPaged(abs, a.Offset, a.MaxBytes, s.spec.Limits.readCap(), s.spec.Limits.budget(), "")
	if err != nil {
		return ToolSkillRead + ": " + err.Error(), true
	}
	if !isErr && a.Path == "" && a.Offset == 0 {
		text = withoutFrontmatter(text)
	}
	return text, isErr
}

// withoutFrontmatter drops the YAML block a skill file opens with: the
// skill's name and description, which the System Prompt already lists.
// Text that does not open with a complete block is returned as it is.
func withoutFrontmatter(text string) string {
	s := strings.TrimPrefix(text, "\xef\xbb\xbf")
	nl := "\n"
	switch {
	case strings.HasPrefix(s, "---\r\n"):
		nl = "\r\n"
	case !strings.HasPrefix(s, "---\n"):
		return text
	}
	rest := s[len("---"+nl):]
	for off := 0; off < len(rest); {
		end := strings.Index(rest[off:], nl)
		line := rest[off:]
		if end >= 0 {
			line = rest[off : off+end]
		}
		if line == "---" || line == "..." {
			if end < 0 {
				return ""
			}
			return strings.TrimLeft(rest[off+end+len(nl):], "\r\n")
		}
		if end < 0 {
			break
		}
		off += end + len(nl)
	}
	return text
}
