package persistent

// The person's computer, for a persistent agent. An agent that declares the
// persistent capability desktop is offered, as roster entries, each
// application whose MCP server its release's capabilities.desktop line
// names, or, without a line, each application the person approved for its
// MCP server in Agentry Desktop, and computer use. Each is off until the
// person switches it on and agrees to it; a switch is a pending change the
// agent's next turn applies, its segment compiled with the desktop link's
// entries in or out. Each application has a second entry, the agreement
// that its code tools run without asking each time, given only with it:
// which of its tools run code is Agentry Desktop's to know. Whether the
// application is running is read at each call, by Agentry Desktop: opening
// or closing it restarts nothing.

import (
	"context"
	"io/fs"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// KindDesktop is the kind of an application's entry and of computer use's;
// KindDesktopExec the kind of an application's exec agreement.
const (
	KindDesktop     EntryKind = "desktop"
	KindDesktopExec EntryKind = "desktop-exec"
)

// The ids of the desktop entries: "desktop:<application id>", its exec
// agreement "desktop:<application id>:exec", and "desktop:computer".
const (
	desktopPrefix = "desktop:"
	execSuffix    = ":exec"
	// EntryComputer is computer use's entry.
	EntryComputer = desktopPrefix + "computer"
)

// DesktopEntryID is an application's roster id.
func DesktopEntryID(id string) string { return desktopPrefix + id }

// DesktopExecID is the roster id of an application's exec agreement.
func DesktopExecID(id string) string { return DesktopEntryID(id) + execSuffix }

// UsesDesktop says the agent declares the persistent capability desktop.
func (a Agent) UsesDesktop() bool { return a.Declared().Declares("desktop") }

// listApplications lists the person's approved applications from the
// Agentry Desktop serving the runner home's link socket; a test replaces
// it.
var listApplications = func() ([]desktop.Application, error) {
	home, err := policy.Home()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	list, err := desktop.Client{Socket: desktop.SocketPath(home)}.List(ctx)
	return list.Applications, err
}

// applicationID is an application id's grammar, as a release names one.
var applicationID = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// switchableID says a switch's id may stand for an application: an id of
// the grammar, at most 40 characters, and neither of the desktop link's own
// entry names.
func switchableID(id string) bool {
	return len(id) <= manifest.MaxDesktopIDLength && applicationID.MatchString(id) && !slices.Contains(manifest.ReservedDesktopIDs, id)
}

// offeredApplication is one application the agent is offered: the line's
// entry and the name the roster shows.
type offeredApplication struct {
	srv  manifest.DesktopServer
	name string
}

// desktopOffered is what the agent is offered on the person's computer: the
// applications whose MCP servers it may use, each with the tools it may use,
// and computer use's scope ("" for none). A release's capabilities.desktop
// line names both. Without one the agent is offered each application the
// person approved for its MCP server, as Agentry Desktop lists them, and
// each one switched on in switches, so an application switched on stays on
// while Agentry Desktop is not answering; and computer use in app scope.
func desktopOffered(a Agent, switches map[string]bool) (apps []offeredApplication, computerUse manifest.ComputerUse) {
	line := a.DesktopLine
	if line == nil {
		listed, _ := listApplications()
		seen := map[string]bool{}
		for _, l := range listed {
			if l.MCP && !seen[l.ID] {
				seen[l.ID] = true
				apps = append(apps, offeredApplication{srv: manifest.DesktopServer{ID: l.ID}, name: l.Name})
			}
		}
		var on []string
		for k, v := range switches {
			id, ok := strings.CutPrefix(k, desktopPrefix)
			if ok && v && k != EntryComputer && !strings.HasSuffix(id, execSuffix) && !seen[id] && switchableID(id) {
				on = append(on, id)
			}
		}
		slices.Sort(on)
		for _, id := range on {
			apps = append(apps, offeredApplication{srv: manifest.DesktopServer{ID: id}, name: id})
		}
		return apps, manifest.ComputerUseApp
	}
	for _, srv := range line.Servers {
		switch {
		case !srv.UsesServer():
		case srv.Choose:
			apps = append(apps, offeredApplication{srv: srv, name: "An application you choose"})
		default:
			apps = append(apps, offeredApplication{srv: srv, name: srv.ID})
		}
	}
	return apps, line.ComputerUse
}

// offeredID is an offered application's roster id part: its id, or the
// chosen entry's.
func offeredID(srv manifest.DesktopServer) string {
	if srv.Choose {
		return manifest.DesktopChosenName
	}
	return srv.ID
}

// desktopEntries are the agent's desktop entries, each off until switched
// on: per application, its entry and its exec agreement; then computer use.
func desktopEntries(a Agent, switches map[string]bool) []RosterEntry {
	if !a.UsesDesktop() {
		return nil
	}
	apps, computerUse := desktopOffered(a, switches)
	var out []RosterEntry
	for _, app := range apps {
		id := offeredID(app.srv)
		out = append(out, RosterEntry{ID: DesktopEntryID(id), Kind: KindDesktop, Name: app.name + " on your computer", GrantedBy: a.grantedBy(),
			Line: "Can use " + manifest.DescribeDesktopServer(app.srv) + ", through Agentry Desktop; whether the application is running is read at each use"})
		out = append(out, RosterEntry{ID: DesktopExecID(id), Kind: KindDesktopExec, Name: app.name + ": code without asking", GrantedBy: a.grantedBy(),
			Line: "Runs the tools of " + app.name + " that run code without Agentry Desktop asking you each time; off, each such call asks you first"})
	}
	switch computerUse {
	case manifest.ComputerUseApp:
		what := "the windows of the applications above"
		if len(apps) == 0 {
			what = "the window of an application you choose"
		}
		out = append(out, RosterEntry{ID: EntryComputer, Kind: KindDesktop, Name: "Computer use", GrantedBy: a.grantedBy(),
			Line: "Can see and use " + what + " on your computer, through Agentry Desktop, which shows when it does and stops it on your key"})
	case manifest.ComputerUseScreen:
		out = append(out, RosterEntry{ID: EntryComputer, Kind: KindDesktop, Name: "Computer use", GrantedBy: a.grantedBy(),
			Line: "Can see and use your whole screen, through Agentry Desktop, which shows when it does and stops it on your key"})
	}
	return out
}

// DesktopGrant is what of the person's computer a segment is given: the
// desktop line its plan compiles with, and the application servers whose
// code tools the person agreed to without asking.
type DesktopGrant struct {
	Line *manifest.Desktop
	Exec []string
}

// desktopGrant is the desktop line the roster's entries that are on make,
// each server with the tools the agent is offered; nil when none is on.
func desktopGrant(a Agent, switches map[string]bool, on func(string) bool) *DesktopGrant {
	if !a.UsesDesktop() {
		return nil
	}
	apps, computerUse := desktopOffered(a, switches)
	line := &manifest.Desktop{}
	var exec []string
	for _, app := range apps {
		id := offeredID(app.srv)
		if !on(DesktopEntryID(id)) {
			continue
		}
		line.Servers = append(line.Servers, app.srv)
		if on(DesktopExecID(id)) {
			exec = append(exec, id)
		}
	}
	if computerUse != "" && on(EntryComputer) {
		line.ComputerUse = computerUse
	}
	if len(line.Servers) == 0 && line.ComputerUse == "" {
		return nil
	}
	return &DesktopGrant{Line: line, Exec: exec}
}

// desktopAnswering says an Agentry Desktop serves its link socket in the
// runner home: the desktop entries that are on read needs-set-up while none
// does. On a runhost the runhost's service serves it for the device that
// linked its agents.
func desktopAnswering() bool {
	home, err := policy.Home()
	if err != nil {
		return false
	}
	sock := desktop.SocketPath(home)
	if fi, err := os.Lstat(sock); err != nil || fi.Mode()&fs.ModeSocket == 0 {
		return false
	}
	// A socket left by an app that has quit answers no one.
	c, err := net.DialTimeout("unix", sock, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// desktopNotAnswering is the reason a desktop entry that is on needs
// set-up.
const desktopNotAnswering = "Agentry Desktop is not running on this computer: start it, and the agent's next turn reaches it"
