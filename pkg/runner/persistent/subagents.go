package persistent

// A persistent agent's subagents: the QuickAgents it may start as runs of
// their own, each a roster entry. The ones its release declares are pinned
// at its install (SubagentPin) and granted by the release; the ones the
// person adds from the QuickAgents installed on this computer are theirs
// (AddedSubagent). An entry is on with the agent unless the person turns it
// off, and each has a second switch, off by default, that asks the person
// before each run the agent starts of it. Both are read when a run is
// asked for, so switching either restarts nothing. An agent installed from
// an author's folder has none.

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
)

// KindSubagent is the kind of a subagent's entries.
const KindSubagent EntryKind = "subagent"

// subagentPrefix starts a subagent entry's id, and subagentAskSuffix ends
// its ask-before-each-run switch's.
const (
	subagentPrefix    = "subagent:"
	subagentAskSuffix = ":ask"
)

// SubagentEntryID is a subagent's entry id: "subagent:<ns>/<name>".
func SubagentEntryID(ref string) string { return subagentPrefix + ref }

// SubagentAskID is the id of a subagent's switch that asks before each run.
func SubagentAskID(ref string) string { return SubagentEntryID(ref) + subagentAskSuffix }

// isSubagentEntry says id is a subagent's entry or its ask switch.
func isSubagentEntry(id string) bool { return strings.HasPrefix(id, subagentPrefix) }

// AddedSubagent is a subagent the person added: an autonomous QuickAgent
// installed on this computer, at the version it was installed at when it
// was added, with what its contract reaches in a line.
type AddedSubagent struct {
	Ref     string `yaml:"ref" json:"ref"`
	Version string `yaml:"version" json:"version"`
	Reach   string `yaml:"reach,omitempty" json:"reach,omitempty"`
}

// Subagent is one subagent of an agent as its roster names it.
type Subagent struct {
	Ref, Version, Why, Reach string
	// GrantedBy is GrantedByRelease for one the release pins, GrantedByYou
	// for one the person added.
	GrantedBy string
}

// Exact is the subagent's ns/name@version.
func (s Subagent) Exact() string { return s.Ref + "@" + s.Version }

// Subagents are the agent's subagents: those its release pins to a version,
// then those the person added under a reference the release does not use.
// An agent from an author's folder has none.
func Subagents(a Agent, c Config) []Subagent {
	if a.Release == nil || a.Source == SourceLocal {
		return nil
	}
	var out []Subagent
	for _, p := range a.Subagents {
		if p.Version == "" {
			continue
		}
		out = append(out, Subagent{Ref: p.Ref, Version: p.Version, Why: p.Why, Reach: p.Reach, GrantedBy: GrantedByRelease})
	}
	for _, s := range c.Subagents {
		if slices.ContainsFunc(out, func(o Subagent) bool { return o.Ref == s.Ref }) {
			continue
		}
		out = append(out, Subagent{Ref: s.Ref, Version: s.Version, Reach: s.Reach, GrantedBy: GrantedByYou})
	}
	return out
}

// SubagentNamed is the agent's subagent ref, if it has one.
func SubagentNamed(a Agent, c Config, ref string) (Subagent, bool) {
	i := slices.IndexFunc(Subagents(a, c), func(s Subagent) bool { return s.Ref == ref })
	if i < 0 {
		return Subagent{}, false
	}
	return Subagents(a, c)[i], true
}

// AppLinkWith is the agent's app link tool set: its declared parts, and the
// subagent tools when its roster names a subagent.
func (a Agent) AppLinkWith(c Config) string {
	set := compile.AppLinkSet(a.Declared())
	if len(Subagents(a, c)) == 0 {
		return set
	}
	return applink.Compose(append(applink.SetParts(set), applink.PartSubagents)...)
}

// subagentLine is a subagent entry's consent sentence.
func subagentLine(s Subagent) string {
	reach := strings.TrimSpace(s.Reach)
	if reach == "" {
		reach = "its contract as its own install consent lists it"
	}
	line := "Starts runs of " + s.Exact() + " when it asks, each an ordinary run on this computer in a sandbox of its own under that QuickAgent's contract (" + reach +
		"), given only the task, the answers and the files it names from its share and its own files, never its memory, secrets or skills; what the run answers and the files it makes come back into its own files"
	if w := strings.TrimSpace(s.Why); w != "" {
		line += ". Its publisher says why: " + strconv.Quote(w)
	}
	return line
}

// subagentEntries are the roster's entries for the agent's subagents: each
// one, then its switch that asks before each run (subagentDefault says
// which is on by default).
func subagentEntries(a Agent, c Config) []RosterEntry {
	var out []RosterEntry
	for _, s := range Subagents(a, c) {
		out = append(out,
			RosterEntry{ID: SubagentEntryID(s.Ref), Kind: KindSubagent, Name: s.Exact(), GrantedBy: s.GrantedBy, Line: subagentLine(s)},
			RosterEntry{ID: SubagentAskID(s.Ref), Kind: KindSubagent, Name: "Ask before each run of " + s.Ref, GrantedBy: GrantedByYou, noConsent: true,
				Line: "Asks you before each run of " + s.Exact() + " it starts, naming the task and the files"})
	}
	return out
}

// subagentDefault is a subagent entry's switch by default: a subagent is on
// with the agent, and asking before each run is off.
func subagentDefault(id string) bool { return !strings.HasSuffix(id, subagentAskSuffix) }

// subagentOn says the roster has the subagent ref on, as the person agreed
// to its sentence, and whether it asks before each run.
func subagentOn(a Agent, c Config, ref string) (on, ask bool) {
	r := BuildRoster(a, c, nil)
	e, ok := r.Entry(SubagentEntryID(ref))
	if !ok || !e.On || slices.ContainsFunc(Unconsented(r, c), func(u RosterEntry) bool { return u.ID == e.ID }) {
		return false, false
	}
	if q, ok := r.Entry(SubagentAskID(ref)); ok && q.On {
		ask = true
	}
	return true, ask
}

// subagentRef is a QuickAgent's ns/name, and subagentVersion an exact
// version.
var (
	subagentRef     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}/[a-z0-9][a-z0-9-]{0,63}$`)
	subagentVersion = regexp.MustCompile(`^[0-9A-Za-z.+\-]{1,64}$`)
)

// maxAddedSubagents bounds the subagents a person adds to one agent.
const maxAddedSubagents = 16

// addSubagent adds sa to c, for an agent installed from the registry.
func addSubagent(a Agent, c *Config, sa AddedSubagent) error {
	switch {
	case a.Release == nil || a.Source == SourceLocal:
		return fmt.Errorf("the %s agent takes no subagents: only a persistent agent installed from the registry runs them", a.Name)
	case !subagentRef.MatchString(sa.Ref) || !subagentVersion.MatchString(sa.Version):
		return fmt.Errorf("%q is not a QuickAgent's namespace/name at a version", sa.Ref+"@"+sa.Version)
	case sa.Ref == a.Ref:
		return fmt.Errorf("the %s agent is not a subagent of itself", a.Name)
	case slices.ContainsFunc(Subagents(a, *c), func(s Subagent) bool { return s.Ref == sa.Ref }):
		return fmt.Errorf("the %s agent already has the subagent %s; remove it first to change its version", a.Name, sa.Ref)
	case len(c.Subagents) >= maxAddedSubagents:
		return fmt.Errorf("the %s agent has %d subagents you added, the most it takes", a.Name, maxAddedSubagents)
	}
	sa.Reach = strings.TrimSpace(sa.Reach)
	c.Subagents = append(c.Subagents, sa)
	delete(c.Switches, SubagentEntryID(sa.Ref))
	delete(c.Switches, SubagentAskID(sa.Ref))
	return nil
}

// removeSubagent takes the subagent ref the person added away from c.
func removeSubagent(a Agent, c *Config, ref string) error {
	i := slices.IndexFunc(c.Subagents, func(s AddedSubagent) bool { return s.Ref == ref })
	if i < 0 {
		if slices.ContainsFunc(a.Subagents, func(p SubagentPin) bool { return p.Ref == ref }) {
			return fmt.Errorf("%s comes with the %s agent's release and cannot be removed: switch it off (agentry agent grant %s %s off)", ref, a.Name, a.Name, SubagentEntryID(ref))
		}
		return fmt.Errorf("the %s agent has no subagent %s you added", a.Name, ref)
	}
	c.Subagents = slices.Delete(c.Subagents, i, i+1)
	for _, id := range []string{SubagentEntryID(ref), SubagentAskID(ref)} {
		delete(c.Switches, id)
		delete(c.Consented, id)
	}
	return nil
}
