package sockfilter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

const (
	lbl       = "agentry.run"
	own       = "agentry.gateway.run"
	gatewayUA = "gateway-under-test"
	relayPin  = "docker/mcp-l4proxy@sha256:0123"
	l7Pin     = "docker/mcp-l7proxy@sha256:4567"
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
	case l7Pin, "sha256:l7relay":
		return "sha256:l7relay", []string{l7Pin}
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
		case id == "agentry-A-web":
			return "A", true // the runner's web network, for run A
		case id == "agentry-B-web":
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
		if strings.Contains(id, "tools") {
			image, _ = imageFor("agentry-runner:test")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id": id + "-full", "Image": image, "Config": map[string]any{"Labels": labelsFor(id, run)},
			"NetworkSettings": map[string]any{"Networks": map[string]any{"agentry-A-relays": map[string]any{"NetworkID": "id-agentry-A-relays", "IPAddress": "10.9.0.5"}}},
			// A container whose id says so has started, or runs under runsc.
			"State":      map[string]any{"Running": strings.Contains(id, "running")},
			"HostConfig": map[string]any{"Runtime": map[bool]string{true: "runsc"}[strings.Contains(id, "runsc")]},
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
		EgressNetworks: map[string][]string{ext: {relayPin, l7Pin}},
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
		{"an undeclared host", relay(nil, "PROXY_HOSTNAME=evil.example", "PROXY_PORTS=443"), "not one the run declared"},
		{"the wildcard's own apex", relay(nil, "PROXY_HOSTNAME=example.org", "PROXY_PORTS=443"), "not one the run declared"},
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

// The gateway's L7 relay, the tools server's way to the agent's declared
// hosts, takes ALLOWED_HOSTS alone, each host:port one the run declared
// (agentry-cli#346); the two relay forms never mix.
func TestAnL7RelayReachesOnlyTheDeclaredHosts(t *testing.T) {
	g := newRig(t, Limits{}, func(c *Config) { c.RelayHosts = []string{"api.osv.dev", "api.frankfurter.dev:443"} })
	l7 := func(env ...string) map[string]any {
		return map[string]any{"Image": l7Pin, "Env": env, "HostConfig": map[string]any{"NetworkMode": ext}}
	}
	if code, body := g.do(t, "POST", "/containers/create", l7("ALLOWED_HOSTS=api.frankfurter.dev:443")); code != http.StatusCreated {
		t.Fatalf("the L7 relay to the agent's declared host: %d %s", code, body)
	}
	if code, body := g.do(t, "POST", "/containers/create", l7("ALLOWED_HOSTS=api.frankfurter.dev:443,api.osv.dev:443")); code != http.StatusCreated {
		t.Errorf("the L7 relay to two declared hosts: %d %s", code, body)
	}
	g.refused(t, "the L7 form on the L4 image", "POST", "/containers/create",
		map[string]any{"Image": relayPin, "Env": []string{"ALLOWED_HOSTS=api.frankfurter.dev:443"}, "HostConfig": map[string]any{"NetworkMode": ext}}, "not ALLOWED_HOSTS")
	for _, c := range []struct {
		name, reason string
		env          []string
	}{
		{"an undeclared host", "not one the run declared", []string{"ALLOWED_HOSTS=evil.example:443"}},
		{"an undeclared port", "port 22", []string{"ALLOWED_HOSTS=api.frankfurter.dev:22"}},
		{"one bad target among good", "not one the run declared", []string{"ALLOWED_HOSTS=api.frankfurter.dev:443,evil.example:443"}},
		{"both forms", "nothing else", []string{"ALLOWED_HOSTS=api.frankfurter.dev:443", "PROXY_HOSTNAME=api.osv.dev"}},
		{"the L4 form on the L7 image", "nothing else", []string{"PROXY_HOSTNAME=api.frankfurter.dev", "PROXY_PORTS=443"}},
		{"a setting twice", "given twice", []string{"ALLOWED_HOSTS=api.frankfurter.dev:443", "ALLOWED_HOSTS=evil.example:443"}},
		{"no port", "not host:port", []string{"ALLOWED_HOSTS=api.frankfurter.dev"}},
		{"a config line", "not host:port", []string{"ALLOWED_HOSTS=api.frankfurter.dev:443\nacl all"}},
		{"empty", "no ALLOWED_HOSTS", []string{"ALLOWED_HOSTS="}},
	} {
		g.refused(t, c.name, "POST", "/containers/create", l7(c.env...), c.reason)
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
		map[string]any{"Image": relayPin, "Env": []string{"PROXY_HOSTNAME=api.osv.dev", "PROXY_PORTS=443"}, "HostConfig": map[string]any{"NetworkMode": ext}}, "not one the run declared")
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
	if !strings.Contains(body, `"`+intNet+`"`) || strings.Contains(body, `"agentry-A-relays"`) {
		t.Errorf("relay inspect shows %s", body)
	}
	// Another run's relay networks, by their own names.
	g.refused(t, "another run's relay network", "POST", "/containers/create", map[string]any{"HostConfig": map[string]any{"NetworkMode": "agentry-B-relays"}}, "not this run's")
	g.refused(t, "another run's way out", "POST", "/networks/agentry-B-relays-out/connect", map[string]any{"Container": "runa-relay"}, "not this run's")
	// The run's own may not be removed by the gateway.
	g.refused(t, "removing the run's relay network", "DELETE", "/networks/"+intNet, nil, "not the gateway's to remove")
}

// Under the policy's runtime (agentry-cli#221): every container the
// gateway makes but a relay runs under it, the gateway choosing none; a
// sandboxed server cannot reach docker's DNS, so each of its links becomes
// an /etc/hosts entry for the linked relay's address on the network they
// share, in either form docker takes a link; the relay itself stays on
// the daemon's default, since it resolves its declared host by that DNS.
func TestTheGatewaysContainersRunUnderTheRunsRuntime(t *testing.T) {
	relays := func(c *Config) {
		c.SharedNetworks = []string{"agentry-A-relays"}
		c.EgressNetworks = map[string][]string{"agentry-A-relays-out": {relayPin}}
		c.NetworkNames = map[string]string{intNet: "agentry-A-relays", ext: "agentry-A-relays-out"}
	}
	g := newRig(t, Limits{}, relays, func(c *Config) { c.Runtime = "runsc" })
	created := func(name string, doc map[string]any) map[string]any {
		t.Helper()
		if code, body := g.do(t, "POST", "/containers/create", doc); code != http.StatusCreated {
			t.Fatalf("%s: %d %s", name, code, body)
		}
		hc, _ := g.body("POST /containers/create")["HostConfig"].(map[string]any)
		return hc
	}
	hc := created("the tools server", map[string]any{"HostConfig": map[string]any{"NetworkMode": "none"}})
	if hc["Runtime"] != "runsc" || hc["ExtraHosts"] != nil {
		t.Errorf("tools server forwarded with runtime %v, hosts %v; want runsc and none", hc["Runtime"], hc["ExtraHosts"])
	}
	for _, c := range []struct {
		name string
		doc  map[string]any
	}{
		{"a server linked in HostConfig", map[string]any{"HostConfig": map[string]any{"NetworkMode": intNet, "Links": []string{"runa-relay:api.osv.dev"}}}},
		{"a server linked on its endpoint", map[string]any{"HostConfig": map[string]any{"NetworkMode": intNet},
			"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{intNet: map[string]any{"Links": []string{"/runa-relay:/container/api.osv.dev"}}}}}},
		{"a server linked in both, as docker's CLI writes it", map[string]any{"HostConfig": map[string]any{"NetworkMode": intNet, "Links": []string{"runa-relay:api.osv.dev"}},
			"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{intNet: map[string]any{"Links": []string{"runa-relay:api.osv.dev"}}}}}},
	} {
		hc := created(c.name, c.doc)
		hosts, _ := hc["ExtraHosts"].([]any)
		if hc["Runtime"] != "runsc" || len(hosts) != 2 || hosts[0] != "api.osv.dev:10.9.0.5" || hosts[1] != "runa-relay:10.9.0.5" {
			t.Errorf("%s: runtime %v, hosts %v; want runsc, and the alias and the relay's name at its address", c.name, hc["Runtime"], hc["ExtraHosts"])
		}
	}
	hc = created("the relay", map[string]any{"Image": relayPin, "Env": []string{"PROXY_HOSTNAME=api.osv.dev", "PROXY_PORTS=443"}, "HostConfig": map[string]any{"NetworkMode": ext}})
	if hc["Runtime"] != nil || hc["ExtraHosts"] != nil {
		t.Errorf("relay forwarded with runtime %v, hosts %v; want the daemon's default and its DNS", hc["Runtime"], hc["ExtraHosts"])
	}
	g.refused(t, "the gateway naming the run's own runtime", "POST", "/containers/create", map[string]any{"HostConfig": map[string]any{"Runtime": "runsc"}}, "not the gateway's to choose")
	g.refused(t, "a link across networks", "POST", "/containers/create", map[string]any{"HostConfig": map[string]any{"NetworkMode": "runa-net", "Links": []string{"runa-relay:api.osv.dev"}}}, "shares no network")
	g.refused(t, "the gateway naming a runtime in lower case", "POST", "/containers/create", `{"Image":"mcp/server","hostconfig":{"runtime":"runc"}}`, "not the gateway's to choose")
	for _, form := range []struct {
		name string
		doc  func(target string) map[string]any
	}{
		{"in HostConfig", func(target string) map[string]any {
			return map[string]any{"HostConfig": map[string]any{"NetworkMode": intNet, "Links": []string{target + ":api.osv.dev"}}}
		}},
		{"on the endpoint", func(target string) map[string]any {
			return map[string]any{"HostConfig": map[string]any{"NetworkMode": intNet},
				"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{intNet: map[string]any{"Links": []string{target + ":api.osv.dev"}}}}}
		}},
	} {
		g.refused(t, "a link to another run's container "+form.name, "POST", "/containers/create", form.doc("runb-relay"), "not this run's")
		g.refused(t, "a link to the runner's own agent "+form.name, "POST", "/containers/create", form.doc("runa-agent"), "not this run's")
	}
	g.refused(t, "a linked name that is not a host name", "POST", "/containers/create", map[string]any{"HostConfig": map[string]any{"NetworkMode": intNet, "Links": []string{"runa_relay:api.osv.dev"}}}, "not a host name")
	g.refused(t, "a link alias that is not a host name", "POST", "/containers/create", map[string]any{"HostConfig": map[string]any{"NetworkMode": intNet, "Links": []string{"runa-relay:api.osv.dev 10.0.0.1"}}}, "not a host name")

	plain := newRig(t, Limits{}, relays)
	if code, body := plain.do(t, "POST", "/containers/create", map[string]any{"HostConfig": map[string]any{"NetworkMode": intNet, "Links": []string{"runa-relay:api.osv.dev"}}}); code != http.StatusCreated {
		t.Fatalf("a server without a runtime: %d %s", code, body)
	}
	hc, _ = plain.body("POST /containers/create")["HostConfig"].(map[string]any)
	if hc["Runtime"] != nil || hc["ExtraHosts"] != nil {
		t.Errorf("without a runtime the server was given runtime %v, hosts %v", hc["Runtime"], hc["ExtraHosts"])
	}
}

// With the run's own resolv.conf, a relay runs under the run's runtime and
// reads that file in place of docker's; without it, the relay stays on the
// daemon's default as before. Under the runtime, a started container is
// never connected to a network: a sandbox would take no such network
// (agentry-cli#337).
func TestRelaysRunUnderTheRuntimeWithTheHostsResolvers(t *testing.T) {
	relays := func(c *Config) {
		c.SharedNetworks = []string{"agentry-A-relays"}
		c.EgressNetworks = map[string][]string{"agentry-A-relays-out": {relayPin}}
		c.NetworkNames = map[string]string{intNet: "agentry-A-relays", ext: "agentry-A-relays-out"}
	}
	conf := filepath.Join(t.TempDir(), "resolv.conf")
	g := newRig(t, Limits{}, relays, func(c *Config) { c.Runtime, c.RelayResolvConf = "runsc", conf })
	relay := map[string]any{"Image": relayPin, "Env": []string{"PROXY_HOSTNAME=api.osv.dev", "PROXY_PORTS=443"}, "HostConfig": map[string]any{"NetworkMode": ext}}
	if code, body := g.do(t, "POST", "/containers/create", relay); code != http.StatusCreated {
		t.Fatalf("the relay: %d %s", code, body)
	}
	hc, _ := g.body("POST /containers/create")["HostConfig"].(map[string]any)
	binds, _ := hc["Binds"].([]any)
	if hc["Runtime"] != "runsc" || len(binds) != 1 || binds[0] != conf+":/etc/resolv.conf:ro" {
		t.Errorf("relay forwarded with runtime %v, binds %v; want runsc and the run's resolv.conf read-only", hc["Runtime"], hc["Binds"])
	}
	g.refused(t, "the gateway naming a relay's resolv.conf", "POST", "/containers/create",
		map[string]any{"Image": relayPin, "Env": []string{"PROXY_HOSTNAME=api.osv.dev", "PROXY_PORTS=443"}, "HostConfig": map[string]any{"NetworkMode": ext, "Binds": []string{conf + ":/etc/resolv.conf:ro"}}}, "")
	g.refused(t, "a started relay under runsc joining the way out", "POST", "/networks/"+ext+"/connect", map[string]any{"Container": "runa-relay-running-runsc"}, "no network connected after start")
	g.refused(t, "a started server under runsc joining a network", "POST", "/networks/runa-net/connect", map[string]any{"Container": "runa-running-runsc"}, "no network connected after start")
	if code, body := g.do(t, "POST", "/networks/"+ext+"/connect", map[string]any{"Container": "runa-relay-runsc"}); code == http.StatusForbidden {
		t.Errorf("a relay joining before it starts was refused: %s", body)
	}
	// A started container on the daemon's default takes a join, whatever
	// the run's runtime: only a sandbox would miss it.
	if code, body := g.do(t, "POST", "/networks/runa-net/connect", map[string]any{"Container": "runa-running"}); code == http.StatusForbidden {
		t.Errorf("a started container on the default was refused a join: %s", body)
	}

	plain := newRig(t, Limits{}, relays, func(c *Config) { c.Runtime = "runsc" })
	if code, body := plain.do(t, "POST", "/containers/create", relay); code != http.StatusCreated {
		t.Fatalf("the relay without resolvers: %d %s", code, body)
	}
	hc, _ = plain.body("POST /containers/create")["HostConfig"].(map[string]any)
	if hc["Runtime"] != nil || hc["Binds"] != nil {
		t.Errorf("without resolvers the relay was given runtime %v, binds %v", hc["Runtime"], hc["Binds"])
	}
	// A relay on the daemon's default takes a join at any time, as before.
	if code, body := plain.do(t, "POST", "/networks/"+ext+"/connect", map[string]any{"Container": "runa-relay-running"}); code == http.StatusForbidden {
		t.Errorf("a started relay on the default was refused a join: %s", body)
	}
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

// TestEveryGatewayContainerDropsItsCapabilities (agentry-cli#227, DR-95):
// whatever the gateway asks, each container it creates drops every
// capability and gets back only its class's: a relay none, the run's tools
// server its toolsrv set, a declared server read past the store's mode, and
// write too only where it is given a writable host folder.
func TestEveryGatewayContainerDropsItsCapabilities(t *testing.T) {
	const spec = "/etc/agentry/toolspec.json"
	g := newRig(t, Limits{}, func(c *Config) {
		c.ToolsImage, c.ToolsCmd = "agentry-runner:test", []string{"tools", "--spec", spec}
	})
	capsOf := func(name string, doc map[string]any) (drop, add []string) {
		t.Helper()
		if code, body := g.do(t, "POST", "/containers/create", doc); code != http.StatusCreated {
			t.Fatalf("%s: %d %s", name, code, body)
		}
		hc, _ := g.body("POST /containers/create")["HostConfig"].(map[string]any)
		return asList(hc["CapDrop"]), asList(hc["CapAdd"])
	}
	store := filepath.Join(g.root, "servers")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		doc  map[string]any
		add  []string
	}{
		{"the tools server", map[string]any{"Image": "agentry-runner:test", "Cmd": []string{"tools", "--spec", spec}}, toolsCapabilities},
		{"the tools server asking to keep nothing dropped", map[string]any{"Image": "agentry-runner:test", "Cmd": []string{"tools", "--spec", spec},
			"HostConfig": map[string]any{"CapDrop": []string{"NET_RAW"}}}, toolsCapabilities},
		{"a relay", map[string]any{"Image": relayPin, "Env": []string{"PROXY_HOSTNAME=api.osv.dev", "PROXY_PORTS=443"}, "HostConfig": map[string]any{"NetworkMode": ext}}, nil},
		{"a declared server reading its store", map[string]any{"Image": "mcp/server", "HostConfig": map[string]any{"Binds": []string{store + ":/srv:ro"}}}, serverReadCapabilities},
		{"a declared server with no bind", map[string]any{"Image": "mcp/server"}, serverReadCapabilities},
		{"a declared server in the tools server's image, another command", map[string]any{"Image": "agentry-runner:test", "Cmd": []string{"node", "server.js"}}, serverReadCapabilities},
		{"a declared server writing its token folder", map[string]any{"Image": "mcp/server", "HostConfig": map[string]any{"Binds": []string{store + ":/srv:ro", g.writable + ":/tokens"}}},
			append(slices.Clone(serverReadCapabilities), serverWriteCapabilities...)},
		{"a declared server writing through a bind mount", map[string]any{"Image": "mcp/server", "HostConfig": map[string]any{"Mounts": []map[string]any{{"Type": "bind", "Source": g.writable, "Target": "/tokens"}}}},
			append(slices.Clone(serverReadCapabilities), serverWriteCapabilities...)},
	} {
		drop, add := capsOf(c.name, c.doc)
		if !slices.Equal(drop, []string{"ALL"}) || !slices.Equal(add, c.add) {
			t.Errorf("%s: CapDrop %v CapAdd %v, want [ALL] and %v", c.name, drop, add, c.add)
		}
	}
	// What a class gets back is the filter's to say, never the gateway's.
	g.refused(t, "a capability the gateway adds", "POST", "/containers/create", map[string]any{"Image": "agentry-runner:test", "Cmd": []string{"tools", "--spec", spec},
		"HostConfig": map[string]any{"CapAdd": []string{"CHOWN"}}}, "CapAdd")
}

func asList(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, x := range l {
		out = append(out, fmt.Sprint(x))
	}
	return out
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

// The built-in Drafter's web tools (agentry-cli#345): their server asks for
// the daemon's default network, which the filter gives no container; it
// joins the run's own web network instead, in its one shape, and nothing
// else does.
func TestTheWebNetworkTakesOnlyTheWebToolsServer(t *testing.T) {
	const target = "/etc/agentry/toolspec.json"
	var specPath string
	web := func(c *Config) {
		specPath = filepath.Join(c.BindRoots[0], "cfg", "toolspec.json")
		if err := os.MkdirAll(filepath.Dir(specPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(specPath, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		other := filepath.Join(c.BindRoots[0], "out", "evil.json")
		if err := os.WriteFile(other, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		c.WebNetwork, c.WebImage = "agentry-A-web", "agentry-runner:test"
		c.WebCmd, c.WebSpec, c.WebSpecTarget = []string{"tools", "--spec", target}, specPath, target
	}
	g := newRig(t, Limits{}, web)
	server := func(edit func(doc map[string]any)) map[string]any {
		doc := map[string]any{
			"Image": "agentry-runner:test", "Cmd": []string{"tools", "--spec", target},
			"HostConfig":       map[string]any{"NetworkMode": "default", "Binds": []string{specPath + ":" + target + ":ro"}},
			"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{"default": map[string]any{}}},
		}
		if edit != nil {
			edit(doc)
		}
		return doc
	}
	for name, doc := range map[string]map[string]any{
		"by the default network": server(nil),
		"asking for no network": server(func(d map[string]any) {
			d["HostConfig"] = map[string]any{"Binds": []string{specPath + ":" + target + ":ro"}}
			delete(d, "NetworkingConfig")
		}),
	} {
		if code, body := g.do(t, "POST", "/containers/create", doc); code != http.StatusCreated {
			t.Fatalf("the web tools' server %s: %d %s", name, code, body)
		}
		sent := g.body("POST /containers/create")
		hc, _ := sent["HostConfig"].(map[string]any)
		if hc["NetworkMode"] != "id-agentry-A-web" {
			t.Fatalf("%s: forwarded on %v, want the run's web network", name, hc["NetworkMode"])
		}
		if nc, ok := sent["NetworkingConfig"].(map[string]any); ok {
			eps, _ := nc["EndpointsConfig"].(map[string]any)
			if len(eps) != 1 || eps["id-agentry-A-web"] == nil {
				t.Fatalf("%s: endpoints %v", name, eps)
			}
		}
	}
	for name, doc := range map[string]map[string]any{
		"another entrypoint": server(func(d map[string]any) { d["Entrypoint"] = []string{"/bin/sh"} }),
		"another command":    server(func(d map[string]any) { d["Cmd"] = []string{"sh", "-c", "curl 169.254.169.254"} }),
		"another spec":       server(func(d map[string]any) { d["Cmd"] = []string{"tools", "--spec", "/out/evil.json"} }),
		"a second network": server(func(d map[string]any) {
			d["NetworkingConfig"] = map[string]any{"EndpointsConfig": map[string]any{"default": map[string]any{}, "runa-net": map[string]any{}}}
		}),
		"the spec writable": server(func(d map[string]any) {
			d["HostConfig"].(map[string]any)["Binds"] = []string{specPath + ":" + target + ":rw"}
		}),
		"another file as the spec": server(func(d map[string]any) {
			d["HostConfig"].(map[string]any)["Binds"] = []string{filepath.Join(filepath.Dir(filepath.Dir(specPath)), "out", "evil.json") + ":" + target + ":ro"}
		}),
		"no spec":  server(func(d map[string]any) { d["HostConfig"].(map[string]any)["Binds"] = []string{} }),
		"a link":   server(func(d map[string]any) { d["HostConfig"].(map[string]any)["Links"] = []string{"runa-relay:api"} }),
		"its user": server(func(d map[string]any) { d["User"] = "0" }),
	} {
		// A spec bound writable, or from inside a directory the run writes,
		// is already refused by the storage rules; the rest by the shape.
		storage := name == "the spec writable" || name == "another file as the spec"
		if code, body := g.do(t, "POST", "/containers/create", doc); code != http.StatusForbidden || (!storage && !strings.Contains(body, "web tools' server")) {
			t.Errorf("%s: %d %s, want the web tools' server's shape refused", name, code, body)
		}
	}
	if code, body := g.do(t, "POST", "/containers/create", map[string]any{"Image": "mcp/osv",
		"HostConfig":       map[string]any{"NetworkMode": "default"},
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{"default": map[string]any{}}}}); code == http.StatusCreated {
		t.Fatalf("another image on the default network: %d %s", code, body)
	}
	g.refused(t, "another image on the web network", "POST", "/containers/create", map[string]any{"Image": "mcp/osv",
		"HostConfig": map[string]any{"NetworkMode": "agentry-A-web"}}, "web tools' server only")
	g.refused(t, "another image on the web network, by endpoint", "POST", "/containers/create", map[string]any{"Image": "mcp/osv",
		"HostConfig":       map[string]any{"NetworkMode": "none"},
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{"agentry-A-web": map[string]any{}}}}, "web tools' server only")
	g.refused(t, "the web tools' server on the bridge", "POST", "/containers/create", map[string]any{"Image": "agentry-runner:test",
		"HostConfig": map[string]any{"NetworkMode": "bridge"}}, "default bridge")
	g.refused(t, "another run's web network", "POST", "/containers/create", map[string]any{"Image": "agentry-runner:test",
		"HostConfig": map[string]any{"NetworkMode": "agentry-B-web"}}, "not this run's")
	g.refused(t, "connecting another container to the web network", "POST", "/networks/agentry-A-web/connect", map[string]any{"Container": "runa-1"}, "web tools' server only")
	g.refused(t, "removing the run's web network", "DELETE", "/networks/agentry-A-web", nil, "not the gateway's to remove")
	g.refused(t, "exec into the web tools' server", "POST", "/containers/runa-tools/exec", map[string]any{"Cmd": []string{"sh"}}, "web tools' server")
	g.refused(t, "writing into the web tools' server", "PUT", "/containers/runa-tools/archive?path=/", "tar", "web tools' server")
	if code, body := g.do(t, "POST", "/containers/runa-tools/attach?stream=1&stdin=1&stdout=1", nil); code == http.StatusForbidden {
		t.Errorf("attaching to the web tools' server (its stdio) refused: %s", body)
	}

	// A run without the web tools gives the image no network either.
	plain := newRig(t, Limits{})
	if code, body := plain.do(t, "POST", "/containers/create", map[string]any{"Image": "agentry-runner:test",
		"HostConfig":       map[string]any{"NetworkMode": "default"},
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{"default": map[string]any{}}}}); code == http.StatusCreated {
		t.Fatalf("the runner image without the web tools: %d %s", code, body)
	}
}

// TestTheWebToolsServerResolvesThroughTheRunsResolvConf (agentry-cli#386):
// with the run's resolv.conf configured, the filter binds it read-only at
// /etc/resolv.conf in the web tools' server, and in no other container; the
// gateway may not bind a resolv.conf of its own there, nor set DNS servers,
// options or hosts entries for it. Without one, the server keeps docker's.
func TestTheWebToolsServerResolvesThroughTheRunsResolvConf(t *testing.T) {
	const target = "/etc/agentry/toolspec.json"
	var specPath, resolv string
	web := func(withResolvers bool) func(c *Config) {
		return func(c *Config) {
			specPath = filepath.Join(c.BindRoots[0], "cfg", "toolspec.json")
			resolv = filepath.Join(c.BindRoots[0], "cfg", "resolv.conf")
			if err := os.MkdirAll(filepath.Dir(specPath), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{specPath, resolv} {
				if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			c.WebNetwork, c.WebImage = "agentry-A-web", "agentry-runner:test"
			c.WebCmd, c.WebSpec, c.WebSpecTarget = []string{"tools", "--spec", target}, specPath, target
			if withResolvers {
				c.WebResolvConf = resolv
			}
		}
	}
	server := func(edit func(hc map[string]any)) map[string]any {
		hc := map[string]any{"NetworkMode": "default", "Binds": []string{specPath + ":" + target + ":ro"}}
		if edit != nil {
			edit(hc)
		}
		return map[string]any{"Image": "agentry-runner:test", "Cmd": []string{"tools", "--spec", target}, "HostConfig": hc}
	}
	binds := func(g *rig) []string {
		hc, _ := g.body("POST /containers/create")["HostConfig"].(map[string]any)
		var out []string
		list, _ := hc["Binds"].([]any)
		for _, b := range list {
			out = append(out, b.(string))
		}
		return out
	}

	g := newRig(t, Limits{}, web(true))
	if code, body := g.do(t, "POST", "/containers/create", server(nil)); code != http.StatusCreated {
		t.Fatalf("the web tools' server: %d %s", code, body)
	}
	if got := binds(g); !slices.Contains(got, resolv+":/etc/resolv.conf:ro") {
		t.Fatalf("the run's resolv.conf is not bound: %v", got)
	}
	for name, edit := range map[string]func(hc map[string]any){
		"its own resolv.conf": func(hc map[string]any) {
			hc["Binds"] = []string{specPath + ":" + target + ":ro", specPath + ":/etc/resolv.conf:ro"}
		},
		"a resolv.conf path not clean": func(hc map[string]any) {
			hc["Binds"] = []string{specPath + ":" + target + ":ro", specPath + ":/etc//resolv.conf:ro"}
		},
		"DNS servers":   func(hc map[string]any) { hc["Dns"] = []string{"203.0.113.9"} },
		"DNS options":   func(hc map[string]any) { hc["DnsOptions"] = []string{"ndots:0"} },
		"a DNS search":  func(hc map[string]any) { hc["DnsSearch"] = []string{"evil.example"} },
		"a hosts entry": func(hc map[string]any) { hc["ExtraHosts"] = []string{"vuln.go.dev:203.0.113.9"} },
		"a resolv.conf mount": func(hc map[string]any) {
			hc["Mounts"] = []map[string]any{{"Type": "bind", "Source": specPath, "Target": "/etc/resolv.conf", "ReadOnly": true}}
		},
		"a bind over /etc": func(hc map[string]any) {
			hc["Binds"] = []string{specPath + ":" + target + ":ro", filepath.Dir(specPath) + ":/etc:ro"}
		},
		"a tmpfs at the resolv.conf": func(hc map[string]any) { hc["Tmpfs"] = map[string]string{"/etc/resolv.conf": ""} },
		"a tmpfs over /etc":          func(hc map[string]any) { hc["Tmpfs"] = map[string]string{"/etc": ""} },
		"volumes from another":       func(hc map[string]any) { hc["VolumesFrom"] = []string{"runa-other"} },
		"a hosts file": func(hc map[string]any) {
			hc["Binds"] = []string{specPath + ":" + target + ":ro", specPath + ":/etc/hosts:ro"}
		},
		"a hosts mount": func(hc map[string]any) {
			hc["Mounts"] = []map[string]any{{"Type": "bind", "Source": specPath, "Target": "/etc/hosts", "ReadOnly": true}}
		},
		"a tmpfs mount over /etc": func(hc map[string]any) {
			hc["Mounts"] = []map[string]any{{"Type": "tmpfs", "Target": "/etc"}}
		},
		"a volume mount at the hosts file": func(hc map[string]any) {
			hc["Mounts"] = []map[string]any{{"Type": "volume", "Source": "runa-vol", "Target": "/etc/hosts"}}
		},
	} {
		if code, body := g.do(t, "POST", "/containers/create", server(edit)); code != http.StatusForbidden {
			t.Errorf("the web tools' server with %s: %d %s, want it refused", name, code, body)
		}
	}
	// The gateway declares its binds' targets in Volumes, as docker's
	// clients do: that passes, and a declared volume over /etc does not.
	withVolumes := func(v map[string]any) map[string]any {
		d := server(nil)
		d["Volumes"] = v
		return d
	}
	if code, body := g.do(t, "POST", "/containers/create", withVolumes(map[string]any{target: map[string]any{}})); code != http.StatusCreated {
		t.Fatalf("the web tools' server declaring its spec's target: %d %s", code, body)
	}
	if !slices.Contains(binds(g), resolv+":/etc/resolv.conf:ro") {
		t.Fatalf("the run's resolv.conf is not bound: %v", binds(g))
	}
	for _, v := range []string{"/etc", "/etc/resolv.conf", "/etc/hosts"} {
		if code, body := g.do(t, "POST", "/containers/create", withVolumes(map[string]any{v: map[string]any{}})); code != http.StatusForbidden {
			t.Errorf("a declared volume at %s: %d %s, want it refused", v, code, body)
		}
	}
	// Another container on a network of the run's gets no resolv.conf.
	if code, body := g.do(t, "POST", "/containers/create", map[string]any{"Image": "mcp/osv",
		"HostConfig": map[string]any{"NetworkMode": "runa-net"}}); code == http.StatusCreated {
		for _, b := range binds(g) {
			if strings.Contains(b, "resolv.conf") {
				t.Fatalf("another container got the run's resolv.conf: %v", b)
			}
		}
	} else if code != http.StatusForbidden {
		t.Fatalf("another container: %d %s", code, body)
	}

	// Without resolvers the server keeps docker's DNS: nothing is bound.
	g = newRig(t, Limits{}, web(false))
	if code, body := g.do(t, "POST", "/containers/create", server(nil)); code != http.StatusCreated {
		t.Fatalf("the web tools' server without resolvers: %d %s", code, body)
	}
	for _, b := range binds(g) {
		if strings.Contains(b, "resolv.conf") {
			t.Fatalf("a run without resolvers bound %v", b)
		}
	}
}

// TestTheToolsServerGetsTheRunsBoundedTmpfs (agentry-cli#599, #729): the
// filter mounts ToolsTmpfs into the tools server, the container of
// ToolsImage running ToolsCmd, and into nothing else; a request that puts
// anything of the gateway's at or beneath those targets is refused.
func TestTheToolsServerGetsTheRunsBoundedTmpfs(t *testing.T) {
	const target = "/etc/agentry/toolspec.json"
	opts := "size=268435456,mode=1777"
	g := newRig(t, Limits{}, func(c *Config) {
		c.ToolsImage, c.ToolsCmd = "agentry-runner:test", []string{"tools", "--spec", target}
		c.ToolsTmpfs = map[string]string{"/tmp": opts, "/scratch": opts}
	})
	tools := func(edit func(doc map[string]any)) map[string]any {
		doc := map[string]any{"Image": "agentry-runner:test", "Cmd": []string{"tools", "--spec", target}, "HostConfig": map[string]any{}}
		if edit != nil {
			edit(doc)
		}
		return doc
	}
	tmpfsOf := func() map[string]any {
		hc, _ := g.body("POST /containers/create")["HostConfig"].(map[string]any)
		m, _ := hc["Tmpfs"].(map[string]any)
		return m
	}
	if code, body := g.do(t, "POST", "/containers/create", tools(nil)); code != http.StatusCreated {
		t.Fatalf("the tools server: %d %s", code, body)
	}
	if m := tmpfsOf(); m["/tmp"] != opts || m["/scratch"] != opts || len(m) != 2 {
		t.Fatalf("the tools server was created with tmpfs %v", m)
	}
	for name, doc := range map[string]map[string]any{
		"another image":   tools(func(d map[string]any) { d["Image"] = "node:22" }),
		"another command": tools(func(d map[string]any) { d["Cmd"] = []string{"node", "server.js"} }),
		"no command":      tools(func(d map[string]any) { delete(d, "Cmd") }),
	} {
		if code, body := g.do(t, "POST", "/containers/create", doc); code != http.StatusCreated {
			t.Fatalf("%s: %d %s", name, code, body)
		}
		if m := tmpfsOf(); len(m) != 0 {
			t.Fatalf("%s was given the tools server's tmpfs %v", name, m)
		}
	}
	for name, doc := range map[string]map[string]any{
		"a tmpfs of its own at /tmp": tools(func(d map[string]any) {
			d["HostConfig"] = map[string]any{"Tmpfs": map[string]any{"/tmp": "size=100g"}}
		}),
		"a volume beneath /scratch": tools(func(d map[string]any) {
			d["Volumes"] = map[string]any{"/scratch/x": map[string]any{}}
		}),
		"a tmpfs mount at /scratch": tools(func(d map[string]any) {
			d["HostConfig"] = map[string]any{"Mounts": []any{map[string]any{"Type": "tmpfs", "Target": "/scratch/"}}}
		}),
	} {
		g.refused(t, name, "POST", "/containers/create", doc, "the run's own tmpfs")
	}
}

// TestTheToolsServerTakesTheHostLinksCopyFolder (agentry-cli#861): the one
// bind the tools server may have beneath its scratch tmpfs is the host
// link's copy folder at /scratch/hosts, the run's own; another folder
// there, or that folder anywhere else beneath the tmpfs, is refused.
func TestTheToolsServerTakesTheHostLinksCopyFolder(t *testing.T) {
	const target = "/etc/agentry/toolspec.json"
	opts := "size=268435456,mode=1777"
	var shared string
	g := newRig(t, Limits{}, func(c *Config) {
		shared = filepath.Join(c.BindRoots[0], "hostfiles")
		if err := os.MkdirAll(shared, 0o700); err != nil {
			t.Fatal(err)
		}
		c.WritableRoots = append(c.WritableRoots, shared)
		c.ToolsImage, c.ToolsCmd = "agentry-runner:test", []string{"tools", "--spec", target}
		c.ToolsTmpfs = map[string]string{"/tmp": opts, "/scratch": opts}
		c.ToolsShared = map[string]string{"/scratch/hosts": shared}
	})
	tools := func(binds ...string) map[string]any {
		return map[string]any{"Image": "agentry-runner:test", "Cmd": []string{"tools", "--spec", target},
			"HostConfig": map[string]any{"Binds": binds}}
	}
	binds := func(g *rig) []string {
		hc, _ := g.body("POST /containers/create")["HostConfig"].(map[string]any)
		var out []string
		list, _ := hc["Binds"].([]any)
		for _, b := range list {
			out = append(out, b.(string))
		}
		return out
	}
	if code, body := g.do(t, "POST", "/containers/create", tools(shared+":/scratch/hosts:rw")); code != http.StatusCreated {
		t.Fatalf("the tools server with the copy folder: %d %s", code, body)
	}
	if !slices.Contains(binds(g), shared+":/scratch/hosts:rw") {
		t.Fatalf("binds %v", binds(g))
	}
	for name, doc := range map[string]map[string]any{
		"another folder at /scratch/hosts": tools(g.writable + ":/scratch/hosts:rw"),
		"the copy folder elsewhere":        tools(shared + ":/scratch/other:rw"),
		"the copy folder over /scratch":    tools(shared + ":/scratch:rw"),
	} {
		g.refused(t, name, "POST", "/containers/create", doc, "the run's own tmpfs")
	}
}

// TestATargetHostsRelayReachesItsOneAddressAndPort is invariant 3 of plan
// runtime-hosts.md (agentry-cli#861): a target host's relay is admitted to
// the address the person's machine resolved and the port they named, and a
// relay to any other address or port is refused.
func TestATargetHostsRelayReachesItsOneAddressAndPort(t *testing.T) {
	g := newRig(t, Limits{}, func(c *Config) { c.RelayHosts = []string{"203.0.113.4:2222"} })
	relay := func(env ...string) map[string]any {
		return map[string]any{"Image": relayPin, "Env": env, "HostConfig": map[string]any{"NetworkMode": ext}}
	}
	if code, body := g.do(t, "POST", "/containers/create", relay("PROXY_HOSTNAME=203.0.113.4", "PROXY_PORTS=2222")); code != http.StatusCreated {
		t.Fatalf("the target host's relay: %d %s", code, body)
	}
	for name, doc := range map[string]map[string]any{
		"another port":       relay("PROXY_HOSTNAME=203.0.113.4", "PROXY_PORTS=22"),
		"a second port":      relay("PROXY_HOSTNAME=203.0.113.4", "PROXY_PORTS=2222,22"),
		"another address":    relay("PROXY_HOSTNAME=203.0.113.5", "PROXY_PORTS=2222"),
		"the address on 443": relay("PROXY_HOSTNAME=203.0.113.4", "PROXY_PORTS=443"),
	} {
		g.refused(t, name, "POST", "/containers/create", doc, "not one the run declared")
	}
}

// A container inherits its image's labels: the image label (agentry-cli#892)
// makes nothing the run's, even carrying the run's own id as its value.
// imageLabelKey is imagelabel.Key, spelled here because the runner image's
// build context is stdlib-only (agentry-cli#322); pkg/runner/engine's
// TestTheFiltersImageLabelIsTheRunners holds the two equal.
const imageLabelKey = "org.agentry.image"

func TestTheImageLabelMakesNothingOurs(t *testing.T) {
	f := &Filter{cfg: Config{RunID: "abcd1234", Label: lbl, OwnerLabel: own}}
	labels := map[string]string{imageLabelKey: "abcd1234", lbl: "abcd1234"}
	if f.ours(labels) {
		t.Error("ours: a container with the image label and the run label is the gateway's")
	}
	anyLabels := map[string]any{imageLabelKey: "abcd1234", lbl: "abcd1234"}
	if f.oursAny(anyLabels) {
		t.Error("oursAny: a container with the image label and the run label is the gateway's")
	}
	if !f.ours(map[string]string{own: "abcd1234"}) {
		t.Error("ours: the owner label no longer makes a container the gateway's")
	}
}
