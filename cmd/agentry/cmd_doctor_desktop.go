package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
)

// doctorDialTimeout bounds each of the desktop rows' connections.
var doctorDialTimeout = time.Second

// doctorList lists the person's approved applications from Agentry
// Desktop; a test replaces it.
var doctorList = func(ctx context.Context, sock string) (desktop.ApplicationList, error) {
	return desktop.Client{Socket: sock}.List(ctx)
}

// doctorDesktop reports the person's computer: whether Agentry Desktop
// answers on its link socket here, and each application the person set up
// in it, as it lists them, with what they approved and whether it is ready.
// A row is a failure only for what the QuickAgent checked uses: an
// application it names that is not set up, not approved for what it uses,
// or not ready; any other application, and Agentry Desktop for a
// QuickAgent that uses none, are said and need nothing.
func doctorDesktop(rep *doctorReport, home string, m *manifest.Manifest) {
	var line *manifest.Desktop
	if m != nil {
		line = m.Capabilities.Desktop
	}
	sock := desktop.SocketPath(home)
	c, err := net.DialTimeout("unix", sock, doctorDialTimeout)
	switch {
	case err == nil:
		_ = c.Close()
		rep.ok("desktop link", "Agentry Desktop answers on %s", sock)
	case line != nil:
		rep.failFix("desktop link", "start Agentry Desktop on this computer",
			"Agentry Desktop is not running: this QuickAgent uses your computer through it")
		return
	default:
		rep.ok("desktop link", "Agentry Desktop is not running here; only a QuickAgent that uses an application on your computer needs it")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	list, err := doctorList(ctx, sock)
	switch {
	case errors.Is(err, desktop.ErrNoListing) && line != nil:
		rep.failFix("desktop: applications", "update Agentry Desktop", "%v", err)
		return
	case err != nil && line != nil:
		rep.failFix("desktop: applications", "open Agentry Desktop's Settings, Your computer", "Agentry Desktop did not list your applications: %v", err)
		return
	case err != nil:
		rep.ok("desktop: applications", "Agentry Desktop did not list your applications (%v); only a QuickAgent that uses one needs them", err)
		return
	case len(list.Applications) == 0 && line == nil:
		rep.ok("desktop: applications", "none set up in Agentry Desktop's Settings, Your computer")
	}
	byID := map[string]desktop.Application{}
	for _, a := range list.Applications {
		byID[a.ID] = a
		rep.ok("desktop: "+a.Name, "%s", applicationState(a))
	}
	if line == nil {
		return
	}
	for _, s := range line.Servers {
		if s.Choose {
			continue
		}
		a, ok := byID[s.ID]
		name := "desktop: " + s.ID
		switch {
		case !ok:
			rep.failFix(name, "add it in Agentry Desktop's Settings, Your computer", "this QuickAgent uses an application called %s, which is not set up on this computer", s.ID)
		case s.UsesServer() && !a.MCP:
			rep.failFix(name, "approve its MCP server in Agentry Desktop's Settings, Your computer", "this QuickAgent uses %s's MCP server, which you have not approved", a.Name)
		case line.UsesWindows(s) && !a.Computer:
			rep.failFix(name, "approve its windows for computer use in Agentry Desktop's Settings, Your computer", "this QuickAgent works %s's windows, which you have not approved", a.Name)
		case !a.Ready:
			rep.failFix(name, stepOr(a), "this QuickAgent uses %s, which is not ready", a.Name)
		}
	}
}

// applicationState is a listed application in the row's words.
func applicationState(a desktop.Application) string {
	var approved []string
	if a.MCP {
		approved = append(approved, "its MCP server")
	}
	if a.Computer {
		approved = append(approved, "its windows")
	}
	what := "nothing approved"
	if len(approved) > 0 {
		what = strings.Join(approved, " and ") + " approved"
	}
	from := "set up by you"
	if !desktop.SetByPerson(a.From) {
		from = "from Agentry Desktop's library"
	}
	ready := "ready"
	if !a.Ready {
		ready = "not ready: " + stepOr(a)
	}
	return fmt.Sprintf("%s (%s), %s, %s", a.ID, from, what, ready)
}

// stepOr is a listed application's step, or a sentence that says what to
// open.
func stepOr(a desktop.Application) string {
	if a.Step != "" {
		return a.Step
	}
	return "open " + a.Name + " and Agentry Desktop's Settings, Your computer"
}
