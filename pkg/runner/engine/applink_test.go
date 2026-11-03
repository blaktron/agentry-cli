package engine

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	// The relative path names a socket that exists from here, so only its
	// being relative refuses it.
	t.Chdir(dir)
	with := &spec.Plan{Capabilities: spec.CapSummary{AppLink: "assistant"}}
	type linkCase struct {
		plan *spec.Plan
		sock string
		ok   bool
		// msg is what the refusal says.
		msg string
	}
	const (
		disagree = "the segment's app link socket and its plan disagree"
		notClean = "is not a clean absolute path"
		notSock  = "is not a socket"
	)
	cases := map[string]linkCase{
		"a socket for a plan with an app link": {with, sock, true, ""},
		"none for a plan without one":          {&spec.Plan{}, "", true, ""},
		"a socket for a plan without one":      {&spec.Plan{}, sock, false, disagree},
		"none for a plan with one":             {with, "", false, disagree},
		"a file for a socket":                  {with, file, false, notSock},
		"a relative path":                      {with, "app.sock", false, notClean},
		"an unclean path":                      {with, dir + string(filepath.Separator) + "." + string(filepath.Separator) + "app.sock", false, notClean},
	}
	// A link to the socket is not the socket.
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(sock, link); err == nil {
		cases["a link to a socket"] = linkCase{with, link, false, notSock}
	}
	// A colon in the path would split the mount it is named in.
	if runtime.GOOS != "windows" {
		colon := filepath.Join(dir, "a:b.sock")
		cln, err := net.Listen("unix", colon)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cln.Close() }()
		cases["a path with a colon"] = linkCase{with, colon, false, notClean}
	}
	for name, tc := range cases {
		rd := runDirsAt(t.TempDir())
		err := prepareAppLink(Params{Plan: tc.plan, Persistent: &PersistentSegment{AppLink: tc.sock}}, rd)
		if (err == nil) != tc.ok || (err != nil && !strings.Contains(err.Error(), tc.msg)) {
			t.Errorf("%s: err = %v, want one saying %q", name, err, tc.msg)
		}
		if tc.ok && rd.appLink != tc.sock {
			t.Errorf("%s: the run's app link is %q", name, rd.appLink)
		}
	}
	// Where the daemon runs in a VM, the run's relay volume carries the
	// calls; where it runs here, the socket is bound itself.
	for inProcess, want := range map[bool][2]string{true: {"", ""}, false: {"agentry-R-applink", "why"}} {
		rd := runDirsAt(filepath.Join(t.TempDir(), "R"))
		place := &filterPlacement{inProcess: inProcess, reason: "why"}
		if err := prepareAppLink(Params{Plan: with, Persistent: &PersistentSegment{AppLink: sock}, place: place}, rd); err != nil {
			t.Fatal(err)
		}
		if rd.appLinkRelay != want[0] || rd.appLinkPlacement != want[1] || rd.appLink != sock {
			t.Errorf("in process %v: relay %q, placement %q, socket %q", inProcess, rd.appLinkRelay, rd.appLinkPlacement, rd.appLink)
		}
	}
	rd := runDirsAt(t.TempDir())
	rd.appLink = sock
	if got := rd.replacer().Replace(spec.PHAppLink); got != sock {
		t.Errorf("the replacer names the socket %q", got)
	}
}
