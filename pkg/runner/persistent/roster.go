package persistent

// The roster is what a persistent agent has, one entry per thing a switch
// turns on or off: its folders, each knowledge pack, each MCP server the
// person added. It is one value, built from the agent's built-in and its
// config.yaml, and every client renders that value: the consent text, the
// desktop's list, `agentry agent show` and the segment's receipt. Apply is
// the one function that turns it into what a segment is given, so a switch
// reaches every gate together at the segment it starts.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/pathguard"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// EntryKind is what a roster entry grants.
type EntryKind string

// The kinds of entry.
const (
	KindFiles     EntryKind = "files"
	KindKnowledge EntryKind = "knowledge"
	KindServer    EntryKind = "server"
)

// EntryState is a roster entry's state.
type EntryState string

// The states.
const (
	StateOn  EntryState = "on"
	StateOff EntryState = "off"
	// StatePending: switched since the running segment started; the next
	// turn starts the segment that applies it.
	StatePending EntryState = "pending"
	// StateNeedsSignIn: a server that signs in by OAuth, which an agent
	// cannot be given in this version.
	StateNeedsSignIn EntryState = "needs-sign-in"
	// StateInstalling: a server that is on and that the agent's store does
	// not hold yet. It is installed in the background while the agent runs
	// without it; once installed it is pending, and the next turn's segment
	// has it.
	StateInstalling EntryState = "installing"
	// StateFailed: a server whose install failed (Reason says why). The
	// agent runs without it until the person switches it off and on.
	StateFailed EntryState = "failed"
)

// Who granted an entry.
const (
	GrantedByBuiltin = "built-in"
	GrantedByYou     = "you"
)

// The ids of the folder entries.
const (
	EntryShare = "share"
	EntryData  = "data"
)

// RosterEntry is one line of the roster.
type RosterEntry struct {
	// ID is how a switch names the entry: "share", "data",
	// "knowledge:<pack>", "server:<id>".
	ID        string     `json:"id"`
	Kind      EntryKind  `json:"kind"`
	Name      string     `json:"name"`
	GrantedBy string     `json:"grantedBy"`
	State     EntryState `json:"state"`
	// On is the entry's switch as config.yaml holds it; State says what a
	// running segment has.
	On bool `json:"on"`
	// Line is the consent sentence for the entry.
	Line string `json:"line"`
	// Fixed is an entry no switch changes.
	Fixed bool `json:"fixed,omitempty"`
	// Reason is why a failed entry failed.
	Reason string `json:"reason,omitempty"`
	// ShareSwitch is, for a server that may see the agent's share folder
	// (a package server with no network), the id of its switch for that:
	// "server:<id>:share". Share says the switch is on: the server sees
	// the share, read only, while the agent has its share on. ShareLine is
	// the server's sentence with the switch on, the one turning it on asks
	// the person to agree to.
	ShareSwitch string `json:"shareSwitch,omitempty"`
	Share       bool   `json:"share,omitempty"`
	ShareLine   string `json:"shareLine,omitempty"`
	// alike are the other sentences an agreement to which is an agreement
	// to this entry: the share's sentence under the other malware-check
	// setting, which changes what the sentence says and grants nothing.
	alike []string
	// noConsent is an entry the agent comes with that grants nothing (a
	// built-in skill): on, it asks no agreement.
	noConsent bool
}

// Roster is an agent's entries, in a fixed order: its folders, its packs,
// its servers.
type Roster []RosterEntry

// RosterEntry is the roster's entry with id.
func (r Roster) Entry(id string) (RosterEntry, bool) {
	for _, e := range r {
		if e.ID == id {
			return e, true
		}
	}
	return RosterEntry{}, false
}

// Receipt is the roster as a segment's envelope records it.
func (r Roster) Receipt() []receipt.RosterEntry {
	out := make([]receipt.RosterEntry, 0, len(r))
	for _, e := range r {
		re := receipt.RosterEntry{ID: e.ID, Kind: string(e.Kind), Name: e.Name, GrantedBy: e.GrantedBy, On: e.On, Line: e.Line, Share: e.Share}
		if e.State == StateInstalling || e.State == StateFailed {
			re.State = string(e.State)
		}
		out = append(out, re)
	}
	return out
}

// ShareFolder is the agent's share folder: config.yaml's, else its default
// under ~/agentry.
func ShareFolder(a Agent, c Config) (string, error) {
	if c.Share != "" {
		return c.Share, nil
	}
	if a.ShareName == "" {
		return "", fmt.Errorf("the %s agent has no share folder", a.Name)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("the %s agent's share folder: %w", a.Name, err)
	}
	return filepath.Join(home, "agentry", a.ShareName), nil
}

// isDefaultShare says the folder is the agent's default, which a start makes
// when it is missing.
func isDefaultShare(a Agent, c Config) bool { return c.Share == "" }

// BuildRoster is the agent's roster from its config, its share's sentence
// saying the malware check is off, as the default policy has it; a client
// that knows the policy the agent runs under builds it with
// BuildRosterUnder. applied, when set, is what the running segment was
// given (Applied): an entry whose switch differs from it is pending.
func BuildRoster(a Agent, c Config, applied *Applied) Roster {
	return BuildRosterUnder(a, c, applied, false)
}

// The share's sentence, by what its check does.
const (
	shareLineChecked   = ", and a file you add is checked for malware with ClamAV before it reads it: one ClamAV finds is never read, one it cannot check is read unchecked"
	shareLineUnchecked = ", and it reads what you add there unchecked: the malware check is off on this computer (malwareCheck in the runner policy), so no file is checked with ClamAV"
	shareLineCoder     = ", and it reads and runs what you add there as it is, with no malware check"
)

// BuildRosterUnder is BuildRoster for an agent that runs under a policy
// whose malware check is malwareCheck: the share's sentence of an agent that
// checks its share says whether ClamAV checks its files. Changing the
// setting changes the sentence and asks nothing: an agreement to the share
// under either setting is an agreement to it under the other (Unconsented).
func BuildRosterUnder(a Agent, c Config, applied *Applied, malwareCheck bool) Roster {
	share, err := ShareFolder(a, c)
	if err != nil {
		share = "~/agentry/" + a.ShareName
	}
	var r Roster
	add := func(e RosterEntry, def bool) {
		e.On = def
		if v, ok := c.Switches[e.ID]; ok && !e.Fixed {
			e.On = v
		}
		e.State = StateOff
		if e.On {
			e.State = StateOn
		}
		// The heartbeat is read at each tick, not given to a segment: its
		// switch takes effect at once and is never pending.
		if applied != nil && !e.Fixed && e.ID != EntryHeartbeat && slices.Contains(applied.On, e.ID) != e.On {
			e.State = StatePending
		}
		r = append(r, e)
	}
	shareLine := "Shares the folder " + share + " with you, read and write: you see what it writes there as it writes it"
	var shareAlike []string
	switch {
	case a.Builtin.ChecksShare():
		checked, unchecked := shareLine+shareLineChecked, shareLine+shareLineUnchecked
		shareLine, shareAlike = unchecked, []string{checked}
		if malwareCheck {
			shareLine, shareAlike = checked, []string{unchecked}
		}
	case a.Builtin.Codes():
		shareLine += shareLineCoder
	}
	add(RosterEntry{ID: EntryShare, Kind: KindFiles, Name: "Your share folder", GrantedBy: GrantedByBuiltin, Line: shareLine, alike: shareAlike}, true)
	add(RosterEntry{ID: EntryData, Kind: KindFiles, Name: "Its own files", GrantedBy: GrantedByBuiltin, Fixed: true,
		Line: "Keeps its own notes and files on a volume of its own, kept until you reset it"}, true)
	if a.Builtin.KeepsMemory() {
		add(RosterEntry{ID: EntryMemory, Kind: KindMemory, Name: "Its memory", GrantedBy: GrantedByBuiltin, Fixed: true, Line: memoryLine}, true)
	}
	if a.Builtin.KeepsTime() {
		add(RosterEntry{ID: EntryHeartbeat, Kind: KindTasks, Name: "Its heartbeat and tasks", GrantedBy: GrantedByBuiltin, Line: heartbeatLine}, false)
	}
	if a.Builtin.Codes() {
		for _, e := range coderEntries(c) {
			add(e, true)
		}
	}
	for _, e := range skillEntries(a, c) {
		add(e, true)
	}
	for _, e := range secretEntries(a, c) {
		add(e, true)
	}
	if a.Builtin.Codes() {
		for _, e := range accountEntries(c) {
			add(e, true)
		}
	}
	for _, k := range c.Knowledge {
		line := "Searches your knowledge pack " + k + ", read-only"
		if a.Builtin.Codes() {
			line += "; the programs it runs can read the pack too, and with open egress on can send it anywhere"
		}
		add(RosterEntry{ID: KnowledgeEntryID(k), Kind: KindKnowledge, Name: k, GrantedBy: GrantedByYou, Line: line}, true)
	}
	for _, as := range agentServers(a, c) {
		if o := as.Offered; o != nil {
			es := offeredEntries(a, c, *o)
			add(es[0], o.On)
			if ok, reason := setUp(a, c, *o); !ok && r[len(r)-1].On {
				r[len(r)-1].State, r[len(r)-1].Reason = StateNeedsSetUp, reason
			} else if f, ok := c.ServerFailures[es[0].ID]; ok && r[len(r)-1].On {
				r[len(r)-1].State, r[len(r)-1].Reason = StateFailed, f.Reason
			}
			if len(es) > 1 {
				add(es[1], false)
			}
			continue
		}
		srv := as.Server
		e := RosterEntry{ID: ServerEntryID(srv), Kind: KindServer, Name: serverName(srv), GrantedBy: GrantedByYou, Line: serverLine(srv)}
		// One the agent suggested says, once added, what its suggestion
		// says of it.
		if sg, ok := suggestionFor(a, srv); ok {
			e.Name = sg.Name + " (" + serverName(srv) + ")"
			if sg.Note != "" {
				e.Line += "; " + sg.Note
			}
		}
		if ServerMaySeeShare(srv) == nil {
			// The server's sentence says whether it sees the share, so
			// letting it asks again; an agreement to the sentence that
			// says it does is an agreement to the one that says less.
			e.ShareSwitch = ServerShareID(srv)
			seesLine := e.Line + serverShareLine(a)
			e.ShareLine = seesLine
			if c.Switches[e.ShareSwitch] {
				e.Share, e.Line = true, seesLine
			} else {
				// So does a sentence that it sees the share without
				// saving into it.
				e.alike = []string{seesLine, e.Line + readOnlyShareLine(a)}
			}
		}
		if srv.Auth == manifest.AuthOAuth || srv.OAuth != nil {
			e.Line += "; it signs in by OAuth, which a persistent agent cannot do in this version"
			e.On, e.State = false, StateNeedsSignIn
			r = append(r, e)
			continue
		}
		add(e, true)
		if f, ok := c.ServerFailures[e.ID]; ok && r[len(r)-1].On {
			r[len(r)-1].State, r[len(r)-1].Reason = StateFailed, f.Reason
		}
	}
	return r
}

// serverReady says a segment may be given srv: its install has not failed,
// an offered server has what it needs, and the agent's store holds its pin
// with no install writing it. A server that is not ready is left out of the
// segment, which runs without it.
func serverReady(home string, a Agent, c Config, srv manifest.MCPServer) bool {
	if _, failed := c.ServerFailures[ServerEntryID(srv)]; failed {
		return false
	}
	if o, ok := offeredServer(a, c, ServerEntryID(srv)); ok {
		if set, _ := setUp(a, c, o); !set {
			return false
		}
	}
	name := a.Name
	if srv.IsRemote() {
		return true
	}
	dir := serversDir(home, name)
	return mcpinstall.Installed(dir, srv) && !mcpinstall.Installing(dir, mcpinstall.IDFor(srv))
}

// leftOut marks each server of r that is on and that a segment was not
// given (given says which it was): failed with its reason when its install
// failed, installing otherwise.
func leftOut(r Roster, c Config, given func(id string) bool) Roster {
	out := slices.Clone(r)
	for i, e := range out {
		if e.Kind != KindServer || !e.On || given(e.ID) {
			continue
		}
		out[i].State = StateInstalling
		if f, ok := c.ServerFailures[e.ID]; ok {
			out[i].State, out[i].Reason = StateFailed, f.Reason
		}
	}
	return out
}

// The prefixes of a pack's and a server's roster ids, and the suffix of a
// server's share switch.
const (
	knowledgePrefix = "knowledge:"
	serverPrefix    = "server:"
	shareSuffix     = ":share"
)

// ServerEntryID is a server's roster id: its store id, which names its
// package or its address.
func ServerEntryID(srv manifest.MCPServer) string { return serverPrefix + mcpinstall.IDFor(srv) }

// ServerShareID is the id of a server's switch that lets it see the
// agent's share folder: its roster id and ":share". It is a switch of the
// server's entry, not an entry of its own: it changes what the server's
// sentence says.
func ServerShareID(srv manifest.MCPServer) string { return ServerEntryID(srv) + shareSuffix }

// ServerMaySeeShare refuses a server the share is never given to: a remote
// server, which is another party's; a package server that reaches a host,
// which could send the person's files there; and one that signs in by
// OAuth, which a persistent agent cannot be given.
func ServerMaySeeShare(srv manifest.MCPServer) error {
	switch {
	case srv.IsRemote():
		return fmt.Errorf("%s is a remote server, another party's: it is never given your share folder", serverName(srv))
	case len(srv.Egress) > 0:
		return fmt.Errorf("%s reaches %s, so it could send your files there: a server that reaches the network is never given your share folder", serverName(srv), strings.Join(srv.Egress, ", "))
	case srv.Auth == manifest.AuthOAuth || srv.OAuth != nil:
		return fmt.Errorf("%s signs in by OAuth, which a persistent agent cannot do in this version", serverName(srv))
	}
	return nil
}

// serverShareLine is what a server's sentence adds when it sees the share,
// and saves new files into it through the share outbox: for the Coding
// agent, whose share is read as it is, that it sees it unchecked.
func serverShareLine(a Agent) string {
	if a.Builtin.Codes() {
		return ", and sees your share folder (read only, unchecked, and writes new files into it)"
	}
	return ", and sees your share folder (read only, and writes new files into it)"
}

// readOnlyShareLine is what a server's sentence adds for seeing the share
// without saving into it. An agreement recorded to that sentence covers the
// server's sentence with the switch off, which says less, and not the one
// with it on, which says more, so turning the switch on asks again.
func readOnlyShareLine(a Agent) string {
	if a.Builtin.Codes() {
		return ", and sees your share folder (read only, unchecked)"
	}
	return ", and sees your share folder (read only)"
}

// serversDir is where the servers the person added to an agent are
// installed.
func serversDir(home, name string) string {
	return mcpinstall.StoreDir(filepath.Join(Dir(home, name), "profile"))
}

// KnowledgeEntryID is a pack's roster id.
func KnowledgeEntryID(pack string) string { return knowledgePrefix + pack }

func serverName(srv manifest.MCPServer) string {
	switch {
	case srv.IsRemote():
		return srv.URL + " (remote)"
	case srv.NPM != "":
		return "npm:" + srv.NPM + "@" + srv.Version
	}
	return "pypi:" + srv.PyPI + "@" + srv.Version
}

// serverLine is a server's consent sentence: what it is, what it reaches,
// and the secrets it is given.
func serverLine(srv manifest.MCPServer) string {
	line := "Uses the MCP server " + serverName(srv)
	if len(srv.Args) > 0 {
		line += " started with the arguments " + strings.Join(srv.Args, " ")
	}
	if len(srv.Tools) > 0 {
		line += ", its tools " + strings.Join(srv.Tools, ", ")
	} else {
		line += ", all its tools"
	}
	switch {
	case srv.IsRemote():
		host := srv.RemoteHost()
		line += ", reaching " + host + " alone"
	case len(srv.Egress) > 0:
		line += ", reaching " + strings.Join(srv.Egress, ", ") + " alone"
	default:
		line += ", with no network"
	}
	if len(srv.Secrets) > 0 {
		line += ", given your secret " + strings.Join(srv.Secrets, ", ")
	}
	return line
}

// Applied is what a segment was given: the ids of the roster's entries that
// were on.
type Applied struct {
	On []string `json:"on"`
}

// Packs opens the knowledge packs a segment searches, by name; done closes
// what it opened once the segment ends.
type Packs func(ctx context.Context, names []string) (packs []engine.AttachedPack, done func(), err error)

// Apply turns the roster into what a segment is given: the share folder when
// it is on, the packs that are on and the servers that are on and ready (a
// server the agent's store does not hold yet, one being installed and one
// whose install failed are left out: serverReady), and the Coding agent's
// grant, its dev server readied by ready, the stored secrets that are on,
// their values read from keys, and its git host accounts that are on, their
// tokens read from keys. It is the one place a switch takes effect, so the
// tools server's spec, the gateway's allowlist, catalogue and secrets file,
// the bind lists and the driver's permit set, all compiled and rendered from
// what it returns, agree. applied names what the segment is given. done
// closes the packs it opened.
func Apply(ctx context.Context, home string, a Agent, c Config, r Roster, open Packs, ready HostReady, keys Secrets) (g localrun.Grants, applied Applied, done func(), err error) {
	done = func() {}
	on := func(id string) bool {
		e, ok := r.Entry(id)
		return ok && e.On
	}
	left := map[string]bool{}
	servers := agentServers(a, c)
	for _, as := range servers {
		if id := as.ID(); on(id) && !serverReady(home, a, c, as.Server) {
			left[id] = true
		}
	}
	for _, e := range r {
		// A server's writes are given with the server alone.
		if e.Kind == KindServerWrites {
			if parent := strings.TrimSuffix(e.ID, writesSuffix); !on(parent) || left[parent] {
				continue
			}
		}
		if e.On && !left[e.ID] {
			applied.On = append(applied.On, e.ID)
		}
	}
	if on(EntryShare) {
		dir, err := ShareFolder(a, c)
		if err != nil {
			return g, applied, done, err
		}
		if err := CheckShareFolder(home, dir); err != nil {
			return g, applied, done, err
		}
		if err := ensureShare(a, c, dir); err != nil {
			return g, applied, done, err
		}
		g.Share = dir
	}
	if a.Builtin.Codes() {
		if g.Coder, g.DevHost, err = coderGrant(ctx, c, on, ready); err != nil {
			return g, applied, done, err
		}
	}
	given, withheld := secretGrants(c, on)
	if len(given) > 0 && keys == nil {
		return g, applied, done, errors.New("the agent has a stored secret on, and this client cannot read the keychain")
	}
	if g.Secrets, err = ReadSecrets(keys, a.Name, given); err != nil {
		return g, applied, done, err
	}
	g.WithheldSecrets = withheld
	// The git host accounts that are on reach the Coding agent's programs,
	// so they are given with its commands alone; one given nothing is not
	// named as applied.
	if g.Coder != nil && g.Coder.Commands {
		if g.Accounts, err = readAccounts(keys, c, on); err != nil {
			return g, applied, done, err
		}
	} else {
		applied.On = slices.DeleteFunc(applied.On, func(id string) bool { return strings.HasPrefix(id, accountPrefix) })
	}
	var givenServers []agentServer
	for _, as := range servers {
		if id := as.ID(); on(id) && !left[id] {
			srv := givenServer(c, as)
			givenServers = append(givenServers, as)
			g.Servers = append(g.Servers, srv)
			// A server sees the share while the agent has it, read only,
			// and only one the share may be given to.
			if e, _ := r.Entry(id); e.Share && g.Share != "" && ServerMaySeeShare(srv) == nil {
				g.ShareServers = append(g.ShareServers, srv.SourceID())
			}
		}
	}
	if len(g.Servers) > 0 {
		g.ServersDir = serversDir(home, a.Name)
	}
	if g.ServerSecrets, err = serverTokens(keys, c, givenServers); err != nil {
		return g, applied, done, err
	}
	if g.Skills, g.SkillsDir, err = stageSkills(home, a, c, on); err != nil {
		return g, applied, done, err
	}
	var names []string
	for _, k := range c.Knowledge {
		if on(KnowledgeEntryID(k)) {
			names = append(names, k)
		}
	}
	if len(names) > 0 {
		if open == nil {
			return g, applied, done, errors.New("the agent searches knowledge packs, and this client cannot open them")
		}
		packs, closePacks, err := open(ctx, names)
		if err != nil {
			return g, applied, done, fmt.Errorf("open the agent's knowledge packs: %w", err)
		}
		g.Knowledge, done = packs, closePacks
	}
	return g, applied, done, nil
}

// ensureShare makes the agent's default share folder at its first start; a
// folder the person chose must already exist.
func ensureShare(a Agent, c Config, dir string) error {
	if _, err := os.Lstat(dir); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the share folder %s: %w", dir, err)
	}
	if !isDefaultShare(a, c) {
		return fmt.Errorf("the share folder %s does not exist: make it, or choose another (agentry agent configure %s --share DIR)", dir, a.Name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the person's own folder, theirs to read
		return fmt.Errorf("make the share folder %s: %w", dir, err)
	}
	return nil
}

// CheckShareFolder refuses a share folder that would hand the agent more
// than the person meant: a path that is not absolute and clean, one reached
// through a link, a filesystem root, the home folder or a folder holding it,
// a hidden folder of the home folder (where keys and settings live) or one
// beneath it, the runner's own folders or the configuration's, a folder
// that holds any of them, and the system's folders. A path holding a colon
// beyond a drive's is refused too: the gateway's bind lists are joined with
// one. A folder that does not exist yet passes, beneath a parent that does.
func CheckShareFolder(home, dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return fmt.Errorf("the share folder %q is not a clean absolute path", dir)
	}
	if strings.Contains(strings.TrimPrefix(dir, filepath.VolumeName(dir)), ":") {
		return fmt.Errorf("the share folder %q has a colon in its path; choose one without", dir)
	}
	userHome, _ := os.UserHomeDir()
	if userHome != "" {
		if err := pathguard.RefuseDangerousDir(dir, userHome); err != nil {
			return fmt.Errorf("the share folder %s is %v; choose a folder of its own", dir, err)
		}
		if within(userHome, dir) {
			rel, _ := filepath.Rel(userHome, dir)
			if first := strings.SplitN(rel, string(filepath.Separator), 2)[0]; strings.HasPrefix(first, ".") {
				return fmt.Errorf("the share folder %s is in a hidden folder of your home folder (%s), where keys and settings are kept; choose another", dir, first)
			}
		}
	}
	resolved, err := resolveExisting(dir)
	if err != nil {
		return fmt.Errorf("the share folder %s: %w", dir, err)
	}
	if resolved != dir {
		return fmt.Errorf("the share folder %s is reached through a link (it is %s); name the folder itself", dir, resolved)
	}
	guarded := []struct{ what, path string }{{"the runner's folder", home}}
	if cfg, err := os.UserConfigDir(); err == nil {
		guarded = append(guarded, struct{ what, path string }{"Agentry's configuration", filepath.Join(cfg, "agentry")})
	}
	for _, g := range guarded {
		p := g.path
		if r, err := resolveExisting(p); err == nil {
			p = r
		}
		switch {
		case within(p, dir):
			return fmt.Errorf("the share folder %s is inside %s (%s)", dir, g.what, p)
		case within(dir, p):
			return fmt.Errorf("the share folder %s holds %s (%s); choose a folder of its own", dir, g.what, p)
		}
	}
	for _, sys := range systemFolders() {
		if dir == sys || within(sys, dir) && !userFolder(dir) {
			return fmt.Errorf("the share folder %s is a system folder; choose a folder of your own", dir)
		}
	}
	return nil
}

// within says p is root or beneath it; without regard to case where the
// filesystem usually has none.
func within(root, p string) bool {
	if root == "" {
		return false
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		root, p = strings.ToLower(root), strings.ToLower(p)
	}
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// resolveExisting resolves the links of the deepest part of p that exists,
// and joins the rest to it.
func resolveExisting(p string) (string, error) {
	rest := ""
	for cur := p; ; cur = filepath.Dir(cur) {
		r, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(r, rest), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		rest = filepath.Join(filepath.Base(cur), rest)
	}
}

// userFolder says a folder beneath a system folder is still a person's: the
// home folders and the mounted volumes are beneath system roots on some
// systems.
func userFolder(dir string) bool {
	if home, err := os.UserHomeDir(); err == nil && within(home, dir) {
		return true
	}
	for _, root := range []string{"/media", "/mnt", "/run/media", "/Volumes"} {
		if runtime.GOOS != "windows" && within(root, dir) && dir != root {
			return true
		}
	}
	return false
}

// systemFolders are the folders a share may be neither nor be beneath.
func systemFolders() []string {
	if runtime.GOOS == "windows" {
		var out []string
		for _, env := range []string{"SystemRoot", "ProgramFiles", "ProgramFiles(x86)", "ProgramData"} {
			if v := os.Getenv(env); v != "" {
				out = append(out, filepath.Clean(v))
			}
		}
		return out
	}
	out := []string{"/", "/bin", "/boot", "/dev", "/etc", "/lib", "/lib64", "/opt", "/proc", "/root", "/run", "/sbin", "/srv", "/sys", "/usr", "/var", "/tmp",
		"/home", "/Users", "/media", "/mnt", "/Volumes"}
	if runtime.GOOS == "darwin" {
		out = append(out, "/System", "/Library", "/Applications", "/private")
	}
	sort.Strings(out)
	return out
}
