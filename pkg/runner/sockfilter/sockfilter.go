// Package sockfilter is the per-run filter between a run's MCP gateway and
// the docker daemon (hosted runs in production D7 a, agentry-cli#219).
//
// The gateway launches the tool and MCP server containers, so it needs the
// daemon's API; handed the daemon's own socket, a compromised gateway
// commands every container on the host, which is what stops runs sharing a
// sandbox host (plan hosted-runs-production.md D2, D7). The filter serves a
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
	// networks become the run's own (DR-22 narrowed, agentry-cli#220). The
	// name is translated on the way in (paths, create bodies, endpoints)
	// and back in what the gateway reads, so it never sees the run's
	// names and never reaches another run's relays.
	NetworkNames map[string]string
	// WebNetwork, when set, is the run's own network with a way out of the
	// host for the built-in Drafter's web tools (agentry-cli#345): the
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
	// resolv.conf naming the policy's resolvers (agentry-cli#386): the
	// filter binds it read-only at /etc/resolv.conf in the web tools'
	// server, in place of docker's, whose embedded DNS a sandboxed runtime
	// does not reach (DR-23). The gateway may not name a resolv.conf or DNS
	// settings of its own for that server.
	WebResolvConf string
	// WebImage is the tools server's image, by reference; the filter
	// resolves it to the image id the check compares, once, when a check
	// first needs it, and refuses the web network while it cannot.
	WebImage string
	// RelayHosts are what a relay may be pointed at: the run's declared
	// servers' egress entries and the agent's own egress hosts on 443
	// (agentry-cli#346), each "host" (ports 443 and 80), "host:port",
	// "*.domain" or "*". Empty admits no relay.
	RelayHosts []string
	// Limits bound every container created through the filter: a request
	// asking for more is refused, one asking for none gets these.
	Limits Limits
	// Runtime is the OCI runtime (the policy's docker.runtime) every
	// container the gateway creates runs under, the relays excepted: a
	// relay resolves its declared host through docker's embedded DNS,
	// which a sandboxed runtime does not reach (DR-23). For the same
	// reason a container under it reaches what it links to through
	// /etc/hosts entries the filter pins. Empty leaves the daemon's
	// default. The gateway never chooses a runtime itself.
	Runtime string
	// Log receives one line per request.
	Log io.Writer
}

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
	// (webImage), and kept once it resolved.
	webImageID string
	webMu      sync.Mutex

	mu      sync.Mutex
	execs   map[string]bool
	refused atomic.Int64
	logMu   sync.Mutex
}

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
