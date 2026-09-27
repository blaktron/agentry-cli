package mcpinstall

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage"
)

// The install's egress (agentry-cli#92, DR-12). A run's containers have
// one route out, the CONNECT proxy whose allowlist the plan names; the
// MCP-server builder used to fetch over Docker's default bridge, which
// reached every host there is. Now each install gets the same shape as a
// run: an internal network with no route off it, and on it the runner
// image's proxy, the one container with a second leg on the bridge, whose
// allowlist is the configured registry plus the artifact hosts the
// registry's own metadata named for the pinned release. The builder sees
// the proxy through the usual proxy variables and nothing else.

const (
	// installProxyAlias and installProxyPort locate the proxy on the
	// install network.
	installProxyAlias = "mcp-install-proxy"
	installProxyPort  = 3128
	// installProxyLogSuffix names the proxy's decisions beside the
	// server's record in the store: <id>.install-proxy.log.
	installProxyLogSuffix = ".install-proxy.log"
	// installProxyLogLines bounds the log kept; an install makes a few
	// dozen connections.
	installProxyLogLines = 2000
)

// installNetworkName and installProxyName are fixed per server, like the
// builder's own name: a leftover from a CLI killed mid-install is removed
// before the next install of that server (and the proxy by the owner
// sweep, agentry-cli#161).
func installNetworkName(id string) string { return "agentry-mcpinstall-" + id + "-net" }

// installEgressName is the proxy's own way out when it runs under the
// policy's runtime (agentry-cli#337): a network joined at create, since a
// sandbox takes no network connected after start and docker refuses the
// default bridge beside a user-defined network at create.
func installEgressName(id string) string { return "agentry-mcpinstall-" + id + "-out" }
func installProxyName(id string) string  { return "agentry-mcpproxy-" + id }

// proxyImage is the runner image, whose proxy role is the run's own
// CONNECT proxy: the tag both clients build from the embedded context
// (DR-14), or the policy's images.runner.
func proxyImage(images policy.Images) string {
	return (&policy.Policy{Images: images}).RunnerImageTag(runnerimage.ContentVersion())
}

// hostPort renders an https URL's host as an allowlist entry (host:port,
// 443 when the URL names none); ok is false for anything else.
func hostPort(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", false
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port), true
}

// fetchHosts are the hosts of the artifact URLs a registry's metadata
// named, other than the registry's own, as allowlist entries. A URL that
// is not https is left out: the proxy tunnels TLS only, so it could not be
// fetched through it anyway, and the install says so when it fails.
func fetchHosts(registry string, urls ...string) []string {
	own, _ := hostPort(registry)
	set := map[string]bool{}
	for _, u := range urls {
		if hp, ok := hostPort(u); ok && hp != own {
			set[hp] = true
		}
	}
	out := make([]string, 0, len(set))
	for hp := range set {
		out = append(out, hp)
	}
	sort.Strings(out)
	return out
}

// installAllowlist is the install proxy's allowlist for one server: the
// configured registry for its ecosystem and the fetch hosts its
// resolution recorded. A registry that is not https is refused here,
// because the proxy only tunnels CONNECT and npm and pip send a plain
// http:// request to a proxy as a request the proxy will not relay.
func installAllowlist(r *Resolved, regs policy.MCPRegistries) ([]string, error) {
	registry, key := regs.NPM, "npm"
	if r.Source == "pypi" {
		registry, key = regs.PyPI, "pypi"
	}
	own, ok := hostPort(registry)
	if !ok {
		return nil, fmt.Errorf("mcpRegistries.%s is %q: the MCP-server builder reaches its registry only through the install proxy, which tunnels https alone — configure an https:// registry", key, registry)
	}
	allow := append([]string{own}, r.FetchHosts...)
	sort.Strings(allow)
	return slices.Compact(allow), nil
}

// installEgress is one install's network and proxy.
type installEgress struct {
	d       dockerx.Client
	network string
	proxy   string
	logw    io.Writer
	// out and dnsDir are set when the proxy runs under the runtime: its
	// egress network and the directory holding its resolv.conf.
	out    string
	dnsDir string
}

// env is the builder's proxy environment: both spellings of the https and
// http variables (pip reads the lower case, npm either), npm's own config
// names, and pip's, so neither falls back to a direct connection that the
// internal network would refuse with a less useful error.
func (e *installEgress) env() map[string]string {
	u := fmt.Sprintf("http://%s:%d", installProxyAlias, installProxyPort)
	return map[string]string{
		"HTTPS_PROXY": u, "https_proxy": u, "HTTP_PROXY": u, "http_proxy": u,
		"npm_config_https_proxy": u, "npm_config_proxy": u,
		"PIP_PROXY": u,
		"NO_PROXY":  "", "no_proxy": "",
	}
}

// startInstallProxy creates the install's internal network and starts the
// proxy on it, joined to the bridge as its second leg; under the policy's
// runtime with the host's name servers (agentry-cli#337), to a network of
// the install's own at create instead, reading a resolv.conf of its own.
// The runner image
// must already be built (the install step builds it before any server is
// installed); a missing one is refused rather than left to docker, which
// would try to pull a name that is only ever built locally.
func startInstallProxy(ctx context.Context, d dockerx.Client, id string, allow []string,
	images policy.Images, docker policy.DockerPolicy, logw io.Writer,
) (*installEgress, error) {
	image := proxyImage(images)
	if ok, err := d.ImageExists(ctx, image); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("the runner image %s, whose proxy the MCP-server builder installs through, is not built — the install step builds it before installing servers", image)
	}
	e := &installEgress{d: d, network: installNetworkName(id), proxy: installProxyName(id), logw: logw}
	// A CLI killed mid-install leaves both behind under these names.
	_ = d.RemoveForce(ctx, e.proxy)
	_ = d.NetworkRemove(ctx, e.network)
	labels := installLabels(id)
	if err := d.NetworkCreate(ctx, e.network, true, labels); err != nil {
		return nil, fmt.Errorf("create install network: %w", err)
	}
	lim := docker.Limits.WithDefaults()
	spec := dockerx.RunSpec{
		Name:    e.proxy,
		Image:   image,
		Argv:    []string{"proxy", "--allow", strings.Join(allow, ","), "--addr", fmt.Sprintf(":%d", installProxyPort)},
		Network: e.network,
		Alias:   installProxyAlias,
		Labels:  labels,
		// The daemon's default runtime, like the run's llm-proxy, unless
		// the host's name servers are named: it resolves the registry's
		// hosts by name, and a sandboxed runtime does not reach docker's
		// embedded DNS (measured under runsc on dev, DR-23,
		// agentry-cli#221, #337).
	}.WithPosture(lim.Memory, lim.CPUs, lim.Pids)
	spec.ReadOnly, spec.Tmpfs = true, []string{"/tmp"}
	if docker.SandboxesProxies() {
		if err := e.sandboxProxy(ctx, id, labels, docker, &spec); err != nil {
			e.stop(ctx, "")
			return nil, err
		}
	}
	if err := d.RunDetached(ctx, spec); err != nil {
		e.stop(ctx, "")
		return nil, fmt.Errorf("start install proxy: %w", err)
	}
	if e.out == "" {
		if err := d.NetworkConnect(ctx, "bridge", e.proxy); err != nil {
			e.stop(ctx, "")
			return nil, fmt.Errorf("connect install proxy to bridge: %w", err)
		}
	}
	_, _ = fmt.Fprintf(logw, "mcp server install egress: through the install proxy, allowing %s\n", strings.Join(allow, ", "))
	return e, nil
}

// sandboxProxy puts the proxy under the policy's runtime (agentry-cli#337):
// its egress network, made here and joined at create, and a resolv.conf of
// the install's own naming the host's name servers, bound over docker's.
func (e *installEgress) sandboxProxy(ctx context.Context, id string, labels map[string]string,
	docker policy.DockerPolicy, spec *dockerx.RunSpec,
) error {
	_ = e.d.NetworkRemove(ctx, installEgressName(id))
	if err := e.d.NetworkCreate(ctx, installEgressName(id), false, labels); err != nil {
		return fmt.Errorf("create the install proxy's egress network: %w", err)
	}
	e.out = installEgressName(id)
	dir, err := os.MkdirTemp("", "agentry-mcpinstall-dns-")
	if err != nil {
		return fmt.Errorf("make the install proxy's resolv.conf directory: %w", err)
	}
	e.dnsDir = dir
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // read by the proxy's user
		return fmt.Errorf("open the install proxy's resolv.conf directory to it: %w", err)
	}
	conf := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(conf, policy.ResolvConf(docker.Resolvers), 0o644); err != nil { //nolint:gosec // read by the proxy's user
		return fmt.Errorf("write the install proxy's resolv.conf: %w", err)
	}
	spec.Runtime = docker.Runtime
	spec.ExtraNetworks = []string{e.out}
	spec.Mounts = append(spec.Mounts, dockerx.Mount{Source: conf, Target: policy.ResolvConfTarget, ReadOnly: true, Relabel: true})
	return nil
}

// hosts pins the proxy's alias into a builder that runs under a
// sandboxed runtime, which cannot ask docker's embedded DNS for it: the
// builder under runsc failed every install with "getaddrinfo EAI_AGAIN
// mcp-install-proxy" (dev, agentry-cli#221). Without a runtime nothing is
// pinned and docker resolves the alias as before.
func (e *installEgress) hosts(ctx context.Context, runtime string) ([]string, error) {
	if runtime == "" {
		return nil, nil
	}
	ip, err := e.d.ContainerIP(ctx, e.proxy, e.network)
	if err != nil {
		return nil, fmt.Errorf("pin the install proxy for the sandboxed builder: %w", err)
	}
	return []string{installProxyAlias + ":" + ip}, nil
}

// stop keeps the proxy's decisions at logPath (when given), then removes
// the proxy and the network. Under a context that survives an interrupt,
// so an interrupted install still leaves its record and no containers.
func (e *installEgress) stop(ctx context.Context, logPath string) {
	keep := context.WithoutCancel(ctx)
	if logPath != "" {
		logs, err := e.d.Logs(keep, e.proxy, installProxyLogLines)
		if err == nil {
			if werr := os.WriteFile(logPath, []byte(logs), 0o600); werr != nil && e.logw != nil {
				_, _ = fmt.Fprintf(e.logw, "warning: could not keep the install proxy's log: %v\n", werr)
			}
		} else if e.logw != nil && !errors.Is(err, context.Canceled) {
			_, _ = fmt.Fprintf(e.logw, "warning: could not read the install proxy's log: %v\n", err)
		}
	}
	_ = e.d.RemoveForce(keep, e.proxy)
	_ = e.d.NetworkRemove(keep, e.network)
	if e.out != "" {
		_ = e.d.NetworkRemove(keep, e.out)
	}
	if e.dnsDir != "" {
		_ = os.RemoveAll(e.dnsDir)
	}
}
