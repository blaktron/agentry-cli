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
		out = append(out, receipt.RosterEntry{ID: e.ID, Kind: string(e.Kind), Name: e.Name, GrantedBy: e.GrantedBy, On: e.On, Line: e.Line})
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

// BuildRoster is the agent's roster from its config. applied, when set, is
// what the running segment was given (Applied): an entry whose switch
// differs from it is pending.
func BuildRoster(a Agent, c Config, applied *Applied) Roster {
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
		if applied != nil && !e.Fixed && slices.Contains(applied.On, e.ID) != e.On {
			e.State = StatePending
		}
		r = append(r, e)
	}
	shareLine := "Shares the folder " + share + " with you, read and write: you see what it writes there as it writes it"
	if a.Builtin.ChecksShare() {
		shareLine += ", and a file you add is checked for malware with ClamAV before it reads it: one ClamAV finds is never read, one it cannot check is read unchecked"
	}
	add(RosterEntry{ID: EntryShare, Kind: KindFiles, Name: "Your share folder", GrantedBy: GrantedByBuiltin, Line: shareLine}, true)
	add(RosterEntry{ID: EntryData, Kind: KindFiles, Name: "Its own files", GrantedBy: GrantedByBuiltin, Fixed: true,
		Line: "Keeps its own notes and files on the volume " + DataVolume(a.Name) + ", kept until you reset it"}, true)
	for _, k := range c.Knowledge {
		add(RosterEntry{ID: KnowledgeEntryID(k), Kind: KindKnowledge, Name: k, GrantedBy: GrantedByYou,
			Line: "Searches your knowledge pack " + k + ", read-only"}, true)
	}
	for _, srv := range c.Servers {
		e := RosterEntry{ID: ServerEntryID(srv), Kind: KindServer, Name: serverName(srv), GrantedBy: GrantedByYou, Line: serverLine(srv)}
		if srv.Auth == manifest.AuthOAuth || srv.OAuth != nil {
			e.Line += "; it signs in by OAuth, which a persistent agent cannot do in this version"
			e.On, e.State = false, StateNeedsSignIn
			r = append(r, e)
			continue
		}
		add(e, true)
	}
	return r
}

// The prefixes of a pack's and a server's roster ids.
const (
	knowledgePrefix = "knowledge:"
	serverPrefix    = "server:"
)

// ServerEntryID is a server's roster id: its store id, which names its
// package or its address.
func ServerEntryID(srv manifest.MCPServer) string { return serverPrefix + mcpinstall.IDFor(srv) }

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
// it is on, the packs and the servers that are on. It is the one place a
// switch takes effect, so the tools server's spec, the gateway's allowlist and
// catalogue, the bind lists and the driver's permit set, all compiled and
// rendered from what it returns, agree. done closes the packs it opened.
func Apply(ctx context.Context, home string, a Agent, c Config, r Roster, open Packs) (g localrun.Grants, applied Applied, done func(), err error) {
	done = func() {}
	on := func(id string) bool {
		e, ok := r.Entry(id)
		return ok && e.On
	}
	for _, e := range r {
		if e.On {
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
	for _, srv := range c.Servers {
		if on(ServerEntryID(srv)) {
			g.Servers = append(g.Servers, srv)
		}
	}
	if len(g.Servers) > 0 {
		g.ServersDir = mcpinstall.StoreDir(filepath.Join(Dir(home, a.Name), "profile"))
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
