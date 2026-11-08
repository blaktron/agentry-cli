package main

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"time"

	"github.com/blaktron/agentry/pkg/appserver"
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
)

// doctorDialTimeout bounds each of the desktop rows' connections.
var doctorDialTimeout = time.Second

// doctorDesktop reports the person's computer: whether Agentry Desktop
// answers on its link socket here, and for each application of the
// catalogue whether the application's MCP listener answers on its loopback
// port. A row is a failure only for what the QuickAgent checked declares:
// any other application, and Agentry Desktop for a QuickAgent that uses
// none, are said and need nothing.
func doctorDesktop(rep *doctorReport, home string, m *manifest.Manifest) {
	var uses []string
	if m != nil && m.Capabilities.Desktop != nil {
		for _, s := range m.Capabilities.Desktop.Servers {
			uses = append(uses, s.ID)
		}
	}
	sock := desktop.SocketPath(home)
	if c, err := net.DialTimeout("unix", sock, doctorDialTimeout); err == nil {
		_ = c.Close()
		rep.ok("desktop link", "Agentry Desktop answers on %s", sock)
	} else if m != nil && m.Capabilities.Desktop != nil {
		rep.failFix("desktop link", "start Agentry Desktop on this computer",
			"Agentry Desktop is not running: this QuickAgent uses your computer through it")
	} else {
		rep.ok("desktop link", "Agentry Desktop is not running here; only a QuickAgent that uses an application on your computer needs it")
	}
	for _, e := range appserver.Builtin().All() {
		name := "desktop: " + e.Application
		port := e.Listener.Port
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		c, err := net.DialTimeout("tcp", addr, doctorDialTimeout)
		switch {
		case err == nil:
			_ = c.Close()
			rep.ok(name, "%s's MCP listener answers on port %d", e.Application, port)
		case slices.Contains(uses, e.ID):
			rep.failFix(name, firstStep(e), "%s does not answer on port %d: this QuickAgent uses it", e.Application, port)
		default:
			rep.ok(name, "%s does not answer on port %d (not running, or its MCP add-on is off)", e.Application, port)
		}
	}
}

// firstStep is the catalogue entry's first install step, or a sentence
// that says what to open.
func firstStep(e appserver.Server) string {
	if len(e.Steps) > 0 {
		return e.Steps[0]
	}
	return fmt.Sprintf("open %s and start its MCP server", e.Application)
}
