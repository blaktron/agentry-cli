package persistent

import (
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
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

// AgentSkill is one of an agent's own skills and who grants it: the
// application, for a built-in's and for the memory's own skills, or its
// release.
type AgentSkill struct {
	localrun.BuiltinSkill
	GrantedBy string
}

// Skills are the agent's own skills: for an agent that takes skills, its
// built-in's tree's or the ones its release ships under skills/; and for a
// published agent that keeps a memory, the memory's own skills
// (compile.MemorySkills), from Agentry's tree, which its compaction and
// consolidation follow; a release's skill of one of their names is left
// out. A release's are read from its folder, which a segment's start holds
// to its integrity record before anything it staged is mounted.
func (a Agent) Skills() []AgentSkill {
	var out []AgentSkill
	if a.Release == nil {
		if a.TakesSkills() {
			for _, s := range a.Builtin.Skills() {
				out = append(out, AgentSkill{s, GrantedByBuiltin})
			}
		}
		return out
	}
	memory := a.KeepsMemory()
	if a.TakesSkills() {
		for _, s := range a.releaseSkills() {
			if !memory || !slices.Contains(compile.MemorySkills, s.Name) {
				out = append(out, AgentSkill{s, GrantedByRelease})
			}
		}
	}
	if memory {
		for _, s := range localrun.Assistant().Skills() {
			if slices.Contains(compile.MemorySkills, s.Name) {
				out = append(out, AgentSkill{s, GrantedByBuiltin})
			}
		}
	}
	return out
}

// releaseSkills are the skills a published agent's release ships, read
// once per release folder.
func (a Agent) releaseSkills() []localrun.BuiltinSkill {
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

// memorySchema is the file of a memory's seed that says how the memory is
// kept, which the memory's skills follow.
const memorySchema = "SCHEMA.md"

// MemorySeed is what the agent's memory volume starts with, for an agent
// that keeps a memory: a built-in's tree's memory/ folder; for a published
// agent, Agentry's memory seed (its schema, index and log) with the files
// of its release's memory/ laid over it, but for the schema, which stays
// Agentry's so the memory's skills can keep it.
func (a Agent) MemorySeed() (map[string][]byte, error) {
	if !a.KeepsMemory() {
		return nil, nil
	}
	if a.Release == nil {
		return a.Builtin.MemorySeed()
	}
	out, err := localrun.Assistant().MemorySeed()
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(filepath.Join(a.Release.Dir, "workspace"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	own, err := localrun.MemorySeedOf(root.FS(), a.Release.Meta.Ref)
	if err != nil {
		return nil, err
	}
	for p, b := range own {
		if p != memorySchema {
			out[p] = b
		}
	}
	return out, nil
}

// grantedBy is who grants what the agent declares: its release for a
// published agent, the application for a built-in.
func (a Agent) grantedBy() string {
	if a.Published() {
		return GrantedByRelease
	}
	return GrantedByBuiltin
}

// Codes says the agent declares the coding toolchain (coder): any program
// in the coder exec image, open egress, the public browser, a dev host,
// ports and git host accounts, each a switch of its roster.
func (a Agent) Codes() bool { return a.Declared().Declares("coder") }

// ReadsCalendars says the agent declares calendars: the person may give it
// calendar feeds.
func (a Agent) ReadsCalendars() bool { return a.Declared().Declares("calendars") }

// OfferedServers are the MCP servers the agent comes with, each an entry of
// its roster: a built-in's package's own (some drawn for its git host
// accounts, hosts); a published agent's, the servers of the mail providers
// its release declares that are reached through a server of their own.
func (a Agent) OfferedServers(hosts []offer.GitHost) []offer.Server {
	if a.Release == nil {
		return a.Builtin.OfferedServers(hosts)
	}
	return localrun.MailServersFor(a.Caps)
}

// ImageServers are the servers run from an image the CLI pins that the agent
// declares, at the policy's images.
func (a Agent) ImageServers(images policy.Images) []offer.ImageServer {
	return localrun.ImageServersFor(a.Declared(), images)
}

// MailProviders are the mail providers the agent declares.
func (a Agent) MailProviders() []offer.MailProvider { return localrun.MailProvidersFor(a.Declared()) }

// SuggestedServers are the MCP servers a built-in's package suggests; a
// published agent suggests none.
func (a Agent) SuggestedServers() []offer.Suggestion {
	if a.Release == nil {
		return a.Builtin.SuggestedServers()
	}
	return nil
}
