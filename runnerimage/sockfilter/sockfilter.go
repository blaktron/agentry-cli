// Package sockfilter is the per-run filter between a run's MCP gateway and
// the docker daemon.
//
// The gateway launches the tool and MCP server containers, so it needs the
// daemon's API; handed the daemon's own socket, a compromised gateway
// commands every container on the host, which is what would stop runs sharing a
// sandbox host. The filter serves a
// socket of its own in the run's directory, which the gateway mounts in
// place of the daemon's, and forwards a request only when it touches this
// run's objects in a way that stays inside the run:
//
//   - every container, network and volume created through it carries the
//     run's label, whatever the request said;
//   - a request naming a container, network, volume or exec must name one
//     carrying that label (the gateway's shared relay networks excepted,
//     by name); lists and the event stream show only the run's objects;
//   - a create body is decoded the way the daemon decodes it (field names
//     in any case), refused if it holds a field the filter does not know,
//     checked, and forwarded as the filter re-encoded it, so the daemon
//     receives exactly what was checked (create.go);
//   - the network that leaves the host takes only the gateway's relays,
//     pinned by digest, and nothing may exec into or write to a relay;
//   - the daemon's version, info and ping, image inspects and pulls by
//     reference pass as they are (images are not a run's objects);
//   - an object that does not exist gets the daemon's 404;
//   - everything else is refused with 403.
//
// Every decision is written to the log, so a run shows what its gateway
// was denied; the count of refusals is Refused.
package sockfilter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config says whose objects the filter serves, where, and within what.
type Config struct {
	// RunID is the run whose objects the gateway may touch.
	RunID string
	// Label is the key every run object carries RunID under (the runner's
	// sweeps find the gateway's objects by it); OwnerLabel the key only
	// objects created through the filter carry, which is what makes one
	// the gateway's to touch. The runner's own containers (the agent, the
	// model proxy, the gateway itself) carry Label and not OwnerLabel, so
	// the gateway cannot exec into, stop or join them.
	Label      string
	OwnerLabel string
	// Listen is the socket the filter serves; created 0666 (the gateway is
	// root without DAC_OVERRIDE, and not the socket's owner). Its directory
	// is the run's own, so no other account reaches it.
	Listen string
	// Upstream is the daemon's socket.
	Upstream string
	// BindRoots are the host directories a bind source may sit under,
	// read-only; WritableRoots those it may also write (the gateway's own
	// two lists, MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS and …_WRITABLE_PATHS).
	BindRoots     []string
	WritableRoots []string
	// SharedNetworks are the networks the runner made for the gateway to
	// use without the owner label: the run's relay networks. They must
	// carry Label=RunID; the gateway may not create or remove them.
	SharedNetworks []string
	// EgressNetworks are shared networks with a way out of the host, each
	// with the image digests (repository@sha256:…) that may join it: the
	// relays' leg out takes the relays and nothing else a gateway launches.
	EgressNetworks map[string][]string
	// NetworkNames maps a network name the gateway uses to the network
	// the daemon holds for this run: the pinned gateway's two fixed relay
	// networks become the run's own. The
	// name is translated on the way in (paths, create bodies, endpoints)
	// and back in what the gateway reads, so it never sees the run's
	// names and never reaches another run's relays.
	NetworkNames map[string]string
	// WebNetwork, when set, is the run's own network with a way out of the
	// host for the built-in Drafter's web tools: the
	// tools server has no network allowlist of its own there (its tools
	// hold their own address rule), and the gateway asks for the daemon's
	// default network for it, which the filter never gives. A container of
	// WebImage asking for the default network joins WebNetwork instead, and
	// no container of another image joins WebNetwork at all. It must carry
	// Label=RunID; the gateway may not create or remove it.
	WebNetwork string
	// WebCmd is the one command the web tools' server runs with (the
	// runner image's tools role and its spec), and WebSpec the host path of
	// the run's rendered tools spec it must be given read-only at
	// WebSpecTarget: a container on the web network runs the run's own
	// tools server and nothing else, as a relay is its image and nothing
	// else.
	WebCmd        []string
	WebSpec       string
	WebSpecTarget string
	// WebResolvConf, when set, is the host path of the run's own
	// resolv.conf naming the policy's resolvers: the
	// filter binds it read-only at /etc/resolv.conf in the web tools'
	// server, in place of docker's, whose embedded DNS a sandboxed runtime
	// does not reach. The gateway may not name a resolv.conf or DNS
	// settings of its own for that server.
	WebResolvConf string
	// RelayResolvConf, when set, is the host path of the run's own
	// resolv.conf for the relays: the filter binds it
	// read-only at /etc/resolv.conf in each relay and creates the relay
	// under Runtime, since a relay resolves its declared host by name and
	// a sandboxed runtime does not reach docker's embedded DNS.
	// Empty leaves the relays on the daemon's default and docker's DNS.
	// The gateway still names no file, DNS setting or runtime for a relay.
	RelayResolvConf string
	// WebImage is the tools server's image, by reference; the filter
	// resolves it to the image id the check compares, once, when a check
	// first needs it, and refuses the web network while it cannot.
	WebImage string
	// RelayHosts are what a relay may be pointed at: the run's declared
	// servers' egress entries and the agent's own egress hosts on 443,
	// each "host" (ports 443 and 80), "host:port",
	// "*.domain" or "*". Empty admits no relay.
	RelayHosts []string
	// ToolsImage and ToolsCmd name the run's tools server: the image it
	// runs in, by reference (resolved to its id when first needed, as
	// WebImage is), and the one command it runs with. ToolsTmpfs is what
	// the filter mounts into that container and no other, target to
	// options: its /tmp and scratch folder as size-capped tmpfs,
	// since the gateway's catalog cannot ask for
	// one. A tools server whose request names anything at or beneath one
	// of those targets is refused. Empty adds nothing.
	ToolsImage string
	ToolsCmd   []string
	ToolsTmpfs map[string]string
	// ToolsShared are the binds the tools server may have beneath one of
	// its tmpfs targets, target to host folder: the host link's copy folder
	// at /scratch/hosts, which the run made. The request
	// must name exactly that folder at exactly that target; anything else
	// at or beneath a tmpfs target is still refused.
	ToolsShared map[string]string
	// ToolsVolumes are the named volumes not the run's that the tools
	// server may mount, by exact name: a persistent agent's data volume,
	// which outlives every run and so carries no run label. No other
	// container may mount one.
	ToolsVolumes []string
	// BrowserImage names the run's browser server: the
	// image a create must be of, by id (resolved when first needed, as
	// ToolsImage is), to be held as the browser class: every capability
	// dropped, a read-only root, BrowserTmpfs mounted, its user
	// BrowserUser, and, with no Runtime, the seccomp profile that lets
	// Chromium's own sandbox run (browser.go). Empty: no browser.
	BrowserImage string
	// BrowserUser is the user the browser server must run as, uid:gid,
	// which owns its tmpfs home; empty is the image's own (10001:10001).
	BrowserUser string
	// BrowserTmpfs is what the filter mounts into the browser server, target
	// to options without the owner, which BrowserUser decides: its home and
	// its /tmp, where Chromium keeps its shared memory.
	BrowserTmpfs map[string]string
	// BrowserNetwork is the run's network for the browser server and the one
	// relay it reaches the browse proxy through, alone: where the gateway names
	// BrowserNetworkFor (the run's internal relay network) for either, the
	// filter gives it this one, so a page that took the browser over
	// reaches no other server's relay; no other container may join it, and
	// the browser server may join nothing after it is made. BrowseProxy is
	// the proxy's host name, the relay's PROXY_HOSTNAME.
	BrowserNetwork    string
	BrowserNetworkFor string
	BrowseProxy       string
	// RelayPins are run-time hosts the filter writes into the /etc/hosts of
	// a relay pointed at one, host to IPv4 address: the run's browse proxy,
	// a container of its own whose name a relay under a sandboxed runtime,
	// on the host's name servers, could not resolve.
	RelayPins map[string]string
	// Limits bound every container created through the filter: a request
	// asking for more is refused, one asking for none gets these.
	Limits Limits
	// Runtime is the OCI runtime (the policy's docker.runtime) every
	// container the gateway creates runs under, the relays excepted unless
	// RelayResolvConf is set: a relay resolves its declared host by name,
	// and a sandboxed runtime does not reach docker's embedded DNS,
	// so it runs under Runtime only with the host's name servers. For the same
	// reason a container under it reaches what it links to through
	// /etc/hosts entries the filter pins. Empty leaves the daemon's
	// default. The gateway never chooses a runtime itself.
	Runtime string
	// Podman says the upstream is podman's Docker-compatible API, which
	// differs from Docker Engine's in three ways the filter follows: a
	// link (HostConfig.Links, an endpoint's) is refused there, so each
	// becomes the /etc/hosts entries the filter pins under a runtime and
	// is cleared from the body; a rootless container keeps the kernel's
	// unprivileged-port floor, so a relay, which binds 443 as a plain
	// user, is given the sysctl that lowers it to 0 (Docker Engine sets it
	// in every container); and a create body's runtime is dropped, so with
	// Runtime set the upstream is an API service of the run's own whose
	// default runtime is Runtime, and every container created through it,
	// the relays included, runs under it.
	Podman bool
	// SELinuxUnconfined creates every container that runs under Runtime
	// with its SELinux label disabled: on podman on an SELinux host, gVisor
	// refuses to start a labelled container. The runtime's own isolation
	// stands in for the label's.
	SELinuxUnconfined bool
	// Verify, when set, is called after the upstream creates a container
	// that must run under a runtime, with the container's id, its class in
	// the receipt's words and the runtime it must be under, before the
	// gateway sees the answer: it reads the runtime back from the engine
	// natively and returns why the two differ, which refuses the create
	// to the gateway and is kept as RuntimeMismatch. Nil reads the runtime
	// through the daemon's own inspect instead (the filter container,
	// which has no engine CLI; Docker Engine's inspect answers truly).
	Verify func(ctx context.Context, id, class, runtime string) error `json:"-"`
	// Log receives one line per request.
	Log io.Writer `json:"-"`
}

// relayPortSysctl is the kernel setting that lets a relay bind 443 as a
// user with no capability on a rootless engine.
const relayPortSysctl = "net.ipv4.ip_unprivileged_port_start"

// Limits are the resource ceilings for the containers a gateway creates.
// Zero leaves a resource unbounded.
type Limits struct {
	MemoryBytes int64
	NanoCPUs    int64
	Pids        int64
}

// Filter is a running filter.
type Filter struct {
	cfg      Config
	ln       net.Listener
	srv      *http.Server
	upstream *http.Client
	proxy    *httputil.ReverseProxy

	// webImageID is WebImage resolved on the first check that needs it
	// (webImage), and kept once it resolved. webMu guards it and
	// toolsImageID.
	webImageID string
	webMu      sync.Mutex
	// toolsImageID is ToolsImage resolved the same way, under webMu too,
	// and browserImageID BrowserImage.
	toolsImageID   string
	browserImageID string

	mu      sync.Mutex
	execs   map[string]bool
	refused atomic.Int64
	logMu   sync.Mutex
	// mismatch is the first runtime Verify refused, kept for the run.
	mismatch atomic.Pointer[runtimeRefusal]
}

// runtimeRefusal is a create the engine answered but Verify refused: the
// container was not created under the runtime it had to run under.
type runtimeRefusal struct{ err error }

func (e *runtimeRefusal) Error() string { return e.err.Error() }
func (e *runtimeRefusal) Unwrap() error { return e.err }

// RuntimeMismatch is the first container the engine created under another
// runtime than the one it was asked for, as Verify reported it, or nil.
// The gateway's create was refused; the run reads this to stop rather than
// carry on with a receipt that would name a sandbox a container did not
// have.
func (f *Filter) RuntimeMismatch() error {
	if m := f.mismatch.Load(); m != nil {
		return m.err
	}
	return nil
}

// createMeta is what containerCreate decides for the response's check:
// the container's class and the runtime it must be under.
type createMeta struct{ class, runtime string }

type createMetaKey struct{}

// Start serves the filter on cfg.Listen.
func Start(cfg Config) (*Filter, error) {
	if cfg.RunID == "" || cfg.Label == "" || cfg.OwnerLabel == "" || cfg.Label == cfg.OwnerLabel || cfg.Listen == "" || cfg.Upstream == "" {
		return nil, errors.New("sockfilter: run id, two distinct labels, listen and upstream are required")
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	_ = os.Remove(cfg.Listen)
	ln, err := net.Listen("unix", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("sockfilter: listen: %w", err)
	}
	if err := os.Chmod(cfg.Listen, 0o666); err != nil { //nolint:gosec // see Config.Listen
		_ = ln.Close()
		return nil, fmt.Errorf("sockfilter: socket mode: %w", err)
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", cfg.Upstream)
	}
	tr := &http.Transport{DialContext: dial, DisableCompression: true}
	f := &Filter{
		cfg:      cfg,
		ln:       ln,
		upstream: &http.Client{Transport: tr, Timeout: 30 * time.Second},
		execs:    map[string]bool{},
	}
	f.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = "docker"
			r.Out.Host = "docker"
		},
		Transport:      tr,
		FlushInterval:  -1, // logs, events, pulls and attach stream
		ModifyResponse: f.modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			var rr *runtimeRefusal
			if errors.As(err, &rr) {
				writeJSON(w, http.StatusForbidden, "agentry run filter: "+err.Error())
				return
			}
			http.Error(w, "agentry run filter: daemon: "+err.Error(), http.StatusBadGateway)
		},
	}
	f.srv = &http.Server{Handler: f, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = f.srv.Serve(ln) }()
	return f, nil
}

// Close stops serving and removes the socket.
func (f *Filter) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := f.srv.Shutdown(ctx)
	if err != nil {
		_ = f.srv.Close()
	}
	_ = os.Remove(f.cfg.Listen)
	return err
}

// Refused is how many requests the filter has refused; the log names them.
func (f *Filter) Refused() int { return int(f.refused.Load()) }

// versionPrefix is the API version a client may put before any path.
var versionPrefix = regexp.MustCompile(`^/v[0-9]+\.[0-9]+(/|$)`)

// canonical strips the API version: rules match the rest.
func canonical(p string) string {
	if loc := versionPrefix.FindStringIndex(p); loc != nil {
		p = "/" + p[loc[1]:]
	}
	return path.Clean("/" + p)
}

// versionOf returns the "/v1.47" a path starts with, or "".
func versionOf(p string) string {
	if loc := versionPrefix.FindStringIndex(p); loc != nil {
		return strings.TrimSuffix(p[:loc[1]], "/")
	}
	return ""
}

// ServeHTTP decides each request.
func (f *Filter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := canonical(r.URL.Path)
	// A create's class and runtime travel to the response's check.
	r = r.WithContext(context.WithValue(r.Context(), createMetaKey{}, &createMeta{}))
	fwd, err := f.decide(r, p)
	if err != nil {
		var nf notFound
		if errors.As(err, &nf) {
			f.logf("absent %s %s: %s", r.Method, p, nf.Error())
			writeJSON(w, http.StatusNotFound, nf.Error())
			return
		}
		var ex exists
		if errors.As(err, &ex) {
			f.logf("exists %s %s: %s", r.Method, p, ex.Error())
			writeJSON(w, http.StatusConflict, ex.Error())
			return
		}
		f.refused.Add(1)
		f.logf("refuse %s %s: %s", r.Method, p, err.Error())
		writeJSON(w, http.StatusForbidden, "agentry run filter: "+err.Error())
		return
	}
	f.logf("allow %s %s", r.Method, p)
	// The daemon gets exactly the path that was judged: cleaned, decoded
	// and with its API version kept, so no ".." or encoded slash can be
	// read one way here and another way there.
	r.URL.Path = versionOf(r.URL.Path) + fwd
	r.URL.RawPath = ""
	f.proxy.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}

func (f *Filter) logf(format string, a ...any) {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	_, _ = fmt.Fprintf(f.cfg.Log, time.Now().UTC().Format(time.RFC3339)+" "+format+"\n", a...)
}

// refusal is a request the filter will not forward.
type refusal struct{ reason string }

func (e refusal) Error() string { return e.reason }

func deny(format string, a ...any) error { return refusal{fmt.Sprintf(format, a...)} }

// notFound is the daemon's own answer for an object that does not exist,
// passed on as the daemon gives it: a gateway reads 404 as "absent" (the
// pinned one looks up its hostname as a container and expects none), and
// a missing object is not a refusal.
type notFound struct{ kind, id string }

func (e notFound) Error() string { return "No such " + e.kind + ": " + e.id }

// exists is the daemon's own answer to creating a network that is already
// there, passed on as the daemon gives it: the pinned gateway creates its
// relay networks unconditionally and takes "already exists" as success.
type exists struct{ name string }

func (e exists) Error() string { return "network with name " + e.name + " already exists" }

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
