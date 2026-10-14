package sockfilter

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The browser class (agentry-notes plans/browser-testing.md §4, §7, M1;
// agentry-cli#915): the run's browser server keeps no capability, runs on
// a read-only root with a tmpfs home and /tmp owned by the run's user, as
// that user, and under runc with the seccomp profile that lets Chromium's
// own sandbox run; under a sandboxed runtime with Docker's default.

const browserImageRef = "ghcr.io/blaktron/agentry-browser:test"

func browserRig(t *testing.T, runtime string) *rig {
	t.Helper()
	return newRig(t, Limits{}, func(c *Config) {
		c.BrowserImage = browserImageRef
		c.BrowserUser = "1000:1000"
		c.BrowserTmpfs = map[string]string{"/home/browser": "rw,nosuid,nodev,size=268435456,mode=0700", "/tmp": "rw,nosuid,nodev,size=1073741824,mode=1777"}
		c.Runtime = runtime
	})
}

func browserCreate(user string, hc map[string]any) map[string]any {
	if hc == nil {
		hc = map[string]any{}
	}
	return map[string]any{"Image": browserImageRef, "User": user, "Cmd": []string{"--headless"}, "HostConfig": hc}
}

func createdHostConfig(t *testing.T, g *rig) map[string]any {
	t.Helper()
	hc, _ := g.body("POST /containers/create")["HostConfig"].(map[string]any)
	if hc == nil {
		t.Fatal("no create reached the daemon")
	}
	return hc
}

func TestTheBrowserServerIsHeldToItsClass(t *testing.T) {
	g := browserRig(t, "")
	if code, body := g.do(t, "POST", "/containers/create", browserCreate("1000:1000", nil)); code != http.StatusCreated {
		t.Fatalf("the browser server: %d %s", code, body)
	}
	hc := createdHostConfig(t, g)
	if add, _ := hc["CapAdd"].([]any); len(add) != 0 {
		t.Fatalf("CapAdd %v: the browser server keeps no capability", add)
	}
	if drop, _ := hc["CapDrop"].([]any); len(drop) != 1 || drop[0] != "ALL" {
		t.Fatalf("CapDrop %v", drop)
	}
	if hc["ReadonlyRootfs"] != true {
		t.Fatalf("ReadonlyRootfs %v", hc["ReadonlyRootfs"])
	}
	tmpfs, _ := hc["Tmpfs"].(map[string]any)
	if home, _ := tmpfs["/home/browser"].(string); !strings.Contains(home, "uid=1000,gid=1000") || !strings.Contains(home, "mode=0700") {
		t.Fatalf("home tmpfs %q", home)
	}
	if tmp, _ := tmpfs["/tmp"].(string); !strings.Contains(tmp, "size=1073741824") {
		t.Fatalf("/tmp tmpfs %q", tmp)
	}
	var seccomp string
	opts, _ := hc["SecurityOpt"].([]any)
	for _, o := range opts {
		if s, _ := o.(string); strings.HasPrefix(s, "seccomp=") {
			seccomp = strings.TrimPrefix(s, "seccomp=")
		}
	}
	var prof struct {
		DefaultAction string `json:"defaultAction"`
		Syscalls      []struct {
			Names    []string `json:"names"`
			Action   string   `json:"action"`
			Args     []any    `json:"args"`
			Includes any      `json:"includes"`
		} `json:"syscalls"`
	}
	if err := json.Unmarshal([]byte(seccomp), &prof); err != nil {
		t.Fatalf("seccomp profile %q: %v", seccomp, err)
	}
	last := prof.Syscalls[len(prof.Syscalls)-1]
	if prof.DefaultAction != "SCMP_ACT_ERRNO" || !slices.Equal(last.Names, []string{"clone", "unshare", "setns", "chroot"}) || last.Action != "SCMP_ACT_ALLOW" || len(last.Args) != 0 {
		t.Fatalf("the profile is not moby's default and Chromium's four calls: %s %+v", prof.DefaultAction, last)
	}
	for _, r := range prof.Syscalls[:len(prof.Syscalls)-1] {
		// Moby's default allows the four only to a process holding
		// CAP_SYS_ADMIN (or CAP_SYS_CHROOT), which the browser never has.
		if r.Action == "SCMP_ACT_ALLOW" && len(r.Args) == 0 && r.Includes == nil && slices.Contains(r.Names, "unshare") {
			t.Fatal("moby's default already allows unshare unconditionally")
		}
	}
}

func TestTheBrowserServerUnderARuntimeTakesDockersDefaultProfile(t *testing.T) {
	g := browserRig(t, "runsc")
	if code, body := g.do(t, "POST", "/containers/create", browserCreate("1000:1000", nil)); code != http.StatusCreated {
		t.Fatalf("the browser server: %d %s", code, body)
	}
	hc := createdHostConfig(t, g)
	opts, _ := hc["SecurityOpt"].([]any)
	for _, o := range opts {
		if strings.HasPrefix(o.(string), "seccomp=") {
			t.Fatalf("a seccomp profile under runsc: %v", hc["SecurityOpt"])
		}
	}
	if hc["Runtime"] != "runsc" || hc["ReadonlyRootfs"] != true {
		t.Fatalf("runtime %v, read-only %v", hc["Runtime"], hc["ReadonlyRootfs"])
	}
}

func TestTheBrowserServerRefusesWhatItsClassDoesNot(t *testing.T) {
	g := browserRig(t, "")
	for name, doc := range map[string]map[string]any{
		"another user":       browserCreate("0:0", nil),
		"the image's user":   browserCreate("10001:10001", nil),
		"a mount at home":    browserCreate("1000:1000", map[string]any{"Tmpfs": map[string]any{"/home/browser": ""}}),
		"a mount under /tmp": browserCreate("1000:1000", map[string]any{"Tmpfs": map[string]any{"/tmp/x": ""}}),
	} {
		g.refused(t, name, "POST", "/containers/create", doc, "browser server")
	}
	// A seccomp option of the gateway's own is refused before the class
	// adds the run's (only no-new-privileges strengthens the default).
	g.refused(t, "unconfined", "POST", "/containers/create",
		browserCreate("1000:1000", map[string]any{"SecurityOpt": []string{"seccomp=unconfined"}}), "SecurityOpt")
	// Where the machine has no uid to give it, the image's own user.
	g2 := newRig(t, Limits{}, func(c *Config) { c.BrowserImage = browserImageRef })
	if code, body := g2.do(t, "POST", "/containers/create", browserCreate("10001:10001", nil)); code != http.StatusCreated {
		t.Fatalf("the image's user with no run user: %d %s", code, body)
	}
}

// TestAnotherImageIsNotTheBrowser: the class is the browser image's, by
// id; a declared server keeps its own class.
func TestAnotherImageIsNotTheBrowser(t *testing.T) {
	g := browserRig(t, "")
	doc := map[string]any{"Image": "node:24", "Cmd": []string{"server"}, "HostConfig": map[string]any{}}
	if code, body := g.do(t, "POST", "/containers/create", doc); code != http.StatusCreated {
		t.Fatalf("a declared server: %d %s", code, body)
	}
	hc := createdHostConfig(t, g)
	if hc["ReadonlyRootfs"] == true || hc["Tmpfs"] != nil {
		t.Fatalf("a declared server took the browser's class: %v", hc)
	}
}

// TestTheBrowseProxysRelayResolvesThroughItsPin: a relay pointed at the
// browse proxy gets the proxy's address in its hosts file, the run's own
// entry, and a relay to anything else gets none.
func TestTheBrowseProxysRelayResolvesThroughItsPin(t *testing.T) {
	g := newRig(t, Limits{}, func(c *Config) {
		c.RelayHosts = []string{"agentry-ab12-browse-proxy:3128", "api.example.com:443"}
		c.RelayPins = map[string]string{"agentry-ab12-browse-proxy": "172.30.0.5"}
	})
	relay := func(host, ports string) map[string]any {
		return map[string]any{"Image": relayPin, "Env": []string{"PROXY_HOSTNAME=" + host, "PROXY_PORTS=" + ports}, "HostConfig": map[string]any{"NetworkMode": ext}}
	}
	if code, body := g.do(t, "POST", "/containers/create", relay("agentry-ab12-browse-proxy", "3128")); code != http.StatusCreated {
		t.Fatalf("the browse proxy's relay: %d %s", code, body)
	}
	if hosts, _ := createdHostConfig(t, g)["ExtraHosts"].([]any); len(hosts) != 1 || hosts[0] != "agentry-ab12-browse-proxy:172.30.0.5" {
		t.Fatalf("ExtraHosts %v", hosts)
	}
	if code, body := g.do(t, "POST", "/containers/create", relay("api.example.com", "443")); code != http.StatusCreated {
		t.Fatalf("another relay: %d %s", code, body)
	}
	if hosts := createdHostConfig(t, g)["ExtraHosts"]; hosts != nil {
		t.Fatalf("another relay got a pin: %v", hosts)
	}
	// The gateway still may not name one itself.
	doc := relay("agentry-ab12-browse-proxy", "3128")
	doc["HostConfig"].(map[string]any)["ExtraHosts"] = []string{"agentry-ab12-browse-proxy:203.0.113.9"}
	g.refused(t, "a relay's own hosts entry", "POST", "/containers/create", doc, "resolves its host as the daemon does")
}

// TestTheBrowsersNetworkIsItsAndItsRelaysAlone is invariant 2 (plan
// browser-testing.md): the browser server and the relay to the browse
// proxy are given the run's browser network where the gateway names the
// internal relay network; no other container joins it, the browser joins
// no other, and nothing is connected to either afterwards.
func TestTheBrowsersNetworkIsItsAndItsRelaysAlone(t *testing.T) {
	const browserNet = "agentry-A-relays-browser"
	g := newRig(t, Limits{}, func(c *Config) {
		c.SharedNetworks = []string{"agentry-A-relays"}
		c.EgressNetworks = map[string][]string{"agentry-A-relays-out": {relayPin}}
		c.NetworkNames = map[string]string{intNet: "agentry-A-relays", ext: "agentry-A-relays-out"}
		c.BrowserImage = browserImageRef
		c.BrowserUser = "1000:1000"
		c.BrowserTmpfs = map[string]string{"/home/browser": "size=1", "/tmp": "size=1"}
		c.BrowserNetwork = browserNet
		c.BrowserNetworkFor = "agentry-A-relays"
		c.BrowseProxy = "agentry-ab12-browse-proxy"
		c.RelayHosts = []string{"agentry-ab12-browse-proxy:3128", "api.example.com:443"}
	})
	networks := func() (string, []string) {
		doc := g.body("POST /containers/create")
		hc, _ := doc["HostConfig"].(map[string]any)
		mode, _ := hc["NetworkMode"].(string)
		var eps []string
		if nc, ok := doc["NetworkingConfig"].(map[string]any); ok {
			for id := range nc["EndpointsConfig"].(map[string]any) {
				eps = append(eps, id)
			}
		}
		return mode, eps
	}
	if code, body := g.do(t, "POST", "/containers/create", browserCreate("1000:1000", map[string]any{"NetworkMode": intNet})); code != http.StatusCreated {
		t.Fatalf("the browser server: %d %s", code, body)
	}
	if mode, _ := networks(); mode != "id-"+browserNet {
		t.Fatalf("the browser server's network = %q, want the browser network", mode)
	}
	relay := func(host, ports string) map[string]any {
		return map[string]any{"Image": relayPin, "Env": []string{"PROXY_HOSTNAME=" + host, "PROXY_PORTS=" + ports},
			"HostConfig":       map[string]any{"NetworkMode": ext},
			"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{intNet: map[string]any{}}}}
	}
	if code, body := g.do(t, "POST", "/containers/create", relay("agentry-ab12-browse-proxy", "3128")); code != http.StatusCreated {
		t.Fatalf("the browse proxy's relay: %d %s", code, body)
	}
	if mode, eps := networks(); mode != "id-agentry-A-relays-out" || !slices.Equal(eps, []string{"id-" + browserNet}) {
		t.Fatalf("the browse proxy's relay: %q %v, want the way out and the browser network", mode, eps)
	}
	// Another relay stays on the internal network, and may not take the
	// browser's.
	if code, body := g.do(t, "POST", "/containers/create", relay("api.example.com", "443")); code != http.StatusCreated {
		t.Fatalf("another relay: %d %s", code, body)
	}
	if _, eps := networks(); !slices.Equal(eps, []string{"id-agentry-A-relays"}) {
		t.Fatalf("another relay: %v", eps)
	}
	other := relay("api.example.com", "443")
	other["NetworkingConfig"] = map[string]any{"EndpointsConfig": map[string]any{browserNet: map[string]any{}}}
	g.refused(t, "another relay on the browser network", "POST", "/containers/create", other, "browser server's and its relay's alone")
	g.refused(t, "a declared server on the browser network", "POST", "/containers/create",
		map[string]any{"Image": "node:24", "HostConfig": map[string]any{"NetworkMode": browserNet}}, "browser server's and its relay's alone")
	g.refused(t, "the browser server on another network", "POST", "/containers/create",
		browserCreate("1000:1000", map[string]any{"NetworkMode": "runa-net"}), "joins its own network alone")
	g.refused(t, "a container joining the browser network", "POST", "/networks/"+browserNet+"/connect", map[string]any{"Container": "runa-1"}, "takes no container after it is made")
}
