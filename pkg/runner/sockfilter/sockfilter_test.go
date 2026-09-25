package sockfilter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	lbl       = "agentry.run"
	own       = "agentry.gateway.run"
	gatewayUA = "gateway-under-test"
	relayPin  = "docker/mcp-l4proxy@sha256:0123"
	ext       = "docker-mcp-proxies-ext"
	intNet    = "docker-mcp-proxies-int"
)

// fakeDaemon answers the routes the filter consults or forwards, with one
// container, network and volume for each of two runs, the two shared
// relay networks, and a relay container whose image carries the pin.
type fakeDaemon struct {
	mu        sync.Mutex
	forwarded []string          // "METHOD path" the gateway's requests reached the daemon as
	bodies    map[string][]byte // the last body each "METHOD path" carried
}

// imageFor is the daemon's image record for a reference or an id: the
// relay's tag and digest name one pinned image; a moved tag names another;
// anything else is an ordinary image; "missing" is absent.
func imageFor(ref string) (string, []string) {
	switch ref {
	case relayPin, "docker/mcp-l4proxy:v1", "sha256:relay":
		return "sha256:relay", []string{relayPin}
	case "docker/mcp-l4proxy:moved", "sha256:moved":
		return "sha256:moved", []string{"docker/mcp-l4proxy@sha256:9999"}
	case "missing", "":
		return "", nil
	}
	if strings.HasPrefix(ref, "sha256:img-") {
		return ref, nil
	}
	return "sha256:img-" + strings.NewReplacer("/", "-", ":", "-").Replace(ref), nil
}

// labelsFor is what the daemon holds for an object of a run: the gateway's
// objects carry both labels, the runner's own ("…-agent") the run's alone.
func labelsFor(id, run string) map[string]string {
	if strings.Contains(id, "agent") || strings.HasPrefix(id, "docker-mcp-proxies") || id == "extid" {
		return map[string]string{lbl: run}
	}
	return map[string]string{lbl: run, own: run}
}

func (d *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := canonical(r.URL.Path)
	owner := func(id string) (string, bool) {
		switch {
		case strings.HasPrefix(id, "runa"):
			return "A", true
		case strings.HasPrefix(id, "runb"):
			return "B", true
		case id == intNet, id == ext, id == "extid":
			return "A", true // the runner's, for run A
		case strings.HasPrefix(id, "agentry-A-relays"):
			return "A", true // the runner's, for run A
		case strings.HasPrefix(id, "agentry-B-relays"):
			return "B", true
		}
		return "", false
	}
	gateway := r.Header.Get("User-Agent") == gatewayUA
	switch {
	case strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json") && !gateway:
		ref := strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/json")
		id, digests := imageFor(ref)
		if id == "" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": id, "RepoDigests": digests})
		return
	case strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json") && p != "/containers/json":
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
		if gateway {
			d.mu.Lock()
			d.forwarded = append(d.forwarded, r.Method+" "+p)
			d.mu.Unlock()
		}
		run, ok := owner(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		image, _ := imageFor("alpine:3")
		if strings.Contains(id, "relay") {
			image, _ = imageFor(relayPin)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id": id + "-full", "Image": image, "Config": map[string]any{"Labels": labelsFor(id, run)},
			"NetworkSettings": map[string]any{"Networks": map[string]any{"agentry-A-relays": map[string]any{"IPAddress": "10.9.0.5"}}},
		})
		return
	case strings.HasPrefix(p, "/networks/") && r.Method == http.MethodGet && !gateway:
		id := strings.TrimPrefix(p, "/networks/")
		run, ok := owner(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		name := id
		if id == "extid" {
			name = ext
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": "id-" + name, "Name": name, "Labels": labelsFor(id, run)})
		return
	case strings.HasPrefix(p, "/volumes/") && r.Method == http.MethodGet && !gateway:
		id := strings.TrimPrefix(p, "/volumes/")
		run, ok := owner(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Name": id, "Labels": labelsFor(id, run)})
		return
	}
	body, _ := io.ReadAll(r.Body)
	d.mu.Lock()
	d.forwarded = append(d.forwarded, r.Method+" "+p)
	d.bodies[r.Method+" "+p] = body
	d.mu.Unlock()
	switch {
	case p == "/containers/json":
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"Id": "runa-1", "Labels": labelsFor("runa-1", "A")},
			{"Id": "runa-agent", "Labels": labelsFor("runa-agent", "A")},
			{"Id": "runb-1", "Labels": labelsFor("runb-1", "B")},
		})
	case p == "/networks":
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"Name": "runa-net", "Labels": labelsFor("runa-net", "A")},
			{"Name": "runa-agent-net", "Labels": labelsFor("runa-agent-net", "A")},
			{"Name": "runb-net", "Labels": labelsFor("runb-net", "B")},
			{"Name": intNet, "Labels": labelsFor(intNet, "B")},
		})
	case strings.HasSuffix(p, "/create"):
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Id":"new"}`))
	case strings.HasSuffix(p, "/exec"):
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Id":"exec-1"}`))
	case strings.HasSuffix(p, "/json"):
		_, _ = w.Write([]byte(`{}`))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

type rig struct {
	f        *Filter
	d        *fakeDaemon
	client   *http.Client
	root     string // a read-only bind root
	writable string // the writable one inside it
}

func newRig(t *testing.T, lim Limits, opts ...func(*Config)) *rig {
	t.Helper()
	dir, err := os.MkdirTemp("", "sf") // short: unix socket paths are capped near 108 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	up := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", up)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDaemon{bodies: map[string][]byte{}}
	srv := &http.Server{Handler: d} //nolint:gosec // test server
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	root := filepath.Join(dir, "run")
	writable := filepath.Join(root, "out")
	if err := os.MkdirAll(writable, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		RunID: "A", Label: lbl, OwnerLabel: own, Listen: filepath.Join(dir, "f.sock"), Upstream: up,
		RelayHosts: []string{"api.osv.dev", "*.example.org"},
		BindRoots:  []string{root}, WritableRoots: []string{writable},
		SharedNetworks: []string{intNet, "docker-mcp-gone"},
		EgressNetworks: map[string][]string{ext: {relayPin}},
		Limits:         lim,
	}
	for _, o := range opts {
		o(&cfg)
	}
	f, err := Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dl net.Dialer
		return dl.DialContext(ctx, "unix", filepath.Join(dir, "f.sock"))
	}}}
	return &rig{f: f, d: d, client: client, root: root, writable: writable}
}

// do sends a request as the gateway would; body is JSON-encoded unless it
// is already a string.
func (g *rig) do(t *testing.T, method, p string, body any) (int, string) {
	t.Helper()
	var rdr io.Reader
	// A create that names no image is given an ordinary one, so a case
	// is refused for what it tests.
	if m, ok := body.(map[string]any); ok && strings.HasPrefix(p, "/containers/create") {
		if _, has := m["Image"]; !has {
			m["Image"] = "mcp/server"
		}
	}
	switch b := body.(type) {
	case nil:
	case string:
		rdr = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rdr = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, "http://docker/v1.47"+p, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", gatewayUA)
	res, err := g.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, p, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func (g *rig) forwarded(what string) bool {
	g.d.mu.Lock()
	defer g.d.mu.Unlock()
	for _, f := range g.d.forwarded {
		if f == what {
			return true
		}
	}
	return false
}

func (g *rig) body(what string) map[string]any {
	g.d.mu.Lock()
	defer g.d.mu.Unlock()
	var doc map[string]any
	_ = json.Unmarshal(g.d.bodies[what], &doc)
	return doc
}

// refused asserts a request is refused by the filter, for the reason given.
func (g *rig) refused(t *testing.T, name, method, p string, body any, reason string) {
	t.Helper()
	code, got := g.do(t, method, p, body)
	if code != http.StatusForbidden || !strings.Contains(got, "agentry run filter") || !strings.Contains(got, reason) {
		t.Errorf("%s: %d %s, want 403 for %q", name, code, strings.TrimSpace(got), reason)
	}
}

// The acceptance test of agentry-cli#219: run A's gateway tries to list,
// inspect, exec into, stop and create against run B's objects, and each is
// refused before it reaches the daemon; the same verbs on its own pass.
func TestAnotherRunsObjectsAreRefused(t *testing.T) {
	g := newRig(t, Limits{})
	cases := []struct{ method, path string }{
		{"GET", "/containers/runb-1/json"},
		{"GET", "/containers/runb-1/logs"},
		{"POST", "/containers/runb-1/exec"},
		{"POST", "/containers/runb-1/stop"},
		{"POST", "/containers/runb-1/kill"},
		{"POST", "/containers/runb-1/start"},
		{"POST", "/containers/runb-1/attach"},
		{"DELETE", "/containers/runb-1"},
		{"GET", "/networks/runb-net"},
		{"DELETE", "/networks/runb-net"},
		{"GET", "/volumes/runb-vol"},
		{"DELETE", "/volumes/runb-vol"},
		{"POST", "/exec/exec-of-b/start"},
	}
	for _, c := range cases {
		g.refused(t, c.method+" "+c.path, c.method, c.path, map[string]any{}, "")
		if g.forwarded(c.method+" "+c.path) || g.forwarded(c.method+" "+strings.Replace(c.path, "runb-1", "runb-1-full", 1)) {
			t.Errorf("%s %s reached the daemon", c.method, c.path)
		}
	}
	g.refused(t, "create on run B's network", "POST", "/containers/create", map[string]any{"HostConfig": map[string]any{"NetworkMode": "runb-net"}}, "not this run's")
	g.refused(t, "create sharing run B's pid namespace", "POST", "/containers/create", map[string]any{"HostConfig": map[string]any{"PidMode": "container:runb-1"}}, "PidMode")
	g.refused(t, "connect run B's container", "POST", "/networks/runa-net/connect", map[string]any{"Container": "runb-1"}, "not this run's")
	if got, want := g.f.Refused(), len(cases)+3; got != want {
		t.Errorf("Refused() = %d, want %d", got, want)
	}

	for _, c := range []struct{ method, path string }{
		{"GET", "/containers/runa-1/json"},
		{"POST", "/containers/runa-1/stop"},
		{"DELETE", "/containers/runa-1"},
		{"GET", "/networks/runa-net"},
		{"GET", "/networks/" + intNet},
	} {
		if code, body := g.do(t, c.method, c.path, nil); code == http.StatusForbidden {
			t.Errorf("own %s %s refused: %s", c.method, c.path, body)
		}
	}
	// A container named by its name reaches the daemon by the id checked.
	if !g.forwarded("POST /containers/runa-1-full/stop") {
		t.Error("the stop was not forwarded under the container's checked id")
	}
}

// The runner's own containers carry the run's label but were not made
// through the filter: the gateway may not exec into, stop, inspect or join
// the agent (which holds the model key) or the model proxy (which has a
// way out), nor use the run's network.
func TestTheRunnersOwnContainersAreNotTheGateways(t *testing.T) {
	g := newRig(t, Limits{})
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/containers/runa-agent/exec", map[string]any{"Cmd": []string{"sh"}}},
		{"GET", "/containers/runa-agent/json", nil},
		{"POST", "/containers/runa-agent/stop", nil},
		{"PUT", "/containers/runa-agent/archive?path=/", "tar"},
		{"GET", "/networks/runa-agent-net", nil},
		{"POST", "/networks/runa-agent-net/connect", map[string]any{"Container": "runa-1"}},
		{"POST", "/containers/create", map[string]any{"HostConfig": map[string]any{"NetworkMode": "runa-agent-net"}}},
	} {
		g.refused(t, c.method+" "+c.path, c.method, c.path, c.body, "not this run's")
	}
	g.refused(t, "joining the agent's pid namespace", "POST", "/containers/create", map[string]any{"HostConfig": map[string]any{"PidMode": "container:runa-agent"}}, "PidMode")
}

func TestTheJudgedPathIsTheForwardedPath(t *testing.T) {
	g := newRig(t, Limits{})
	for _, raw := range []string{
		"/v1.47/containers/runa-1/../runb-1/json",
		"/v1.47/containers/runa-1%2F..%2Frunb-1/json",
		"/v1.47/containers/runb-1%2Fjson",
	} {
		req, _ := http.NewRequest(http.MethodGet, "http://docker"+raw, nil)
		req.Header.Set("User-Agent", gatewayUA)
		res, err := g.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want refused or absent", raw, res.StatusCode)
		}
	}
	if g.forwarded("GET /containers/runb-1-full/json") || g.forwarded("GET /containers/runb-1/json") {
		t.Error("run B's container reached the daemon through a crafted path")
	}
	if code, _ := g.do(t, "GET", "/containers/./runa-1/json", nil); code != http.StatusOK {
		t.Errorf("own container through a dot segment: %d", code)
	}
	if !g.forwarded("GET /containers/runa-1-full/json") {
		t.Error("the cleaned, resolved path was not the one forwarded")
	}
}

// A missing object gets the daemon's own 404, not a refusal: the pinned
// gateway inspects its hostname as a container and expects none.
func TestAbsentObjectsAreTheDaemonsNotFound(t *testing.T) {
	g := newRig(t, Limits{})
	for _, p := range []string{"/containers/gw-nothere/json", "/networks/nothere", "/volumes/nothere"} {
		code, body := g.do(t, "GET", p, nil)
		if code != http.StatusNotFound || !strings.Contains(body, "No such") {
			t.Errorf("GET %s: %d %s, want the daemon's 404", p, code, body)
		}
	}
	if n := g.f.Refused(); n != 0 {
		t.Errorf("absent objects counted as %d refusals", n)
	}
}

func TestListsShowOnlyTheRun(t *testing.T) {
	g := newRig(t, Limits{})
	_, body := g.do(t, "GET", "/containers/json?all=1", nil)
	if !strings.Contains(body, "runa-1") || strings.Contains(body, "runb-1") || strings.Contains(body, "runa-agent") {
		t.Errorf("containers list: %s", body)
	}
	_, body = g.do(t, "GET", "/networks", nil)
	if !strings.Contains(body, "runa-net") || strings.Contains(body, "runb-net") || !strings.Contains(body, intNet) {
		t.Errorf("networks list: %s", body)
	}
}

func TestCreateCarriesTheRunLabelAndWhatWasChecked(t *testing.T) {
	g := newRig(t, Limits{})
	code, body := g.do(t, "POST", "/containers/create", map[string]any{
		"Image":  "mcp/server",
		"Labels": map[string]string{lbl: "B", "docker-mcp": "true"},
		"HostConfig": map[string]any{
			"NetworkMode": "none",
			"Binds":       []string{g.root + "/:/work:ro", g.writable + ":/out"},
			"SecurityOpt": []string{"no-new-privileges"},
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	doc := g.body("POST /containers/create")
	labels, _ := doc["Labels"].(map[string]any)
	if labels[lbl] != "A" || labels[own] != "A" || labels["docker-mcp"] != "true" {
		t.Errorf("created with labels %v, want %s=A and %s=A beside the gateway's own", labels, lbl, own)
	}
	if code, _ := g.do(t, "POST", "/networks/create", map[string]any{"Name": "x", "Internal": true}); code != http.StatusCreated {
		t.Fatalf("network create: %d", code)
	}
	if labels, _ := g.body("POST /networks/create")["Labels"].(map[string]any); labels[lbl] != "A" {
		t.Errorf("network labels %v", labels)
	}
	// Keys in another case reach the daemon under its own names.
	if code, body := g.do(t, "POST", "/containers/create", `{"image":"mcp/server","hostconfig":{"networkmode":"none"}}`); code != http.StatusCreated {
		t.Fatalf("lower-case create: %d %s", code, body)
	}
	doc = g.body("POST /containers/create")
	if doc["Image"] != "sha256:img-mcp-server" || doc["image"] != nil {
		t.Errorf("forwarded body kept the caller's spelling: %v", doc)
	}
}

func TestCreateRefusesWhatReachesPastTheRun(t *testing.T) {
	g := newRig(t, Limits{})
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(g.writable, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		body   any
		reason string
	}{
		{"privileged", map[string]any{"HostConfig": map[string]any{"Privileged": true}}, "privileged"},
		{"privileged, any case", `{"hostconfig":{"privileged":true}}`, "privileged"},
		{"privileged, mixed case", `{"HostConfig":{"PRIVILEGED":true}}`, "privileged"},
		{"a field the filter does not know", `{"HostConfig":{"Privileged2":true}}`, "does not judge"},
		{"a second value", `{"Image":"x"}{"HostConfig":{"Privileged":true}}`, "more than one value"},
		{"cap add", map[string]any{"HostConfig": map[string]any{"CapAdd": []string{"SYS_ADMIN"}}}, "CapAdd"},
		{"device", map[string]any{"HostConfig": map[string]any{"Devices": []map[string]string{{"PathOnHost": "/dev/kmsg"}}}}, "devices"},
		{"host pid", map[string]any{"HostConfig": map[string]any{"PidMode": "host"}}, "PidMode"},
		{"a relay's pid namespace", map[string]any{"HostConfig": map[string]any{"PidMode": "container:runa-relay"}}, "PidMode"},
		{"a relay's ipc namespace", map[string]any{"HostConfig": map[string]any{"IpcMode": "container:runa-relay"}}, "IpcMode"},
		{"an unkillable container", map[string]any{"HostConfig": map[string]any{"OomScoreAdj": -1000}}, "OOM score"},
		{"no OOM kill", map[string]any{"HostConfig": map[string]any{"OomKillDisable": true}}, "OOM killer"},
		{"realtime priority", map[string]any{"HostConfig": map[string]any{"Ulimits": []map[string]any{{"Name": "rtprio", "Soft": 99, "Hard": 99}}}}, "ulimits"},
		{"a heavier CPU weight", map[string]any{"HostConfig": map[string]any{"CpuShares": 262144}}, "CPU share"},
		{"a writable directory inside the run's writable one", map[string]any{"HostConfig": map[string]any{"Binds": []string{g.writable + "/d:/x"}}}, "inside a directory the run writes"},
		{"a read-only directory inside the run's writable one", map[string]any{"HostConfig": map[string]any{"Binds": []string{g.writable + "/d:/x:ro"}}}, "inside a directory the run writes"},
		{"host userns", map[string]any{"HostConfig": map[string]any{"UsernsMode": "host"}}, "UsernsMode"},
		{"host network", map[string]any{"HostConfig": map[string]any{"NetworkMode": "host"}}, "host networking"},
		{"default bridge", map[string]any{"HostConfig": map[string]any{"NetworkMode": "bridge"}}, "default bridge"},
		{"another's network namespace", map[string]any{"HostConfig": map[string]any{"NetworkMode": "container:runa-relay"}}, "network namespace"},
		{"published port", map[string]any{"HostConfig": map[string]any{"PortBindings": map[string]any{"80/tcp": []map[string]string{{"HostPort": "80"}}}}}, "publishing ports"},
		{"seccomp off", map[string]any{"HostConfig": map[string]any{"SecurityOpt": []string{"seccomp=unconfined"}}}, "SecurityOpt"},
		{"a seccomp profile", map[string]any{"HostConfig": map[string]any{"SecurityOpt": []string{`seccomp={"defaultAction":"SCMP_ACT_ALLOW"}`}}}, "SecurityOpt"},
		{"selinux off", map[string]any{"HostConfig": map[string]any{"SecurityOpt": []string{"label:disable"}}}, "SecurityOpt"},
		{"selinux level", map[string]any{"HostConfig": map[string]any{"SecurityOpt": []string{"label=level:s0:c1,c2"}}}, "SecurityOpt"},
		{"another runtime", map[string]any{"HostConfig": map[string]any{"Runtime": "runc"}}, "Runtime"},
		{"unmasked /proc", map[string]any{"HostConfig": map[string]any{"MaskedPaths": []string{}}}, "unmasking"},
		{"a shorter mask", map[string]any{"HostConfig": map[string]any{"MaskedPaths": []string{"/x"}}}, "unmasking"},
		{"writable kernel paths", map[string]any{"HostConfig": map[string]any{"ReadonlyPaths": []string{}}}, "unmasking"},
		{"annotations", map[string]any{"HostConfig": map[string]any{"Annotations": map[string]string{"dev.gvisor.x": "y"}}}, "annotations"},
		{"a log driver off the host", map[string]any{"HostConfig": map[string]any{"LogConfig": map[string]any{"Type": "syslog"}}}, "log driver"},
		{"restart always", map[string]any{"HostConfig": map[string]any{"RestartPolicy": map[string]any{"Name": "always"}}}, "restart policy"},
		{"cgroup parent", map[string]any{"HostConfig": map[string]any{"CgroupParent": "/"}}, "cgroup"},
		{"sysctls", map[string]any{"HostConfig": map[string]any{"Sysctls": map[string]string{"kernel.x": "1"}}}, "Sysctls"},
		{"volumes from", map[string]any{"HostConfig": map[string]any{"VolumesFrom": []string{"runa-1"}}}, "VolumesFrom"},
		{"docker socket", map[string]any{"HostConfig": map[string]any{"Binds": []string{"/var/run/docker.sock:/var/run/docker.sock:ro"}}}, "/var/run/docker.sock"},
		{"bind outside", map[string]any{"HostConfig": map[string]any{"Binds": []string{outside + ":/x:ro"}}}, "outside the run's directories"},
		{"bind escape", map[string]any{"HostConfig": map[string]any{"Binds": []string{g.root + "/../..:/x:ro"}}}, "outside the run's directories"},
		{"writable bind of a read-only root", map[string]any{"HostConfig": map[string]any{"Binds": []string{g.root + ":/x"}}}, "not one the run lets the gateway write"},
		{"bind propagation", map[string]any{"HostConfig": map[string]any{"Binds": []string{g.root + ":/x:ro,rshared"}}}, "bind option"},
		{"bind relabel", map[string]any{"HostConfig": map[string]any{"Binds": []string{g.root + ":/x:ro,z"}}}, "bind option"},
		{"mount outside", map[string]any{"HostConfig": map[string]any{"Mounts": []map[string]any{{"Type": "bind", "Source": outside, "Target": "/x", "ReadOnly": true}}}}, "outside the run's directories"},
		{"writable mount of a read-only root", map[string]any{"HostConfig": map[string]any{"Mounts": []map[string]any{{"Type": "bind", "Source": g.root, "Target": "/x"}}}}, "not one the run lets the gateway write"},
		{"anonymous volume as a host bind", map[string]any{"HostConfig": map[string]any{"Mounts": []map[string]any{{"Type": "volume", "Target": "/x", "VolumeOptions": map[string]any{"DriverConfig": map[string]any{"Name": "local", "Options": map[string]string{"type": "none", "o": "bind", "device": "/"}}}}}}}, "driver configuration"},
		{"other run's volume", map[string]any{"HostConfig": map[string]any{"Binds": []string{"runb-vol:/data"}}}, "not this run's"},
	} {
		g.refused(t, c.name, "POST", "/containers/create", c.body, c.reason)
	}
	// A symlink under the run's directory that points outside it.
	link := filepath.Join(g.writable, "out-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	g.refused(t, "bind through a symlink out of the run", "POST", "/containers/create",
		map[string]any{"HostConfig": map[string]any{"Binds": []string{link + ":/x"}}}, "not one the run lets the gateway write")
	// A symlink inside the run is forwarded as what it resolves to.
	inside := filepath.Join(g.writable, "in-link")
	if err := os.Symlink(g.writable, inside); err != nil {
		t.Fatal(err)
	}
	if code, body := g.do(t, "POST", "/containers/create", map[string]any{"Image": "mcp/server", "HostConfig": map[string]any{"Binds": []string{inside + ":/x"}}}); code != http.StatusCreated {
		t.Fatalf("bind through a link inside the run: %d %s", code, body)
	}
	hc, _ := g.body("POST /containers/create")["HostConfig"].(map[string]any)
	if binds, _ := hc["Binds"].([]any); len(binds) != 1 || strings.Contains(binds[0].(string), "in-link") {
		t.Errorf("forwarded binds %v, want the resolved path", hc["Binds"])
	}
}

func TestNetworksAndVolumesTheGatewayMayMake(t *testing.T) {
	g := newRig(t, Limits{})
	for _, c := range []struct {
		name   string
		body   any
		reason string
	}{
		{"not internal", map[string]any{"Name": "n"}, "not internal"},
		{"macvlan", map[string]any{"Name": "n", "Internal": true, "Driver": "macvlan"}, "macvlan"},
		{"driver options", map[string]any{"Name": "n", "Internal": true, "Options": map[string]string{"com.docker.network.bridge.gateway_mode_ipv4": "routed"}}, "driver options"},
		{"own address plan", map[string]any{"Name": "n", "Internal": true, "IPAM": map[string]any{"Config": []map[string]string{{"Subnet": "10.0.0.0/8"}}}}, "address plan"},
		{"a shared network the runner has not made", map[string]any{"Name": "docker-mcp-gone", "Internal": true}, "the runner makes it"},
	} {
		g.refused(t, c.name, "POST", "/networks/create", c.body, c.reason)
	}
	// Asked for a shared network that is there, the gateway gets the
	// daemon's own "already exists", whatever it asked the network to be.
	for _, name := range []string{intNet, ext} {
		code, body := g.do(t, "POST", "/networks/create", map[string]any{"Name": name})
		if code != http.StatusConflict || !strings.Contains(body, "already exists") {
			t.Errorf("create %s: %d %s, want the daemon's 409", name, code, body)
		}
		if g.forwarded("POST /networks/create") {
			t.Errorf("create %s reached the daemon", name)
		}
	}
	g.refused(t, "removing the shared network", "DELETE", "/networks/"+intNet, nil, "not the gateway's to remove")
	g.refused(t, "a name in the runner's namespace", "POST", "/networks/create", map[string]any{"Name": "agentry-B-relays", "Internal": true}, "the runner's")
	g.refused(t, "a volume with driver options", "POST", "/volumes/create", map[string]any{"Name": "v", "DriverOpts": map[string]string{"device": "/"}}, "driver options")
}

// The relays' way out takes the pinned relays and nothing else, however
// the network or the image is named, at create and at connect.
func TestTheEgressNetworkTakesOnlyThePinnedRelay(t *testing.T) {
	g := newRig(t, Limits{})
	for _, c := range []struct {
		name, image, network string
		endpoint             bool
		want                 int
	}{
		{"relay by digest", relayPin, ext, false, http.StatusCreated},
		{"relay by tag, pulled at the pin", "docker/mcp-l4proxy:v1", ext, true, http.StatusCreated},
		{"relay whose tag moved", "docker/mcp-l4proxy:moved", ext, false, http.StatusForbidden},
		{"a server", "mcp/osv", ext, false, http.StatusForbidden},
		{"a server, by endpoint", "mcp/osv", ext, true, http.StatusForbidden},
		{"a server, the network named by id", "mcp/osv", "extid", false, http.StatusForbidden},
		{"a server, by id, by endpoint", "mcp/osv", "extid", true, http.StatusForbidden},
	} {
		doc := map[string]any{"Image": c.image, "Env": []string{"PROXY_HOSTNAME=api.osv.dev", "PROXY_PORTS=443"}, "HostConfig": map[string]any{"NetworkMode": c.network}}
		if c.endpoint {
			doc["HostConfig"] = map[string]any{"NetworkMode": "none"}
			doc["NetworkingConfig"] = map[string]any{"EndpointsConfig": map[string]any{c.network: map[string]any{}}}
		}
		if code, body := g.do(t, "POST", "/containers/create", doc); code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.want)
		}
	}
	g.refused(t, "connecting a non-relay to the way out", "POST", "/networks/"+ext+"/connect", map[string]any{"Container": "runa-1"}, "pinned relays only")
	g.refused(t, "connecting a non-relay by the network's id", "POST", "/networks/extid/connect", map[string]any{"Container": "runa-1"}, "pinned relays only")
	if code, body := g.do(t, "POST", "/networks/"+ext+"/connect", map[string]any{"Container": "runa-relay"}); code == http.StatusForbidden {
		t.Errorf("connecting the run's relay refused: %s", body)
	}
	if code, body := g.do(t, "POST", "/networks/"+ext+"/disconnect", map[string]any{"Container": "runa-1"}); code == http.StatusForbidden {
		t.Errorf("disconnecting the run's container refused: %s", body)
	}
	g.refused(t, "exec into the relay", "POST", "/containers/runa-relay/exec", map[string]any{"Cmd": []string{"sh"}}, "relay")
	g.refused(t, "writing into the relay", "PUT", "/containers/runa-relay/archive?path=/", "tar", "relay")
}

// A relay is its image, pointed at a host the run's servers declared, and
// nothing more: no code of the gateway's, no files, no other settings.
func TestARelayIsOnlyARelay(t *testing.T) {
	g := newRig(t, Limits{})
	relay := func(extra map[string]any, env ...string) map[string]any {
		doc := map[string]any{"Image": relayPin, "Env": env, "HostConfig": map[string]any{"NetworkMode": ext}}
		for k, v := range extra {
			doc[k] = v
		}
		return doc
	}
	if code, body := g.do(t, "POST", "/containers/create", relay(nil, "PROXY_HOSTNAME=api.osv.dev", "PROXY_PORTS=443")); code != http.StatusCreated {
		t.Fatalf("the relay the gateway makes: %d %s", code, body)
	}
	if code, body := g.do(t, "POST", "/containers/create", relay(nil, "PROXY_HOSTNAME=cdn.example.org", "PROXY_PORTS=443,80")); code != http.StatusCreated {
		t.Errorf("a relay to a host under a declared wildcard: %d %s", code, body)
	}
	for _, c := range []struct {
		name   string
		doc    map[string]any
		reason string
	}{
		{"an undeclared host", relay(nil, "PROXY_HOSTNAME=evil.example", "PROXY_PORTS=443"), "not one the run's servers declared"},
		{"the wildcard's own apex", relay(nil, "PROXY_HOSTNAME=example.org", "PROXY_PORTS=443"), "not one the run's servers declared"},
		{"a config line in the host", relay(nil, "PROXY_HOSTNAME=api.osv.dev\nglobal\n master-worker", "PROXY_PORTS=443"), "not a host name"},
		{"a config line in the ports", relay(nil, "PROXY_HOSTNAME=api.osv.dev", "PROXY_PORTS=443\nprogram x"), "not ports"},
		{"another setting", relay(nil, "PROXY_HOSTNAME=api.osv.dev", "HAPROXY_CFG=/x"), "relay setting"},
		{"its own entrypoint", relay(map[string]any{"Entrypoint": []string{"sh", "-c", "curl evil"}}), "entrypoint"},
		{"its own command", relay(map[string]any{"Cmd": []string{"-f", "/tmp/x"}}), "entrypoint"},
		{"a healthcheck", relay(map[string]any{"Healthcheck": map[string]any{"Test": []string{"CMD", "sh"}}}), "entrypoint"},
		{"another user", relay(map[string]any{"User": "root"}), "user"},
		{"files", relay(map[string]any{"HostConfig": map[string]any{"NetworkMode": ext, "Tmpfs": map[string]string{"/x": ""}}}), "no files"},
		{"on an internal network, then out", map[string]any{"Image": relayPin, "Entrypoint": []string{"sh"}, "HostConfig": map[string]any{"NetworkMode": "none"}}, "entrypoint"},
		{"the host re-pointed", relay(map[string]any{"HostConfig": map[string]any{"NetworkMode": ext, "ExtraHosts": []string{"api.osv.dev:192.0.2.1"}}}, "PROXY_HOSTNAME=api.osv.dev", "PROXY_PORTS=443"), "resolves its host"},
		{"its own DNS", relay(map[string]any{"HostConfig": map[string]any{"NetworkMode": ext, "Dns": []string{"192.0.2.53"}}}, "PROXY_HOSTNAME=api.osv.dev", "PROXY_PORTS=443"), "resolves its host"},
		{"an undeclared port", relay(nil, "PROXY_HOSTNAME=api.osv.dev", "PROXY_PORTS=443,22"), "port 22"},
		{"no host at all", relay(nil, "PROXY_PORTS=443"), "not a host name"},
	} {
		g.refused(t, c.name, "POST", "/containers/create", c.doc, c.reason)
	}
}

// A relay may be declared with its port, and then takes that port alone;
// with no declaration it may not be made at all.
func TestARelaysPortsAreTheDeclaredOnes(t *testing.T) {
	g := newRig(t, Limits{}, func(c *Config) { c.RelayHosts = []string{"api.osv.dev:8443"} })
	make := func(ports string) int {
		code, _ := g.do(t, "POST", "/containers/create", map[string]any{"Image": relayPin, "Env": []string{"PROXY_HOSTNAME=api.osv.dev", "PROXY_PORTS=" + ports}, "HostConfig": map[string]any{"NetworkMode": ext}})
		return code
	}
	if code := make("8443"); code != http.StatusCreated {
		t.Errorf("the declared port: %d", code)
	}
	if code := make("443"); code != http.StatusForbidden {
		t.Errorf("an undeclared port: %d", code)
	}
	none := newRig(t, Limits{}, func(c *Config) { c.RelayHosts = nil })
	none.refused(t, "a relay with nothing declared", "POST", "/containers/create",
		map[string]any{"Image": relayPin, "Env": []string{"PROXY_HOSTNAME=api.osv.dev", "PROXY_PORTS=443"}, "HostConfig": map[string]any{"NetworkMode": ext}}, "not one the run's servers declared")
}

// What a container is, and where a request goes, is fixed by id: a tag
// moved after the check changes nothing the filter decided.
func TestImagesAndNetworksAreJudgedAndForwardedByID(t *testing.T) {
	g := newRig(t, Limits{})
	if code, body := g.do(t, "POST", "/containers/create", map[string]any{"Image": "docker/mcp-l4proxy:v1", "Env": []string{"PROXY_HOSTNAME=api.osv.dev", "PROXY_PORTS=443"}, "HostConfig": map[string]any{"NetworkMode": ext}}); code != http.StatusCreated {
		t.Fatalf("relay by tag: %d %s", code, body)
	}
	doc := g.body("POST /containers/create")
	hc, _ := doc["HostConfig"].(map[string]any)
	if doc["Image"] != "sha256:relay" || hc["NetworkMode"] != "id-"+ext {
		t.Errorf("forwarded image %v on network %v, want both by id", doc["Image"], hc["NetworkMode"])
	}
	if code, body := g.do(t, "POST", "/containers/create", map[string]any{"Image": "missing"}); code != http.StatusNotFound {
		t.Errorf("a missing image: %d %s, want the daemon's 404", code, body)
	}
	if code, _ := g.do(t, "POST", "/networks/runa-net/connect", map[string]any{"Container": "runa-1"}); code == http.StatusForbidden {
		t.Fatal("own connect refused")
	}
	if !g.forwarded("POST /networks/id-runa-net/connect") {
		t.Error("the connect was not forwarded under the network's id")
	}
}

// The gateway's fixed relay network names are the run's own networks: it
// asks for, joins and reads them by the names it knows, the daemon sees
// the run's, and another run's are out of reach under any name.
func TestTheGatewaysRelayNamesAreTheRunsNetworks(t *testing.T) {
	g := newRig(t, Limits{}, func(c *Config) {
		c.SharedNetworks = []string{"agentry-A-relays"}
		c.EgressNetworks = map[string][]string{"agentry-A-relays-out": {relayPin}}
		c.NetworkNames = map[string]string{intNet: "agentry-A-relays", ext: "agentry-A-relays-out"}
	})
	for _, name := range []string{intNet, ext} {
		if code, body := g.do(t, "POST", "/networks/create", map[string]any{"Name": name, "Internal": name == intNet}); code != http.StatusConflict || !strings.Contains(body, name) {
			t.Errorf("create %s: %d %s, want the daemon's 409 under the gateway's name", name, code, body)
		}
	}
	if code, body := g.do(t, "POST", "/containers/create", map[string]any{
		"Image": relayPin, "Env": []string{"PROXY_HOSTNAME=api.osv.dev", "PROXY_PORTS=443"},
		"HostConfig":       map[string]any{"NetworkMode": ext},
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{intNet: map[string]any{}}},
	}); code != http.StatusCreated {
		t.Fatalf("relay on the gateway's names: %d %s", code, body)
	}
	doc := g.body("POST /containers/create")
	hc, _ := doc["HostConfig"].(map[string]any)
	eps, _ := doc["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)
	if hc["NetworkMode"] != "id-agentry-A-relays-out" || eps["id-agentry-A-relays"] == nil {
		t.Errorf("forwarded to %v and %v, want the run's networks by id", hc["NetworkMode"], eps)
	}
	// The relay, read back, is on the network the gateway knows.
	_, body := g.do(t, "GET", "/containers/runa-relay/json", nil)
	if !strings.Contains(body, `"`+intNet+`"`) || strings.Contains(body, "agentry-A-relays") {
		t.Errorf("relay inspect shows %s", body)
	}
	// Another run's relay networks, by their own names.
	g.refused(t, "another run's relay network", "POST", "/containers/create", map[string]any{"HostConfig": map[string]any{"NetworkMode": "agentry-B-relays"}}, "not this run's")
	g.refused(t, "another run's way out", "POST", "/networks/agentry-B-relays-out/connect", map[string]any{"Container": "runa-relay"}, "not this run's")
	// The run's own may not be removed by the gateway.
	g.refused(t, "removing the run's relay network", "DELETE", "/networks/"+intNet, nil, "not the gateway's to remove")
}

func TestExecsAreTheRunsOwnAndUnprivileged(t *testing.T) {
	g := newRig(t, Limits{})
	g.refused(t, "a privileged exec", "POST", "/containers/runa-1/exec", `{"Cmd":["sh"],"privileged":true}`, "privileged exec")
	if code, body := g.do(t, "POST", "/containers/runa-1/exec", map[string]any{"Cmd": []string{"true"}}); code != http.StatusCreated {
		t.Fatalf("exec create: %d %s", code, body)
	}
	if code, body := g.do(t, "POST", "/exec/exec-1/start", map[string]any{}); code == http.StatusForbidden {
		t.Errorf("own exec start refused: %s", body)
	}
	g.refused(t, "an exec not created here", "GET", "/exec/exec-2/json", nil, "not created through")
}

func TestTheRunsLimitsBoundEveryContainer(t *testing.T) {
	g := newRig(t, Limits{MemoryBytes: 2 << 30, NanoCPUs: 1e9, Pids: 512})
	if code, body := g.do(t, "POST", "/containers/create", map[string]any{"Image": "mcp/server"}); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	hc, _ := g.body("POST /containers/create")["HostConfig"].(map[string]any)
	if hc["Memory"] != float64(2<<30) || hc["NanoCpus"] != float64(1e9) || hc["PidsLimit"] != float64(512) {
		t.Errorf("unbounded create got %v", hc)
	}
	g.refused(t, "more memory", "POST", "/containers/create", map[string]any{"HostConfig": map[string]any{"Memory": 4 << 30}}, "memory")
	g.refused(t, "more CPU", "POST", "/containers/create", map[string]any{"HostConfig": map[string]any{"NanoCpus": 2e9}}, "CPU")
	g.refused(t, "unbounded processes", "POST", "/containers/create", map[string]any{"HostConfig": map[string]any{"PidsLimit": 100000}}, "pids")
	g.refused(t, "CPU by quota", "POST", "/containers/create", map[string]any{"HostConfig": map[string]any{"CpuQuota": 200000}}, "quota")
	g.refused(t, "shared memory above the run's", "POST", "/containers/create", map[string]any{"HostConfig": map[string]any{"ShmSize": 1 << 40}}, "shared memory")
	// Unlimited swap becomes none beyond the memory bound.
	if code, body := g.do(t, "POST", "/containers/create", map[string]any{"Image": "mcp/server", "HostConfig": map[string]any{"MemorySwap": -1}}); code != http.StatusCreated {
		t.Fatalf("create with swap -1: %d %s", code, body)
	}
	hc, _ = g.body("POST /containers/create")["HostConfig"].(map[string]any)
	if hc["MemorySwap"] != float64(2<<30) {
		t.Errorf("MemorySwap forwarded as %v, want the memory bound", hc["MemorySwap"])
	}
}

// A run whose servers need no relays gets no shared network at all.
func TestARunWithoutRelaysHasNoSharedNetwork(t *testing.T) {
	g := newRig(t, Limits{}, func(c *Config) { c.SharedNetworks, c.EgressNetworks = nil, nil })
	g.refused(t, "joining the shared relay network", "POST", "/containers/create", map[string]any{"HostConfig": map[string]any{"NetworkMode": intNet}}, "not this run's")
	g.refused(t, "joining the way out", "POST", "/containers/create", map[string]any{"Image": relayPin, "HostConfig": map[string]any{"NetworkMode": ext}}, "not this run's")
}

func TestOnlyTheOperationsAGatewayNeeds(t *testing.T) {
	g := newRig(t, Limits{})
	for _, c := range []struct{ method, path string }{
		{"POST", "/build"},
		{"POST", "/containers/prune"},
		{"POST", "/containers/runa-1/update"},
		{"POST", "/containers/runa-1/rename"},
		{"GET", "/containers/runa-1/export"},
		{"POST", "/commit"},
		{"POST", "/swarm/init"},
		{"POST", "/plugins/pull"},
		{"DELETE", "/images/alpine"},
		{"POST", "/images/alpine/push"},
		{"POST", "/images/alpine/tag"},
		{"POST", "/images/load"},
		{"POST", "/_ping"},
		{"GET", "/system/df"},
		{"POST", "/images/create?fromSrc=https://example.invalid/x.tar&repo=docker/mcp-l4proxy"},
		{"POST", "/images/create?fromSrc=-&repo=docker/mcp-l4proxy"},
		{"POST", "/images/create"},
	} {
		g.refused(t, c.method+" "+c.path, c.method, c.path, nil, "")
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/_ping"}, {"GET", "/version"}, {"GET", "/info"},
		{"GET", "/images/mcp/server:1/json"}, {"POST", "/images/create?fromImage=mcp/server&tag=1"},
	} {
		if code, body := g.do(t, c.method, c.path, nil); code == http.StatusForbidden {
			t.Errorf("%s %s refused: %s", c.method, c.path, body)
		}
	}
}

func TestEventsAreScopedToTheRun(t *testing.T) {
	g := newRig(t, Limits{})
	req, _ := http.NewRequest(http.MethodGet, "http://docker/events?filters="+`{"type":["container"]}`, nil)
	if _, err := g.f.decide(req, "/events"); err != nil {
		t.Fatal(err)
	}
	var filters map[string][]string
	if err := json.Unmarshal([]byte(req.URL.Query().Get("filters")), &filters); err != nil {
		t.Fatal(err)
	}
	if strings.Join(filters["label"], ",") != own+"=A" || strings.Join(filters["type"], ",") != "container" {
		t.Errorf("filters = %v", filters)
	}
}
