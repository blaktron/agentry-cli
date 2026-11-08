package persistent

// The person's computer, for a persistent agent. An agent that declares the
// persistent capability desktop is offered, as roster entries, each
// application server of the catalogue (narrowed by its release's
// capabilities.desktop line, when it has one) and computer use. Each is off
// until the person switches it on and agrees to it; a switch is a pending
// change the agent's next turn applies, its segment compiled with the
// desktop link's entries in or out. An application server whose tools run
// code has a second entry, the agreement that its code tools run without
// asking each time, given only with its server. Whether the application is
// running is read at each call, by Agentry Desktop: opening or closing it
// restarts nothing.

import (
	"io/fs"
	"net"
	"os"
	"slices"
	"time"

	"github.com/blaktron/agentry/pkg/appserver"
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

// The ids of the desktop entries: "desktop:<catalogue id>", its exec
// agreement "desktop:<catalogue id>:exec", and "desktop:computer".
const (
	desktopPrefix = "desktop:"
	execSuffix    = ":exec"
	// EntryComputer is computer use's entry.
	EntryComputer = desktopPrefix + "computer"
)

// DesktopEntryID is an application server's roster id.
func DesktopEntryID(id string) string { return desktopPrefix + id }

// DesktopExecID is the roster id of an application server's exec
// agreement.
func DesktopExecID(id string) string { return DesktopEntryID(id) + execSuffix }

// UsesDesktop says the agent declares the persistent capability desktop.
func (a Agent) UsesDesktop() bool { return a.Declared().Declares("desktop") }

// desktopOffered is what the agent is offered on the person's computer: the
// catalogue's application servers, each with the tools it may use, and
// computer use's scope ("" for none). A release's capabilities.desktop line
// narrows both; without one every catalogue entry is offered, and computer
// use in app scope.
func desktopOffered(a Agent) (servers []manifest.DesktopServer, computerUse manifest.ComputerUse) {
	line := a.DesktopLine
	if line == nil {
		for _, e := range appserver.Builtin().All() {
			servers = append(servers, manifest.DesktopServer{ID: e.ID})
		}
		return servers, manifest.ComputerUseApp
	}
	return slices.Clone(line.Servers), line.ComputerUse
}

// desktopEntries are the agent's desktop entries, each off until switched
// on: per application server, its entry and, when it has a tool that runs
// code, its exec agreement; then computer use.
func desktopEntries(a Agent) []RosterEntry {
	if !a.UsesDesktop() {
		return nil
	}
	servers, computerUse := desktopOffered(a)
	catalogue := appserver.Builtin()
	var out []RosterEntry
	var apps []string
	for _, srv := range servers {
		e, ok := catalogue.Lookup(srv.ID)
		if !ok {
			continue
		}
		apps = append(apps, e.Application)
		out = append(out, RosterEntry{ID: DesktopEntryID(e.ID), Kind: KindDesktop, Name: e.Application + " on your computer", GrantedBy: a.grantedBy(),
			Line: "Can use " + manifest.DescribeDesktopServer(srv) + ", through Agentry Desktop; whether " + e.Application + " is running is read at each use"})
		if execTools(srv, e) > 0 {
			out = append(out, RosterEntry{ID: DesktopExecID(e.ID), Kind: KindDesktopExec, Name: e.Application + ": code without asking", GrantedBy: a.grantedBy(),
				Line: "Runs code inside " + e.Application + " without Agentry Desktop asking you each time; off, each such call asks you first"})
		}
	}
	switch computerUse {
	case manifest.ComputerUseApp:
		what := "the windows of the applications above"
		if len(apps) == 0 {
			what = "the window of an application you pick"
		}
		out = append(out, RosterEntry{ID: EntryComputer, Kind: KindDesktop, Name: "Computer use", GrantedBy: a.grantedBy(),
			Line: "Can see and use " + what + " on your computer, through Agentry Desktop, which shows when it does and stops it on your key"})
	case manifest.ComputerUseScreen:
		out = append(out, RosterEntry{ID: EntryComputer, Kind: KindDesktop, Name: "Computer use", GrantedBy: a.grantedBy(),
			Line: "Can see and use your whole screen, through Agentry Desktop, which shows when it does and stops it on your key"})
	}
	return out
}

// execTools counts the tools of srv, as the catalogue entry e gives them,
// that run code.
func execTools(srv manifest.DesktopServer, e appserver.Server) int {
	n := 0
	for _, t := range srv.Allowed(e) {
		if t.Class == appserver.ClassExec {
			n++
		}
	}
	return n
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
func desktopGrant(a Agent, on func(string) bool) *DesktopGrant {
	if !a.UsesDesktop() {
		return nil
	}
	servers, computerUse := desktopOffered(a)
	line := &manifest.Desktop{}
	var exec []string
	for _, srv := range servers {
		if !on(DesktopEntryID(srv.ID)) {
			continue
		}
		line.Servers = append(line.Servers, srv)
		if on(DesktopExecID(srv.ID)) {
			exec = append(exec, srv.ID)
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
