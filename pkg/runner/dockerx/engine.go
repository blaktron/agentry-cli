package dockerx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/procx"
)

// The kinds of container engine the runner drives: what the daemon says it
// is, not which CLI talks to it.
const (
	KindDocker = "docker"
	KindPodman = "podman"
)

// Engine is what a run needs to know of the container engine behind a
// client: where the client sends its requests, what answers there, and
// whether that daemon shares this machine's kernel. A run reads it once and
// the CLI keeps it for the life of the process.
type Engine struct {
	// Kind is KindDocker (a server component named "Engine") or KindPodman
	// ("Podman Engine", or podman's own CLI).
	Kind string
	// CLI is the client binary family driven: "docker" or "podman".
	CLI string
	// Version is the daemon's version.
	Version string
	// Endpoint is where the client's requests go, as the client resolves
	// it: unix:///path, npipe://…, tcp://…, ssh://….
	Endpoint string
	// Rootless says the daemon runs as the person, not as root.
	Rootless bool
	// DaemonHost and DaemonKernel are the hostname and kernel release the
	// daemon reports: this machine's for a local daemon, a VM's otherwise.
	DaemonHost   string
	DaemonKernel string
	// OS is what the daemon says it runs on ("Docker Desktop", a
	// distribution's name).
	OS string
}

// Endpoint schemes the docker and podman clients use.
const (
	SchemeUnix  = "unix"
	SchemeNpipe = "npipe"
)

// ParseEndpoint splits a client endpoint into its scheme and its address:
// the socket's path for unix://, the pipe's for npipe://, host[:port] for
// tcp:// and ssh://. A bare absolute path is a Unix socket.
func ParseEndpoint(raw string) (scheme, addr string, err error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "/") {
		return SchemeUnix, raw, nil
	}
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok || scheme == "" {
		return "", "", fmt.Errorf("container engine endpoint %q names no scheme", raw)
	}
	switch scheme = strings.ToLower(scheme); scheme {
	case SchemeUnix:
		if !strings.HasPrefix(rest, "/") {
			return "", "", fmt.Errorf("container engine endpoint %q is not an absolute socket path", raw)
		}
		return scheme, rest, nil
	case SchemeNpipe:
		if rest == "" {
			return "", "", fmt.Errorf("container engine endpoint %q names no pipe", raw)
		}
		return scheme, rest, nil
	}
	u, perr := url.Parse(raw)
	if perr != nil || u.Host == "" {
		return "", "", fmt.Errorf("container engine endpoint %q names no host", raw)
	}
	return scheme, u.Host, nil
}

// SocketPath is the Unix socket the endpoint names, "" when it names
// something else.
func (e Engine) SocketPath() string {
	scheme, addr, err := ParseEndpoint(e.Endpoint)
	if err != nil || scheme != SchemeUnix {
		return ""
	}
	return addr
}

// Summary is the engine in one phrase: its kind and version, the CLI when
// it is not the engine's own, and whether it runs as root.
func (e Engine) Summary() string {
	s := e.Kind + " " + e.Version
	if e.CLI != "" && e.CLI != e.Kind {
		s += " through the " + e.CLI + " CLI"
	}
	if e.Rootless {
		return s + ", rootless"
	}
	return s + ", rootful"
}

// EngineDescriber is the client's account of its engine (Engine): an optional
// capability asked for with a type assertion, like Execer. A client without it
// is taken to be Docker Engine on the conventional socket on this machine.
type EngineDescriber interface {
	Engine(ctx context.Context) (Engine, error)
}

// DescribeEngine is d's engine, or the default for a client that cannot say.
func DescribeEngine(ctx context.Context, d Client) (Engine, error) {
	if ed, ok := d.(EngineDescriber); ok {
		return ed.Engine(ctx)
	}
	return LocalDockerEngine(), nil
}

// DefaultSocket is the Docker Engine's conventional socket.
const DefaultSocket = "/var/run/docker.sock"

// LocalDockerEngine is Docker Engine on its conventional socket, run by root
// on this machine.
func LocalDockerEngine() Engine {
	host, _ := os.Hostname()
	return Engine{Kind: KindDocker, CLI: "docker", Version: "unknown", Endpoint: SchemeUnix + "://" + DefaultSocket,
		DaemonHost: host, DaemonKernel: HostKernel()}
}

// HostKernel is this machine's kernel release, "" where it cannot be read
// (any system but Linux).
func HostKernel() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Engine implements EngineDescriber. The docker CLI is asked where its
// current context points (DOCKER_HOST and DOCKER_CONTEXT as it honours them),
// what the server's first component is called, and the daemon's hostname,
// kernel and security options; podman's own CLI answers all of it from
// `podman info`, and its API socket, which the run's docker filter and the
// gateway need, must answer a ping. The answer is kept for the life of the
// client.
func (c *CLI) Engine(ctx context.Context) (Engine, error) {
	if c.misnamed != nil {
		return Engine{}, c.misnamed
	}
	c.resolve(ctx)
	c.mu.Lock()
	cached := c.engine
	c.mu.Unlock()
	if cached != nil {
		return *cached, nil
	}
	var e Engine
	var err error
	if c.Podman {
		e, err = c.podmanEngine(ctx)
	} else {
		e, err = c.dockerEngine(ctx)
	}
	if err != nil {
		return Engine{}, err
	}
	c.mu.Lock()
	c.engine = &e
	c.mu.Unlock()
	return e, nil
}

// serverVersion is the part of `docker version --format {{json .Server}}`
// the engine reads.
type serverVersion struct {
	Version    string `json:"Version"`
	Components []struct {
		Name    string `json:"Name"`
		Version string `json:"Version"`
	} `json:"Components"`
}

// daemonInfoFormat asks `docker info` for the four fields the engine reads
// and nothing of the client's, so the CLI's plugins are not consulted.
const daemonInfoFormat = `{"name":{{json .Name}},"kernel":{{json .KernelVersion}},"security":{{json .SecurityOptions}},"os":{{json .OperatingSystem}}}`

type daemonInfo struct {
	Name     string   `json:"name"`
	Kernel   string   `json:"kernel"`
	Security []string `json:"security"`
	OS       string   `json:"os"`
}

func (c *CLI) dockerEngine(ctx context.Context) (Engine, error) {
	endpoint, err := c.run(ctx, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	if err != nil {
		return Engine{}, fmt.Errorf("the docker CLI's context: %w", err)
	}
	vout, err := c.run(ctx, "version", "--format", "{{json .Server}}")
	if err != nil {
		return Engine{}, fmt.Errorf("docker daemon not reachable: %w", err)
	}
	iout, err := c.run(ctx, "info", "--format", daemonInfoFormat)
	if err != nil {
		return Engine{}, fmt.Errorf("docker daemon not reachable: %w", err)
	}
	return parseDockerEngine(endpoint, vout, iout)
}

// parseDockerEngine reads the docker CLI's three answers.
func parseDockerEngine(endpoint, versionJSON, infoJSON string) (Engine, error) {
	var v serverVersion
	if err := json.Unmarshal([]byte(strings.TrimSpace(versionJSON)), &v); err != nil {
		return Engine{}, fmt.Errorf("docker version: %w", err)
	}
	var info daemonInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(infoJSON)), &info); err != nil {
		return Engine{}, fmt.Errorf("docker info: %w", err)
	}
	e := Engine{CLI: "docker", Endpoint: strings.TrimSpace(endpoint), Version: v.Version,
		DaemonHost: info.Name, DaemonKernel: info.Kernel, OS: info.OS}
	kind, err := kindOfComponent(v)
	if err != nil {
		return Engine{}, err
	}
	e.Kind = kind
	for _, opt := range info.Security {
		if opt == "name=rootless" {
			e.Rootless = true
		}
	}
	return e, nil
}

// kindOfComponent is the kind the server's first component names.
func kindOfComponent(v serverVersion) (string, error) {
	if len(v.Components) == 0 {
		return "", errors.New("docker version: the server names no component, so its kind is unknown")
	}
	switch name := v.Components[0].Name; name {
	case "Engine":
		return KindDocker, nil
	case "Podman Engine":
		return KindPodman, nil
	default:
		return "", fmt.Errorf("docker version: the server's engine is %q, which the runner does not drive (Docker Engine and podman serve the Docker API it needs)", name)
	}
}

// podmanInfo is the part of `podman info --format json` the engine reads.
type podmanInfo struct {
	Host struct {
		Arch         string `json:"arch"`
		Hostname     string `json:"hostname"`
		Kernel       string `json:"kernel"`
		Distribution struct {
			Distribution string `json:"distribution"`
			Version      string `json:"version"`
		} `json:"distribution"`
		OCIRuntime struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"ociRuntime"`
		RemoteSocket struct {
			Path string `json:"path"`
		} `json:"remoteSocket"`
		Security struct {
			Rootless bool `json:"rootless"`
		} `json:"security"`
		ServiceIsRemote    bool `json:"serviceIsRemote"`
		NetworkBackendInfo struct {
			DefaultNetwork string `json:"defaultNetwork"`
		} `json:"networkBackendInfo"`
	} `json:"host"`
	Version struct {
		Version string `json:"Version"`
	} `json:"version"`
}

func (c *CLI) podmanInfo(ctx context.Context) (podmanInfo, error) {
	out, err := c.run(ctx, "info", "--format", "json")
	if err != nil {
		return podmanInfo{}, fmt.Errorf("podman not usable: %w", err)
	}
	var info podmanInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &info); err != nil {
		return podmanInfo{}, fmt.Errorf("podman info: %w", err)
	}
	return info, nil
}

func (c *CLI) podmanEngine(ctx context.Context) (Engine, error) {
	info, err := c.podmanInfo(ctx)
	if err != nil {
		return Engine{}, err
	}
	if info.Host.ServiceIsRemote {
		return Engine{}, errors.New("podman drives a service on another machine (CONTAINER_HOST, a connection, or a podman machine), and the runner drives podman only on this machine: its docker filter needs the service's socket here")
	}
	e := parsePodmanEngine(info)
	if err := PingSocket(ctx, e.SocketPath()); err != nil {
		how := "systemctl --user enable --now podman.socket"
		if !e.Rootless {
			how = "systemctl enable --now podman.socket"
		}
		return Engine{}, fmt.Errorf("podman's API socket %s does not answer (%v): the run's docker filter and the gateway need it — start it with `%s`", e.Endpoint, err, how)
	}
	return e, nil
}

// parsePodmanEngine reads `podman info`.
func parsePodmanEngine(info podmanInfo) Engine {
	e := Engine{Kind: KindPodman, CLI: "podman", Version: info.Version.Version, Rootless: info.Host.Security.Rootless,
		DaemonHost: info.Host.Hostname, DaemonKernel: info.Host.Kernel, OS: info.Host.Distribution.Distribution}
	if p := info.Host.RemoteSocket.Path; p != "" {
		e.Endpoint = p
		if strings.HasPrefix(p, "/") {
			e.Endpoint = SchemeUnix + "://" + p
		}
	}
	return e
}

// PingSocket asks the Docker API on a Unix socket for /_ping: an engine is
// known by its answer, never by its socket file, which a stopped service can
// leave behind.
func PingSocket(ctx context.Context, path string) error {
	if path == "" {
		return errors.New("no Unix socket")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := conn.Write([]byte("GET /_ping HTTP/1.0\r\nHost: engine\r\n\r\n")); err != nil {
		return err
	}
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("no answer: %w", err)
	}
	if f := strings.Fields(status); len(f) < 2 || f[1] != "200" {
		return fmt.Errorf("answered %q", strings.TrimSpace(status))
	}
	return nil
}

// PersonUser is the --user a container runs as when the files it writes into
// the person's directories must be theirs: their own uid:gid on a rootful
// engine, and root on a rootless one, where the container's root is the
// person and any other uid is a subordinate one that cannot write their
// directories. "" leaves the image's own user, on a host without POSIX ids.
func PersonUser(ctx context.Context, d Client) (string, error) {
	e, err := DescribeEngine(ctx, d)
	if err != nil {
		return "", err
	}
	return e.PersonUser(), nil
}

// PersonUser is PersonUser for an engine already described.
func (e Engine) PersonUser() string {
	uid, gid, ok := e.PersonIDs()
	switch {
	case !ok:
		return ""
	case uid == 0 && gid == 0:
		return "0"
	}
	return fmt.Sprintf("%d:%d", uid, gid)
}

// PersonIDs are the uid and gid that, inside a container on e, are the
// person who runs the CLI: their own on a rootful engine, root's on a
// rootless one, where the engine maps the person to the container's root
// and every other id to a subordinate one. A file a container hands to
// these ids is the person's to remove. ok is false on a host without POSIX
// ids.
func (e Engine) PersonIDs() (uid, gid int, ok bool) {
	uid, gid = os.Getuid(), os.Getgid()
	if uid < 0 || gid < 0 {
		return 0, 0, false
	}
	if e.Rootless {
		return 0, 0, true
	}
	return uid, gid, true
}

// RuntimeChecker verifies one OCI runtime by the name a policy gives it: an
// optional capability of a Client, asked for with a type assertion. On
// podman the name is tried (`podman --runtime <name> info`), since podman's
// configuration lists every runtime it names, installed or not; on docker
// the daemon's table is read.
type RuntimeChecker interface {
	// CheckRuntime is the registration behind name, or why there is none.
	CheckRuntime(ctx context.Context, name string) (Runtime, error)
}

// CheckRuntime implements RuntimeChecker.
func (c *CLI) CheckRuntime(ctx context.Context, name string) (Runtime, error) {
	c.resolve(ctx)
	if !c.Podman {
		table, err := c.Runtimes(ctx)
		if err != nil {
			return Runtime{}, err
		}
		rt, ok := table[name]
		if !ok {
			return Runtime{}, fmt.Errorf("runtime %q is not registered on this docker daemon (it has: %s)", name, strings.Join(SortedRuntimeNames(table), ", "))
		}
		return rt, nil
	}
	out, err := c.run(ctx, "--runtime", name, "info", "--format", "{{.Host.OCIRuntime.Name}} {{.Host.OCIRuntime.Path}}")
	if err != nil {
		return Runtime{}, fmt.Errorf("runtime %q is not registered with podman (%v): name it under [engine.runtimes] in containers.conf", name, err)
	}
	got, path, _ := strings.Cut(strings.TrimSpace(out), " ")
	if got != name || path == "" {
		return Runtime{}, fmt.Errorf("runtime %q is not registered with podman: `podman --runtime %s info` names %q at %q", name, name, got, path)
	}
	return Runtime{Path: path}, nil
}

// CheckRuntime implements RuntimeChecker for Fake, from its table.
func (f *Fake) CheckRuntime(ctx context.Context, name string) (Runtime, error) {
	table, err := f.Runtimes(ctx)
	if err != nil {
		return Runtime{}, err
	}
	rt, ok := table[name]
	if !ok {
		return Runtime{}, fmt.Errorf("runtime %q is not registered (the engine has: %s)", name, strings.Join(SortedRuntimeNames(table), ", "))
	}
	return rt, nil
}

// CheckHostUDSWrapper says why the file podman registers for the gateway's
// runtime (gateway, at path) is not a wrapper that runs the policy's
// runtime (registered at runtime) with HostUDSOpen: podman takes a path per
// runtime name and no arguments, so the gateway's runtime is a script that
// adds the flag, and the script is read for it. It must name the runtime's
// own path or the binary that path runs, since docker.runtime is itself a
// wrapper where the runtime needs a flag of its own (a rootless podman's
// --ignore-cgroups). A binary cannot be read that way and is refused with
// the recipe. nil when it is right.
func CheckHostUDSWrapper(path, runtime, gateway string) error {
	b, err := readScript(path)
	if err != nil {
		return fmt.Errorf("%q is registered at %s, which cannot be read (%v)", gateway, path, err)
	}
	if b == nil {
		return fmt.Errorf("%q is registered at %s, which is not a script: register a wrapper script that runs %s with %s", gateway, path, runtime, HostUDSOpen)
	}
	if !bytes.Contains(b, []byte(HostUDSOpen)) {
		return fmt.Errorf("%q is registered at %s, a script that does not pass %s: the gateway could not open its run's docker filter under it", gateway, path, HostUDSOpen)
	}
	if !bytes.Contains(b, []byte(runtime)) && !bytes.Contains(b, []byte(RuntimeBinary(runtime))) {
		return fmt.Errorf("%q is registered at %s, a script that does not name %s or the binary it runs: it must run the same runtime as docker.runtime", gateway, path, runtime)
	}
	return nil
}

// readScript reads a registered runtime's file when it is a script (a
// shebang line, within a size a script has); nil for a binary.
func readScript(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the path podman registers for the runtime
	if err != nil {
		return nil, err
	}
	if len(b) > 1<<20 || !bytes.HasPrefix(b, []byte("#!")) {
		return nil, nil
	}
	return b, nil
}

// RuntimeBinary is the binary a registered runtime runs: the path itself,
// or, when the path is a wrapper script, the first absolute path in it that
// names an existing file, which is what such a script execs.
func RuntimeBinary(path string) string {
	b, err := readScript(path)
	if err != nil || b == nil {
		return path
	}
	for _, f := range strings.Fields(string(b)) {
		f = strings.Trim(f, `"'`)
		if !strings.HasPrefix(f, "/") {
			continue
		}
		if fi, err := os.Stat(f); err == nil && fi.Mode().IsRegular() {
			return f
		}
	}
	return path
}

// PodmanServicer starts a Docker-compatible API service of podman's own
// whose default runtime is the one named: an optional capability of a
// Client that drives podman's CLI. Every container created through such a
// service's socket lands under that runtime, whatever its create body
// says, which is how a run's gateway puts the containers it launches under
// the policy's runtime on podman.
type PodmanServicer interface {
	// PodmanServiceCommand is the command that serves the API on the Unix
	// socket at path with runtime as its default, until stopped; it is not
	// started.
	PodmanServiceCommand(ctx context.Context, runtime, path string) (*exec.Cmd, error)
}

// PodmanServiceCommand implements PodmanServicer for podman's own CLI; the
// docker CLI, which has no such service, refuses.
func (c *CLI) PodmanServiceCommand(ctx context.Context, runtime, path string) (*exec.Cmd, error) {
	c.resolve(ctx)
	if !c.Podman {
		return nil, errors.New("a per-run podman API service needs podman's own CLI (AGENTRY_ENGINE=podman)")
	}
	cmd := exec.CommandContext(ctx, c.Bin, "--runtime", runtime, "system", "service", "--time=0", SchemeUnix+"://"+path) //nolint:gosec // the engine's binary, fixed args
	cmd.Env = os.Environ()
	procx.NoWindow(cmd)
	// A CLI that dies takes its service with it, so a killed run leaves
	// no API service behind that the next run's sweep would have to find.
	dieWithParent(cmd)
	return cmd, nil
}

// Engine selection. AGENTRY_ENGINE names the CLI to drive ("docker" or
// "podman"); AGENTRY_DOCKER and AGENTRY_PODMAN name each binary. With no
// engine named and no docker endpoint given (DOCKER_HOST, DOCKER_CONTEXT),
// the client finds its engine on first use: the docker CLI when it reaches a
// daemon, else podman when it is installed and answers, else the docker CLI
// again, whose error then says what is missing. A miss is not kept, so an
// engine that starts later is found.
const (
	EnvEngine = "AGENTRY_ENGINE"
	EnvDocker = "AGENTRY_DOCKER"
	EnvPodman = "AGENTRY_PODMAN"
)

// detectWait bounds each probe of detection.
const detectWait = 15 * time.Second

func podmanBin() string {
	if b := os.Getenv(EnvPodman); b != "" {
		return b
	}
	return "podman"
}

// resolve settles which CLI this client drives, once.
func (c *CLI) resolve(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.detect {
		return
	}
	probe := func(bin string, args ...string) bool {
		if _, err := exec.LookPath(bin); err != nil {
			return false
		}
		pctx, cancel := context.WithTimeout(ctx, detectWait)
		defer cancel()
		cmd := exec.CommandContext(pctx, bin, args...) //nolint:gosec // the engine's own binary, fixed args
		procx.NoWindow(cmd)
		out, err := cmd.Output()
		return err == nil && strings.TrimSpace(string(out)) != ""
	}
	if probe(c.Bin, "version", "--format", "{{.Server.Version}}") {
		c.detect = false
		return
	}
	if pb := podmanBin(); probe(pb, "info", "--format", "{{.Host.Arch}}") {
		c.Bin, c.Podman, c.detect = pb, true, false
	}
}
