package engine

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// TestTheAppLinksSocketIsHeldToThePlan: a segment is given the app link's
// socket exactly when its plan has an app link, and it must be a socket;
// the run's replacer names it and the person's user for the container.
func TestTheAppLinksSocketIsHeldToThePlan(t *testing.T) {
	dir, err := os.MkdirTemp("", "al")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "app.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	with := &spec.Plan{Capabilities: spec.CapSummary{AppLink: "assistant"}}
	for name, tc := range map[string]struct {
		plan *spec.Plan
		sock string
		ok   bool
	}{
		"a socket for a plan with an app link": {with, sock, true},
		"none for a plan without one":          {&spec.Plan{}, "", true},
		"a socket for a plan without one":      {&spec.Plan{}, sock, false},
		"none for a plan with one":             {with, "", false},
		"a file for a socket":                  {with, file, false},
		"a relative path":                      {with, "app.sock", false},
	} {
		rd := runDirsAt(t.TempDir())
		err := prepareAppLink(Params{Plan: tc.plan, Persistent: &PersistentSegment{AppLink: tc.sock}}, rd)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", name, err)
		}
		if tc.ok && rd.appLink != tc.sock {
			t.Errorf("%s: the run's app link is %q", name, rd.appLink)
		}
	}
	rd := runDirsAt(t.TempDir())
	rd.appLink = sock
	if got := rd.replacer().Replace(spec.PHAppLink); got != sock {
		t.Errorf("the replacer names the socket %q", got)
	}
}
