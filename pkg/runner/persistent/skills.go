package persistent

// A persistent agent's skills: Agent Skills folders it reads, read-only, at
// /agent/skills. Its built-in's own come with the binary; the person adds
// their own from a folder or a .zip on their computer, held to the skill
// bounds and run through the share's check before they are kept. Each skill
// is a roster entry. At each segment's start the supervisor fills a folder
// of the agent's own with the skills its roster has on, and nothing else,
// and the segment's tools server alone mounts it, read-only. A skill guides
// how the agent works; it grants nothing.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/skill"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// KindSkill is a skill's roster entry.
const KindSkill EntryKind = "skill"

// skillPrefix begins a skill's roster id.
const skillPrefix = "skill:"

// The agent's skill folders under its state folder: the skills the person
// added, as they were checked, and the segment's, filled at each start.
const (
	addedSkillsDir = "skills-added"
	skillsDir      = "skills"
)

// MaxAddedSkills is how many skills the person may add to an agent.
const MaxAddedSkills = 50

// How an added skill was checked when it was added.
const (
	SkillCheckedClamAV = "clamav"
	SkillUnchecked     = "unchecked"
)

// AddedSkill is a skill the person added: its name and description from its
// SKILL.md, the digest of its files as they were checked, how they were
// checked, and when.
type AddedSkill struct {
	Name        string    `yaml:"name" json:"name"`
	Description string    `yaml:"description" json:"description"`
	Digest      string    `yaml:"digest" json:"digest"`
	Check       string    `yaml:"check" json:"check"`
	Added       time.Time `yaml:"added" json:"added"`
}

// SkillEntryID is a skill's roster id.
func SkillEntryID(name string) string { return skillPrefix + name }

// AddedSkillsPath is where the agent's added skills are kept, one folder
// each.
func AddedSkillsPath(home, name string) string { return filepath.Join(Dir(home, name), addedSkillsDir) }

// skillsPath is the folder a segment's skills are put in.
func skillsPath(home, name string) string { return filepath.Join(Dir(home, name), skillsDir) }

// skillLineCut bounds the description a skill's consent sentence quotes.
const skillLineCut = 200

// skillEntries are the agent's skill entries: its built-in's, on by default
// and asking no agreement (they come with the agent and grant nothing),
// then the person's, each agreed to as its sentence says.
func skillEntries(a Agent, c Config) []RosterEntry {
	var out []RosterEntry
	for _, s := range a.Builtin.Skills() {
		out = append(out, RosterEntry{ID: SkillEntryID(s.Name), Kind: KindSkill, Name: s.Name, GrantedBy: GrantedByBuiltin,
			Line: "Follows the skill " + s.Name + ", built into Agentry, when a request matches it: " + strconv.Quote(cutText(s.Description, skillLineCut)), noConsent: true})
	}
	for _, s := range c.Skills {
		out = append(out, RosterEntry{ID: SkillEntryID(s.Name), Kind: KindSkill, Name: s.Name, GrantedBy: GrantedByYou, Line: addedSkillLine(a, s)})
	}
	return out
}

// addedSkillLine is the consent sentence of a skill the person added: theirs,
// not reviewed, how it was checked, and for an agent that runs programs,
// that its scripts can be run.
func addedSkillLine(a Agent, s AddedSkill) string {
	line := "Follows the skill " + s.Name + " you added yourself, when a request matches it: " + strconv.Quote(cutText(s.Description, skillLineCut)) +
		". Agentry has not reviewed it"
	if s.Check == SkillCheckedClamAV {
		line += ", and ClamAV found nothing in its files when you added it"
	} else {
		line += ", and its files were not checked for malware when you added it"
	}
	if a.Builtin.Codes() {
		line += "; its scripts run with the agent's programs, and reach what they reach"
	}
	return line
}

// stageSkills fills the agent's segment folder with the skills on says are
// on, and nothing else, and returns them as the plan lists them and the
// folder; none and "" when no skill is on, the folder then removed. An added
// skill whose files are not the ones checked when it was added is refused.
func stageSkills(home string, a Agent, c Config, on func(id string) bool) ([]spec.Skill, string, error) {
	dir := skillsPath(home, a.Name)
	if err := os.RemoveAll(dir); err != nil {
		return nil, "", fmt.Errorf("the %s agent's skills folder: %w", a.Name, err)
	}
	var skills []spec.Skill
	builtins := a.Builtin.Skills()
	for _, s := range builtins {
		if on(SkillEntryID(s.Name)) {
			skills = append(skills, spec.Skill{Name: s.Name, Description: s.Description})
		}
	}
	for _, s := range c.Skills {
		if on(SkillEntryID(s.Name)) {
			skills = append(skills, spec.Skill{Name: s.Name, Description: s.Description})
		}
	}
	if len(skills) == 0 {
		return nil, "", nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", fmt.Errorf("the %s agent's skills folder: %w", a.Name, err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return nil, "", err
	}
	for _, s := range builtins {
		if on(SkillEntryID(s.Name)) {
			if err := skill.CopyTo(s.FS, filepath.Join(dir, s.Name)); err != nil {
				return nil, "", fmt.Errorf("the skill %s: %w", s.Name, err)
			}
		}
	}
	for _, s := range c.Skills {
		if !on(SkillEntryID(s.Name)) {
			continue
		}
		src := filepath.Join(AddedSkillsPath(home, a.Name), s.Name)
		if d, err := skill.Digest(src); err != nil || d != s.Digest {
			return nil, "", fmt.Errorf("the skill %s you added is not as it was when it was checked: remove it and add it again (agentry agent skill remove %s %s)", s.Name, a.Name, s.Name)
		}
		if err := skill.CopyTo(os.DirFS(src), filepath.Join(dir, s.Name)); err != nil {
			return nil, "", fmt.Errorf("the skill %s: %w", s.Name, err)
		}
	}
	return skills, dir, nil
}

// SkillCheck checks a staged skill's files as the share's are checked; it
// says whether ClamAV checked every file and found nothing, and refuses a
// finding with an error.
type SkillCheck func(ctx context.Context, dir string) (clamav bool, err error)

// InstallSkill takes the skill at src, a folder or a .zip on this computer,
// holds it to the skill bounds, runs check over its files and keeps it
// among the agent's added skills, not yet on its roster: the change that
// adds it (Change.AddSkill) does that, and asks the person first. A skill of
// a name the agent has already is refused.
func InstallSkill(ctx context.Context, home string, a Agent, src string, check SkillCheck) (AddedSkill, error) {
	if a.DesktopOnly {
		return AddedSkill{}, fmt.Errorf("%s %w", a.Title, ErrDesktopOnly)
	}
	cfg, err := LoadConfig(home, a.Name)
	if err != nil {
		return AddedSkill{}, err
	}
	if len(cfg.Skills) >= MaxAddedSkills {
		return AddedSkill{}, fmt.Errorf("the %s agent has %d skills you added, the most it takes: remove one first", a.Name, MaxAddedSkills)
	}
	work := Dir(home, a.Name)
	if err := os.MkdirAll(work, 0o700); err != nil {
		return AddedSkill{}, err
	}
	staged, m, err := skill.Stage(src, work)
	if err != nil {
		return AddedSkill{}, err
	}
	defer func() { _ = os.RemoveAll(filepath.Dir(staged)) }()
	if err := skillNameFree(a, cfg, m.Name); err != nil {
		return AddedSkill{}, err
	}
	clamav, err := check(ctx, staged)
	if err != nil {
		return AddedSkill{}, fmt.Errorf("the skill %s was refused: %w", m.Name, err)
	}
	digest, err := skill.Digest(staged)
	if err != nil {
		return AddedSkill{}, err
	}
	kept := filepath.Join(AddedSkillsPath(home, a.Name), m.Name)
	if err := os.MkdirAll(filepath.Dir(kept), 0o700); err != nil {
		return AddedSkill{}, err
	}
	// A folder of that name not on the roster is what a refused add left.
	if err := os.RemoveAll(kept); err != nil {
		return AddedSkill{}, err
	}
	if err := os.Rename(staged, kept); err != nil {
		return AddedSkill{}, err
	}
	s := AddedSkill{Name: m.Name, Description: m.Description, Digest: digest, Check: SkillUnchecked, Added: time.Now().UTC()}
	if clamav {
		s.Check = SkillCheckedClamAV
	}
	return s, nil
}

// DiscardSkill removes an added skill's files once it is not on the
// agent's roster: after it is removed, or after the change that would have
// added it was refused.
func DiscardSkill(home string, a Agent, name string) error {
	if !skill.NameValid(name) {
		return fmt.Errorf("%q is not a skill's name", name)
	}
	cfg, err := LoadConfig(home, a.Name)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(cfg.Skills, func(s AddedSkill) bool { return s.Name == name }) {
		return fmt.Errorf("the %s agent still has the skill %s", a.Name, name)
	}
	return os.RemoveAll(filepath.Join(AddedSkillsPath(home, a.Name), name))
}

// skillNameFree refuses a name the agent has already, built in or added.
func skillNameFree(a Agent, c Config, name string) error {
	for _, s := range a.Builtin.Skills() {
		if s.Name == name {
			return fmt.Errorf("the %s agent has a skill %s built in: give yours another name", a.Name, name)
		}
	}
	if slices.ContainsFunc(c.Skills, func(s AddedSkill) bool { return s.Name == name }) {
		return fmt.Errorf("the %s agent already has the skill %s: remove it first to replace it", a.Name, name)
	}
	return nil
}

// addSkill and removeSkill are a Change's AddSkill and RemoveSkill on the
// next config, under the runner home home.
func addSkill(home string, a Agent, next *Config, s AddedSkill) error {
	switch {
	case !skill.NameValid(s.Name):
		return fmt.Errorf("%q is not a skill's name", s.Name)
	case skill.DescriptionValid(s.Description) != nil:
		return skill.DescriptionValid(s.Description)
	case s.Check != SkillCheckedClamAV && s.Check != SkillUnchecked:
		return errors.New("a skill is added as checked by ClamAV or unchecked")
	case len(next.Skills) >= MaxAddedSkills:
		return fmt.Errorf("the %s agent has %d skills you added, the most it takes", a.Name, MaxAddedSkills)
	}
	if err := skillNameFree(a, *next, s.Name); err != nil {
		return err
	}
	// The files InstallSkill kept are the ones the change names.
	if d, err := skill.Digest(filepath.Join(AddedSkillsPath(home, a.Name), s.Name)); err != nil || d != s.Digest {
		return fmt.Errorf("the skill %s's files are not in place: add it with agentry agent skill add", s.Name)
	}
	next.Skills = append(slices.Clone(next.Skills), s)
	delete(next.Switches, SkillEntryID(s.Name))
	return nil
}

func removeSkill(a Agent, next *Config, name string) error {
	for _, s := range a.Builtin.Skills() {
		if s.Name == name {
			return fmt.Errorf("the skill %s is built into the %s agent: switch it off instead (agentry agent grant %s %s off)", name, a.Name, a.Name, SkillEntryID(name))
		}
	}
	i := slices.IndexFunc(next.Skills, func(s AddedSkill) bool { return s.Name == name })
	if i < 0 {
		return fmt.Errorf("the %s agent has no skill %s you added", a.Name, name)
	}
	next.Skills = slices.Delete(slices.Clone(next.Skills), i, i+1)
	delete(next.Switches, SkillEntryID(name))
	delete(next.Consented, SkillEntryID(name))
	return nil
}
