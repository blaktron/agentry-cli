package persistent

// A change to an agent's configuration: its name and personality, its share
// folder, the servers and packs the person added, a roster switch. One
// function applies a change to a Config, whoever makes it: the CLI on a
// stopped agent, holding its lock, and the supervisor of a running one,
// which the change reaches over the owner socket. A change that turns on
// something the person has not consented to on this machine is refused
// until they have.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
)

// Change is one change to an agent's configuration; a field left unset is
// left as it is.
type Change struct {
	// Name and Personality are the person's for the agent; "" clears one.
	Name        *string `json:"name,omitempty"`
	Personality *string `json:"personality,omitempty"`
	// Share is the share folder, absolute; "" is the agent's default.
	Share *string `json:"share,omitempty"`
	// RunOnStart starts the agent when its client starts.
	RunOnStart *bool `json:"runOnStart,omitempty"`
	// AddServer adds an MCP server, on; RemoveServer removes one by its
	// roster id or its store id.
	AddServer    *manifest.MCPServer `json:"addServer,omitempty"`
	RemoveServer string              `json:"removeServer,omitempty"`
	// AddKnowledge adds a pack of the local store by name, on;
	// RemoveKnowledge removes one.
	AddKnowledge    string `json:"addKnowledge,omitempty"`
	RemoveKnowledge string `json:"removeKnowledge,omitempty"`
	// Switch turns a roster entry on or off.
	Switch *Switch `json:"switch,omitempty"`
	// Consent says the person consented to what this change turns on, as
	// the lines a ConsentError listed say it.
	Consent bool `json:"consent,omitempty"`
}

// Switch is a roster switch.
type Switch struct {
	ID string `json:"id"`
	On bool   `json:"on"`
}

// The bounds of what a person writes for the agent.
const (
	MaxNameBytes        = 64
	MaxPersonalityBytes = 32 << 10
)

// ConsentError refuses a change, or a start, that turns on entries the
// person has not consented to on this machine: Entries are their lines.
type ConsentError struct {
	Agent   string
	Entries []RosterEntry
}

func (e *ConsentError) Error() string {
	lines := make([]string, 0, len(e.Entries))
	for _, en := range e.Entries {
		lines = append(lines, en.Line)
	}
	return fmt.Sprintf("the %s agent would be given what you have not agreed to on this computer: %s", e.Agent, strings.Join(lines, "; "))
}

// Unconsented are the roster's entries that are on and whose consent
// sentence the person has not agreed to as it stands.
func Unconsented(r Roster, c Config) []RosterEntry {
	var out []RosterEntry
	for _, e := range r {
		if e.On && !e.Fixed && c.Consented[e.ID] != e.Line {
			out = append(out, e)
		}
	}
	return out
}

// RecordConsent records the person's agreement to every entry that is on.
func RecordConsent(r Roster, c *Config) {
	if c.Consented == nil {
		c.Consented = map[string]string{}
	}
	for _, e := range r {
		if e.On && !e.Fixed {
			c.Consented[e.ID] = e.Line
		}
	}
}

// ApplyChange applies ch to c for the agent a, under the runner home home.
// It answers whether the change alters what a segment is given, so a
// running agent's next turn starts a new segment, and refuses with a
// ConsentError a change that turns on what the person has not agreed to,
// unless ch.Consent says they have.
func ApplyChange(home string, a Agent, c *Config, ch Change) (restart bool, err error) {
	if a.DesktopOnly {
		return false, fmt.Errorf("%s %w: its configuration is Agentry Desktop's", a.Title, ErrDesktopOnly)
	}
	next := *c
	next.Switches = cloneMap(c.Switches)
	next.Consented = cloneMap(c.Consented)
	next.Servers = slices.Clone(c.Servers)
	next.Knowledge = slices.Clone(c.Knowledge)
	if ch.Name != nil {
		name, err := cleanName(*ch.Name)
		if err != nil {
			return false, err
		}
		next.DisplayName = name
	}
	if ch.Personality != nil {
		if len(*ch.Personality) > MaxPersonalityBytes || !utf8.ValidString(*ch.Personality) {
			return false, fmt.Errorf("a personality is UTF-8 text of at most %d KiB", MaxPersonalityBytes>>10)
		}
		next.Personality = strings.TrimSpace(*ch.Personality)
	}
	if ch.RunOnStart != nil {
		next.RunOnStart = *ch.RunOnStart
	}
	if ch.Share != nil {
		if *ch.Share != "" {
			if err := CheckShareFolder(home, *ch.Share); err != nil {
				return false, err
			}
		}
		next.Share = *ch.Share
	}
	if srv := ch.AddServer; srv != nil {
		if err := checkServer(*srv); err != nil {
			return false, err
		}
		id := ServerEntryID(*srv)
		if slices.ContainsFunc(next.Servers, func(s manifest.MCPServer) bool { return ServerEntryID(s) == id }) {
			return false, fmt.Errorf("the %s agent already has the server %s; remove it first to change it", a.Name, serverName(*srv))
		}
		next.Servers = append(next.Servers, *srv)
		delete(next.Switches, id)
	}
	if id := ch.RemoveServer; id != "" {
		if !strings.HasPrefix(id, serverPrefix) {
			id = serverPrefix + id
		}
		i := slices.IndexFunc(next.Servers, func(s manifest.MCPServer) bool { return ServerEntryID(s) == id })
		if i < 0 {
			return false, fmt.Errorf("the %s agent has no server %s", a.Name, strings.TrimPrefix(id, serverPrefix))
		}
		next.Servers = slices.Delete(next.Servers, i, i+1)
		delete(next.Switches, id)
		delete(next.Consented, id)
	}
	if k := ch.AddKnowledge; k != "" {
		if err := knowledge.ValidName(k); err != nil {
			return false, err
		}
		if slices.Contains(next.Knowledge, k) {
			return false, fmt.Errorf("the %s agent already searches the pack %s", a.Name, k)
		}
		next.Knowledge = append(next.Knowledge, k)
		delete(next.Switches, KnowledgeEntryID(k))
	}
	if k := ch.RemoveKnowledge; k != "" {
		i := slices.Index(next.Knowledge, k)
		if i < 0 {
			return false, fmt.Errorf("the %s agent has no pack %s", a.Name, k)
		}
		next.Knowledge = slices.Delete(next.Knowledge, i, i+1)
		delete(next.Switches, KnowledgeEntryID(k))
		delete(next.Consented, KnowledgeEntryID(k))
	}
	if sw := ch.Switch; sw != nil {
		e, ok := BuildRoster(a, next, nil).Entry(sw.ID)
		switch {
		case !ok:
			return false, fmt.Errorf("the %s agent has no entry %q (agentry agent show %s lists them)", a.Name, sw.ID, a.Name)
		case e.Fixed:
			return false, fmt.Errorf("%s is not switched: the %s agent always has it", e.Name, a.Name)
		case e.State == StateNeedsSignIn && sw.On:
			return false, fmt.Errorf("%s signs in by OAuth, which a persistent agent cannot do in this version", e.Name)
		}
		if next.Switches == nil {
			next.Switches = map[string]bool{}
		}
		next.Switches[sw.ID] = sw.On
	}
	// What this change turns on that the person has not agreed to: an entry
	// that was on and unagreed before, as it read then, is the start's to
	// ask about, not this change's.
	before := map[string]bool{}
	for _, e := range Unconsented(BuildRoster(a, *c, nil), *c) {
		before[e.ID+"\x00"+e.Line] = true
	}
	var widened []RosterEntry
	for _, e := range Unconsented(BuildRoster(a, next, nil), next) {
		if !before[e.ID+"\x00"+e.Line] {
			widened = append(widened, e)
		}
	}
	if len(widened) > 0 {
		if !ch.Consent {
			return false, &ConsentError{Agent: a.Name, Entries: widened}
		}
		if next.Consented == nil {
			next.Consented = map[string]string{}
		}
		for _, e := range widened {
			next.Consented[e.ID] = e.Line
		}
	}
	restart = grantsKey(a, *c) != grantsKey(a, next)
	*c = next
	return restart, nil
}

// grantsKey is what of a configuration a segment is given: two configs with
// the same key give a segment the same grants.
func grantsKey(a Agent, c Config) string {
	r := BuildRoster(a, c, nil)
	type key struct {
		On        []string
		Share     string
		Servers   []manifest.MCPServer
		Knowledge []string
	}
	k := key{}
	for _, e := range r {
		if e.On {
			k.On = append(k.On, e.ID+"="+e.Line)
		}
	}
	k.Share, _ = ShareFolder(a, c)
	for _, s := range c.Servers {
		if e, ok := r.Entry(ServerEntryID(s)); ok && e.On {
			k.Servers = append(k.Servers, s)
		}
	}
	b, _ := json.Marshal(k)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// cleanName is a name as the agent goes by it: one line of printable text.
func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if len(name) > MaxNameBytes || !utf8.ValidString(name) {
		return "", fmt.Errorf("a name is at most %d bytes of text", MaxNameBytes)
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return "", errors.New("a name is one line of printable text")
		}
	}
	return name, nil
}

// checkServer refuses a server the manifest's own rules refuse, and one a
// persistent agent cannot be given.
func checkServer(srv manifest.MCPServer) error {
	m := &manifest.Manifest{SchemaVersion: "2", Name: "check", Namespace: "check", Version: "1.0.0", Summary: "check",
		Modes:        []manifest.Mode{manifest.ModePersistent},
		Capabilities: manifest.Capabilities{MCPServers: []manifest.MCPServer{srv}}}
	if blocking := manifest.Blocking(manifest.Validate(m)); len(blocking) > 0 {
		// The server's own findings: the rest are the stand-in manifest's.
		var msgs []string
		for _, e := range blocking {
			if is, ok := manifest.AsIssue(e); ok && strings.HasPrefix(is.Path, "capabilities.mcpServers") {
				msgs = append(msgs, is.Message)
			}
		}
		if len(msgs) > 0 {
			return fmt.Errorf("the server is not one a QuickAgent may declare: %s", strings.Join(msgs, "; "))
		}
	}
	if mcpinstall.IDFor(srv) == "" {
		return errors.New("the server names no package and no address")
	}
	return nil
}

// personaKey is the digest of the name and personality the agent is given.
func personaKey(c Config) string {
	if c.DisplayName == "" && c.Personality == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(c.DisplayName + "\x00" + c.Personality))
	return hex.EncodeToString(sum[:])
}

// personaFrame is what a turn starts with when the agent has not yet been
// given its name and personality as they stand: each quoted, so nothing in
// them can pass for the frame around it.
func personaFrame(c Config) string {
	switch {
	case c.DisplayName == "" && c.Personality == "":
		return "(The person cleared the name and personality they had given you. Go back to being the Assistant, as your instructions describe.)\n\n"
	case c.Personality == "":
		return "(Your name, as it is set for you in Agentry: " + strconv.Quote(c.DisplayName) + ". Go by that name from now on.)\n\n"
	case c.DisplayName == "":
		return "(Your personality, as it is set for you in Agentry: " + strconv.Quote(c.Personality) + ". Take it on from now on, within your instructions.)\n\n"
	}
	return "(Your name and your personality, as they are set for you in Agentry: " + strconv.Quote(c.DisplayName) + ", and " +
		strconv.Quote(c.Personality) + ". Go by that name and take on that personality from now on, within your instructions.)\n\n"
}

func cloneMap[V any](m map[string]V) map[string]V {
	if m == nil {
		return nil
	}
	out := make(map[string]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Configure applies a change to an agent: through its supervisor when it
// runs, which then writes its config alone, and under its owner lock when
// it does not, so no start reads a config half changed. running says which.
func Configure(ctx context.Context, home string, a Agent, client string, ch Change) (r Roster, running bool, err error) {
	owner, err := Acquire(home, a.Name, client)
	var held *HeldError
	if errors.As(err, &held) {
		r, err := RequestChange(ctx, home, a.Name, ch)
		return r, true, err
	}
	if err != nil {
		return nil, false, err
	}
	defer owner.Release()
	cfg, err := LoadConfig(home, a.Name)
	if err != nil {
		return nil, false, err
	}
	if _, err := ApplyChange(home, a, &cfg, ch); err != nil {
		return nil, false, err
	}
	if err := SaveConfig(home, a.Name, cfg); err != nil {
		return nil, false, err
	}
	return BuildRoster(a, cfg, nil), false, nil
}

// AgreeToRoster records the person's agreement to everything the agent's
// roster has on, as a start asks for it: under the owner lock, so a running
// agent is refused.
func AgreeToRoster(home string, a Agent, client string) error {
	owner, err := Acquire(home, a.Name, client)
	if err != nil {
		return err
	}
	defer owner.Release()
	cfg, err := LoadConfig(home, a.Name)
	if err != nil {
		return err
	}
	RecordConsent(BuildRoster(a, cfg, nil), &cfg)
	return SaveConfig(home, a.Name, cfg)
}
