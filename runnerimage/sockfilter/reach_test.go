package sockfilter

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// reachRig is a filter with a held root (a connection's folder, held to its
// bridge), a root only the tools server may bind (the share), an offline
// root (the share's mirror), and a plain folder beside them.
type reachRig struct {
	*rig
	conn, share, mirror, plain string
}

func newReachRig(t *testing.T) *reachRig {
	t.Helper()
	var r reachRig
	r.rig = newRig(t, Limits{}, func(c *Config) {
		base := c.BindRoots[0]
		r.conn = filepath.Join(base, "cfg", "connections", "remote")
		r.share = filepath.Join(base, "share")
		r.mirror = filepath.Join(base, "share-mirror")
		r.plain = filepath.Join(base, "plain")
		for _, d := range []string{r.conn, r.share, r.mirror, r.plain} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		c.BindRoots = append(c.BindRoots, r.share, r.mirror)
		c.ToolsOnlyRoots = []string{r.share}
		c.OfflineRoots = []string{r.mirror}
		c.HeldRoots = []HeldRoot{{Root: r.conn, By: []Holder{{
			Image: "agentry-runner:test", Cmd: []string{"bridge"},
			Binds: []HolderBind{{Source: r.conn, Target: "/run/agentry/connection"}},
		}}}}
	})
	return &r
}

// TestNothingReadsOrRunsInAContainerThatHoldsARoot: a container that binds a
// held, tools-only or offline root, or a folder above one, takes no exec and
// no archive in either direction; one that binds none of them still does.
func TestNothingReadsOrRunsInAContainerThatHoldsARoot(t *testing.T) {
	g := newReachRig(t)
	g.d.binds["runa-holds-conn"] = []string{g.conn + ":/run/agentry/connection:ro"}
	g.d.binds["runa-holds-share"] = []string{g.share + ":/agent/share:rw"}
	g.d.binds["runa-holds-mirror"] = []string{g.mirror + ":/agent/share:ro"}
	g.d.binds["runa-holds-above"] = []string{filepath.Dir(g.conn) + ":/c:ro"}
	g.d.binds["runa-holds-mixed"] = []string{g.plain + ":/p:ro", g.mirror + ":/agent/share:ro"}
	g.d.binds["runa-plain"] = []string{g.plain + ":/p:ro"}
	for _, id := range []string{"runa-holds-conn", "runa-holds-share", "runa-holds-mirror", "runa-holds-above", "runa-holds-mixed"} {
		for _, c := range []struct{ method, op string }{
			{"POST", "/exec"}, {"GET", "/archive?path=/run"}, {"HEAD", "/archive?path=/run"}, {"PUT", "/archive?path=/"},
		} {
			var body any
			if c.method == "POST" {
				body = map[string]any{"Cmd": []string{"cat", "/run/agentry/connection/bearer"}}
			}
			if c.method == "HEAD" {
				// A HEAD answer carries no body to read the reason from.
				if code, _ := g.do(t, c.method, "/containers/"+id+c.op, body); code != http.StatusForbidden {
					t.Errorf("%s %s on %s: %d, want 403", c.method, c.op, id, code)
				}
				continue
			}
			g.refused(t, c.method+" "+c.op+" on "+id, c.method, "/containers/"+id+c.op, body, "holds files the run keeps from the gateway")
		}
	}
	for _, c := range []struct{ method, op string }{
		{"POST", "/exec"}, {"GET", "/archive?path=/run"}, {"HEAD", "/archive?path=/run"}, {"PUT", "/archive?path=/"},
	} {
		var body any
		if c.method == "POST" {
			body = map[string]any{"Cmd": []string{"true"}}
		}
		if code, got := g.do(t, c.method, "/containers/runa-plain"+c.op, body); code == http.StatusForbidden {
			t.Errorf("%s %s on a container that binds no held folder: %s", c.method, c.op, got)
		}
	}
	// What a held container does for the gateway stays: its logs, its
	// stdio and its stop.
	for _, c := range []struct{ method, op string }{
		{"GET", "/logs"}, {"POST", "/attach"}, {"POST", "/stop"}, {"GET", "/json"}, {"POST", "/start"},
	} {
		if code, got := g.do(t, c.method, "/containers/runa-holds-conn"+c.op, nil); code == http.StatusForbidden {
			t.Errorf("%s %s on a holder: %s", c.method, c.op, got)
		}
	}
}

// TestACreateRunsNoHealthcheckCommand: a health check runs a command inside
// the container it is set on, with that container's binds, so a create
// carries none; one that only switches the image's own off passes.
func TestACreateRunsNoHealthcheckCommand(t *testing.T) {
	g := newReachRig(t)
	create := func(hc any) map[string]any {
		return map[string]any{"Image": "python:3.13-slim", "Cmd": []string{"/x"}, "Healthcheck": hc, "HostConfig": map[string]any{}}
	}
	for name, hc := range map[string]any{
		"a shell command":         map[string]any{"Test": []string{"CMD-SHELL", "cat /run/agentry/connection/bearer"}},
		"a command":               map[string]any{"Test": []string{"CMD", "cat", "/x"}},
		"a command, any case":     map[string]any{"test": []string{"CMD-SHELL", "cat /x"}},
		"none with an interval":   map[string]any{"Test": []string{"NONE"}, "Interval": 1},
		"the image's own":         map[string]any{"Interval": 1000000000},
		"an empty one":            map[string]any{},
		"a test of none and more": map[string]any{"Test": []string{"NONE", "CMD-SHELL", "cat /x"}},
	} {
		g.refused(t, name, "POST", "/containers/create", create(hc), "health check")
	}
	for name, hc := range map[string]any{
		"switched off": map[string]any{"Test": []string{"NONE"}},
		"none":         nil,
	} {
		if code, got := g.do(t, "POST", "/containers/create", create(hc)); code != http.StatusCreated {
			t.Errorf("%s: %d %s", name, code, got)
		}
	}
}

// TestAHolderBindsExactlyItsEntrysFolders: a create that matches a holder's
// image and command is the holder only with the folders its entry binds,
// source and target: a folder swapped in at the entry's target, another bind
// beside them, a missing one, a mount of another kind and a volume at the
// target are each refused, so the command runs the code the entry names.
func TestAHolderBindsExactlyItsEntrysFolders(t *testing.T) {
	store := t.TempDir()
	other := t.TempDir()
	link := filepath.Join(t.TempDir(), "store-link")
	if err := os.Symlink(store, link); err != nil {
		t.Fatal(err)
	}
	var conn string
	cmd := []string{"/opt/agentry-mcp/x/bin/x"}
	g := newRig(t, Limits{}, func(c *Config) {
		conn = filepath.Join(c.BindRoots[0], "cfg", "connections", "x")
		if err := os.MkdirAll(conn, 0o755); err != nil {
			t.Fatal(err)
		}
		c.BindRoots = append(c.BindRoots, store, other)
		c.HeldRoots = []HeldRoot{{Root: conn, By: []Holder{{
			Image: "python:3.13-slim", Cmd: cmd,
			Binds: []HolderBind{
				{Source: store, Target: "/opt/agentry-mcp/x"},
				{Source: conn, Target: "/run/agentry/connection"},
			},
		}}}}
	})
	create := func(binds []string, mounts ...any) map[string]any {
		hc := map[string]any{"Binds": binds}
		if len(mounts) > 0 {
			hc["Mounts"] = mounts
		}
		return map[string]any{"Image": "python:3.13-slim", "Cmd": cmd, "HostConfig": hc}
	}
	const held = "held to the run's container that needs it"
	exact := []string{store + ":/opt/agentry-mcp/x:ro", conn + ":/run/agentry/connection:ro"}
	if code, body := g.do(t, "POST", "/containers/create", create(exact)); code != http.StatusCreated {
		t.Fatalf("the holder with exactly its entry's binds: %d %s", code, body)
	}
	if code, body := g.do(t, "POST", "/containers/create", create([]string{exact[1], exact[0]})); code != http.StatusCreated {
		t.Fatalf("the same binds in another order: %d %s", code, body)
	}
	if code, body := g.do(t, "POST", "/containers/create", create([]string{link + ":/opt/agentry-mcp/x:ro", exact[1]})); code != http.StatusCreated {
		t.Fatalf("the store spelled through a link: %d %s", code, body)
	}
	if code, body := g.do(t, "POST", "/containers/create", create(nil,
		map[string]any{"Type": "bind", "Source": store, "Target": "/opt/agentry-mcp/x", "ReadOnly": true},
		map[string]any{"Type": "bind", "Source": conn, "Target": "/run/agentry/connection", "ReadOnly": true})); code != http.StatusCreated {
		t.Fatalf("the same binds as mounts: %d %s", code, body)
	}
	for name, c := range map[string]map[string]any{
		"another folder at the store's target":   create([]string{other + ":/opt/agentry-mcp/x:ro", exact[1]}),
		"the store at another target":            create([]string{store + ":/opt/agentry-mcp/y:ro", exact[1]}),
		"the token folder at another target":     create([]string{exact[0], conn + ":/elsewhere:ro"}),
		"another bind beside them":               create([]string{exact[0], exact[1], other + ":/extra:ro"}),
		"the token folder alone":                 create([]string{exact[1]}),
		"a named volume at the store's target":   create([]string{exact[1], "runa-vol:/opt/agentry-mcp/x"}),
		"a tmpfs beside them":                    create(exact, map[string]any{"Type": "tmpfs", "Target": "/opt/agentry-mcp/x/bin"}),
		"an anonymous volume beside them":        create(exact, map[string]any{"Type": "volume", "Target": "/opt/agentry-mcp/x/lib"}),
		"a mount of another folder in its place": create([]string{exact[1]}, map[string]any{"Type": "bind", "Source": other, "Target": "/opt/agentry-mcp/x", "ReadOnly": true}),
	} {
		g.refused(t, name, "POST", "/containers/create", c, held)
	}
	// Declared volumes and a tmpfs map name more places than the entry binds.
	withVolumes := create(exact)
	withVolumes["Volumes"] = map[string]any{"/opt/agentry-mcp/x/lib": map[string]any{}}
	g.refused(t, "a declared volume beside them", "POST", "/containers/create", withVolumes, held)
	withTmpfs := create(exact)
	withTmpfs["HostConfig"].(map[string]any)["Tmpfs"] = map[string]string{"/opt/agentry-mcp/x/bin": "rw"}
	g.refused(t, "a tmpfs map beside them", "POST", "/containers/create", withTmpfs, held)
}
