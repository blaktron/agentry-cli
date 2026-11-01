package persistent

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry/pkg/manifest"
)

// Declared is what the agent declares under capabilities.persistent: its
// release's block for a published agent, its own manifest's for a built-in.
// Each capability below is reached only by declaring it, whatever the
// agent's source; nil declares nothing.
func (a Agent) Declared() *manifest.Persistent {
	if a.Release != nil {
		return a.Caps
	}
	return a.Builtin.Declared()
}

// KeepsTime says the agent is told the time at each turn and keeps a
// heartbeat that fires the tasks it registers (clock).
func (a Agent) KeepsTime() bool { return a.Declared().Declares("clock") }

// KeepsMemory says the agent has a memory volume of its own, with
// compaction, the daily consolidation and recall (memory).
func (a Agent) KeepsMemory() bool { return a.Declared().Declares("memory") }

// KeepsSecrets says the person may store secrets for the agent, each a
// roster entry (secrets).
func (a Agent) KeepsSecrets() bool { return a.Declared().Declares("secrets") }

// TakesSkills says the agent follows skills: its package's own and the
// ones the person adds (skills).
func (a Agent) TakesSkills() bool { return a.Declared().Declares("skills") }

// ConfiguresItself says the agent may save its own name and persona
// through self_configure (selfConfigure).
func (a Agent) ConfiguresItself() bool { return a.Declared().Declares("selfConfigure") }

// ArmsMeetings says the agent's app link has the meeting tools, through
// which it arms Agentry Desktop to record a meeting, and its roster the
// switch that lets it (meetings).
func (a Agent) ArmsMeetings() bool { return a.Declared().Declares("meetings") }

// AppLink is the agent's app link tool set, composed from what it declares;
// empty for none.
func (a Agent) AppLink() string { return compile.AppLinkSet(a.Declared()) }

// Skills are the agent's own skills, for an agent that takes skills: its
// built-in's tree's, or the ones its release ships under skills/. A
// release's are read from its folder, which a segment's start holds to its
// integrity record before anything it staged is mounted.
func (a Agent) Skills() []localrun.BuiltinSkill {
	if !a.TakesSkills() {
		return nil
	}
	if a.Release == nil {
		return a.Builtin.Skills()
	}
	key := a.Release.Dir
	if v, ok := releaseSkills.Load(key); ok {
		return v.([]localrun.BuiltinSkill)
	}
	// Read through the folder's root, so a link in the release is not
	// followed out of it; the root stays open with the cached skills.
	root, err := os.OpenRoot(filepath.Join(a.Release.Dir, "workspace"))
	if err != nil {
		return nil
	}
	out := localrun.SkillsOf(root.FS())
	if v, loaded := releaseSkills.LoadOrStore(key, out); loaded {
		_ = root.Close()
		return v.([]localrun.BuiltinSkill)
	}
	return out
}

// releaseSkills are each installed release's skills by its release folder,
// which is named by the release's digest, so it never changes under it.
var releaseSkills sync.Map

// MemorySeed is what the agent's memory volume starts with, for an agent
// that keeps a memory: its tree's memory/ folder.
func (a Agent) MemorySeed() (map[string][]byte, error) {
	if !a.KeepsMemory() {
		return nil, nil
	}
	if a.Release == nil {
		return a.Builtin.MemorySeed()
	}
	root, err := os.OpenRoot(filepath.Join(a.Release.Dir, "workspace"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return localrun.MemorySeedOf(root.FS(), a.Release.Meta.Ref)
}

// grantedBy is who grants what the agent declares: its release for a
// published agent, the application for a built-in.
func (a Agent) grantedBy() string {
	if a.Published() {
		return GrantedByRelease
	}
	return GrantedByBuiltin
}
