// Package dockerx wraps the docker CLI behind the smallest interface the engine
// needs. The CLI, not an SDK: the runner targets "plain docker CE on a server",
// the CLI is the one interface guaranteed present there, and an interface this
// small stays fakeable — the whole execute path is unit-tested against Fake.
package dockerx

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/blaktron/agentry-cli/pkg/runner/procx"
)

// LogsAll asks Logs for the whole log.
const LogsAll = 0

// Client is what the engine talks to.
type Client interface {
	// Available errors when no usable docker daemon answers.
	Available(ctx context.Context) error
	// Runtimes is the daemon's runtime table: each registered OCI runtime
	// (runc, runsc, …) by the name a policy's docker.runtime may give,
	// with the binary it runs and the arguments its registration adds.
	Runtimes(ctx context.Context) (map[string]Runtime, error)
	// ContainerIP is a running container's address on one network.
	ContainerIP(ctx context.Context, name, network string) (string, error)
	// ServerArch is the daemon's architecture as Go names it (amd64,
	// arm64): the platform an image built here runs on.
	ServerArch(ctx context.Context) (string, error)
	// ImageExists reports whether ref is present locally.
	ImageExists(ctx context.Context, ref string) (bool, error)
	// ImageLabel is a present image's label key, "" when it has none.
	ImageLabel(ctx context.Context, ref, key string) (string, error)
	// ImageFile reads one file out of an image without running it (a
	// stopped container is created, copied from, removed).
	ImageFile(ctx context.Context, ref, path string) ([]byte, error)
	// Pull pulls ref (first-use install of pinned images).
	Pull(ctx context.Context, ref string) error
	// Build builds dir into tag with the given build args.
	Build(ctx context.Context, dir, tag string, buildArgs map[string]string) error
	// NetworkCreate creates a bridge network; internal networks have no
	// route out.
	NetworkCreate(ctx context.Context, name string, internal bool, labels map[string]string) error
	// NetworkConnect attaches an existing container to another network.
	NetworkConnect(ctx context.Context, network, container string) error
	// NetworkRemove removes a network.
	NetworkRemove(ctx context.Context, name string) error
	// NetworksByLabel lists the names of the networks carrying label
	// ("k=v", or "k" for any value), for the sweep of a dead run's
	// networks whose names its run id alone does not determine.
	NetworksByLabel(ctx context.Context, label string) ([]string, error)
	// VolumeCreate creates a named volume with labels; VolumeRemove
	// removes one (absent is not an error); VolumesByLabel lists the
	// names carrying a label, for the run's teardown sweep.
	VolumeCreate(ctx context.Context, name string, labels map[string]string) error
	VolumeRemove(ctx context.Context, name string) error
	VolumesByLabel(ctx context.Context, label string) ([]string, error)
	// RunDetached starts a container in the background.
	RunDetached(ctx context.Context, r RunSpec) error
	// RunForeground runs a container to completion, streaming stdio, and
	// returns its exit code. A non-zero container exit is NOT an error —
	// the (code, nil) pair is the normal shape; err means docker itself
	// failed.
	RunForeground(ctx context.Context, r RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	// Stop gracefully stops a container (SIGTERM, then kill after
	// timeout). Not-found is not an error.
	Stop(ctx context.Context, name string, timeoutSeconds int) error
	// RemoveForce force-removes a container and its anonymous volumes;
	// not-found is not an error.
	RemoveForce(ctx context.Context, name string) error
	// PSByLabel lists container names carrying label (leak sweeps).
	PSByLabel(ctx context.Context, label string) ([]string, error)
	// PSLabels lists every container (running or not) carrying label, each
	// with the values of the label keys asked for ("" where it has none):
	// the startup sweep reads who created a container before removing it.
	PSLabels(ctx context.Context, label string, keys []string) ([]Labeled, error)
	// Logs returns a container's output: the last tail lines, or all of
	// it for LogsAll (gateway diagnostics and startup checks).
	Logs(ctx context.Context, name string, tail int) (string, error)
	// Port returns the host port a container port was published on
	// (RunSpec.Publish), for a host-run harness to reach the run.
	Port(ctx context.Context, name string, containerPort int) (int, error)
}

// Labeled is one container PSLabels found, with the label values it read.
type Labeled struct {
	Name   string
	Labels map[string]string
}

// RunSpec describes one docker run.
type RunSpec struct {
	Name  string
	Image string
	// Argv follows the image (entrypoint args).
	Argv []string
	// Env is passed by NAME through the docker client's environment
	// (`-e NAME` + cmd.Env), never as `-e NAME=value` argv — values (the
	// gateway token, the model key) must not appear in process listings.
	// The exception is a name the docker client reads itself (clientReads:
	// HOME, PATH, USERPROFILE, DOCKER_*): set in the client's environment
	// it would reconfigure the client, so it goes as `-e NAME=value` and
	// must never carry a secret.
	Env map[string]string
	// Mounts are bind mounts.
	Mounts []Mount
	// Volumes are anonymous volumes mounted at these paths, each with its
	// own labels (`--mount type=volume,dst=…,volume-label=k=v`), in place
	// of the unlabelled anonymous volume an image's VOLUME would otherwise
	// get. Like any anonymous volume they go with their
	// container (RemoveForce passes -v); the labels let teardown and the
	// sweeps find one whose container went some other way.
	Volumes []VolumeMount
	// Network and Alias attach the container to the run network.
	Network string
	Alias   string
	// ExtraNetworks are the container's other networks, joined at create rather
	// than by a connect after start: a sandboxed runtime's network stack takes
	// no network connected once it runs (as under gVisor), and docker refuses
	// the default bridge beside a user-defined network at create, so each is
	// user-defined. Alias, if any, is Network's alone.
	ExtraNetworks []string
	// Hostname sets the container's hostname when it must differ from
	// the container name (the gateway looks itself up by hostname).
	Hostname string
	// Labels tag the container for teardown sweeps.
	Labels map[string]string
	// User runs the container's process as this uid:gid (`--user`), for a
	// container that writes into a directory the invoking user owns and
	// must be able to remove afterwards (the MCP server installer).
	// Empty leaves the image's own user.
	User string
	// Runtime names the daemon's OCI runtime for this container
	// (`--runtime`): a policy's docker.runtime, so the container that runs
	// the QuickAgent's code can sit behind gVisor on a host that has it.
	// Empty leaves the daemon's default.
	Runtime string
	// ExtraHosts pins names to addresses in the container's /etc/hosts
	// (`--add-host name:ip`). A sandboxed runtime's network stack does not
	// reach docker's embedded DNS, so the run network's aliases travel
	// this way when a runtime is set.
	ExtraHosts []string
	// Entrypoint replaces the image's entrypoint (`--entrypoint`): for a
	// one-shot that borrows an image for a shell command, such as the
	// session store's ownership step. Empty keeps it.
	Entrypoint string
	// Interactive attaches stdin (foreground runs).
	Interactive bool
	// AutoRemove passes --rm; teardown still force-removes by name in
	// case the client process died before docker could reap.
	AutoRemove bool
	// Publish exposes container ports on the host (-p): only for a
	// host-run harness, and only on 127.0.0.1 (HostPort 0 = ephemeral).
	Publish []PortPublish
	// DisableSELinuxLabel runs this container with SELinux labelling off
	// (`--security-opt label=disable`). It is for the ONE container that
	// drives the host's docker daemon: an SELinux-enabled daemon confines
	// container processes so they cannot open the docker socket at all, and
	// the alternative — relabeling the socket itself — would hand that
	// access to every container on the machine instead of this one.
	// Every other container in a run stays confined. On a
	// daemon built without SELinux support the option is accepted and does
	// nothing.
	DisableSELinuxLabel bool
	// CapDrop and CapAdd are the container's capabilities (`--cap-drop`,
	// `--cap-add`); NoNewPrivileges sets `--security-opt no-new-privileges`;
	// ReadOnly makes the root filesystem read-only with Tmpfs as its writable
	// scratch; PidsLimit, Memory and CPUs are its resource bounds. Zero values
	// leave docker's defaults.
	CapDrop         []string
	CapAdd          []string
	NoNewPrivileges bool
	ReadOnly        bool
	Tmpfs           []string
	PidsLimit       int
	Memory          string
	CPUs            int

	// named are the named volumes podman's dialect mounts in place of
	// Volumes (dialectSpec).
	named []namedVolume
}

// WithPosture is r with the posture every run container takes: every capability
// dropped, no privilege gained through a setuid binary, and memory, CPUs and
// processes bounded. A container that needs one capability back adds it to
// CapAdd afterwards.
func (r RunSpec) WithPosture(memory string, cpus, pids int) RunSpec {
	r.CapDrop = []string{"ALL"}
	r.NoNewPrivileges = true
	r.Memory = memory
	r.CPUs = cpus
	r.PidsLimit = pids
	return r
}

// Mount is one bind mount, already placeholder-resolved.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
	// Relabel asks docker to give the source an SELinux label the container can
	// read (the `z` option). Set it for directories THIS RUN created and
	// nothing else: relabeling is a write to the host's file labels, so
	// pointing it at a path that belongs to the machine — the docker socket
	// above all — would change what every other container on that host may
	// touch. On a daemon built without SELinux support docker accepts the
	// option and ignores it (the source's label is untouched), so it costs
	// nothing to carry.
	Relabel bool
}

// VolumeMount is an anonymous volume at Target carrying Labels.
type VolumeMount struct {
	Target string
	Labels map[string]string
}

// mountField renders one field of a --mount value, which docker reads as
// CSV: a field holding a comma or a quote is quoted, its quotes doubled.
func mountField(f string) string {
	if !strings.ContainsAny(f, ",\"\n") {
		return f
	}
	return `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
}

// CLI is the real client. It drives the docker CLI, or podman's own (Podman),
// whose commands take docker's spelling for everything the runner creates and
// differ only in what they report about the engine.
type CLI struct {
	// Bin is the client binary ("docker" resolved via PATH by default;
	// AGENTRY_DOCKER overrides for odd installs, AGENTRY_PODMAN for podman).
	Bin string
	// Podman says Bin is podman's own CLI.
	Podman bool

	// detect says the CLI to drive is still to be found (resolve).
	detect bool
	mu     sync.Mutex
	engine *Engine
}

// New returns a CLI client for the engine AGENTRY_ENGINE names, or for the
// one it finds on first use (resolve).
func New() *CLI {
	switch strings.ToLower(os.Getenv(EnvEngine)) {
	case KindPodman:
		return &CLI{Bin: podmanBin(), Podman: true}
	case KindDocker:
		return &CLI{Bin: dockerBin()}
	}
	if os.Getenv(EnvDocker) != "" || os.Getenv("DOCKER_HOST") != "" || os.Getenv("DOCKER_CONTEXT") != "" {
		return &CLI{Bin: dockerBin()}
	}
	return &CLI{Bin: dockerBin(), detect: true}
}

func dockerBin() string {
	if b := os.Getenv(EnvDocker); b != "" {
		return b
	}
	return "docker"
}

// name is the CLI's name in an error.
func (c *CLI) name() string {
	if c.Podman {
		return "podman"
	}
	return "docker"
}

func (c *CLI) command(ctx context.Context, args ...string) *exec.Cmd {
	c.resolve(ctx)
	cmd := exec.CommandContext(ctx, c.Bin, args...) //nolint:gosec // fixed binary, structured args
	cmd.Env = os.Environ()
	procx.NoWindow(cmd)
	return cmd
}

// run executes a client command, returning stdout and a described error.
func (c *CLI) run(ctx context.Context, args ...string) (string, error) {
	cmd := c.command(ctx, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %w: %s", c.name(), args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Available implements Client. It asks the daemon for its version with `docker
// version`, not `docker info`: info also fills in its client section, which
// runs every CLI plugin for its metadata before the daemon is asked. A current
// Docker Desktop ships about thirty, and on Windows that took 2–6 s against
// about 0.1 s for version, answered or not. That cost would come out of the
// time a caller allows for an idle engine to wake. Both commands exit non-zero
// with the daemon's error on stderr when nothing answers; a server version that
// comes back empty is treated the same way.
//
// For podman, whose CLI runs the engine itself, `podman info` is the check.
func (c *CLI) Available(ctx context.Context) error {
	c.resolve(ctx)
	if _, err := exec.LookPath(c.Bin); err != nil {
		if c.Podman {
			return fmt.Errorf("podman not found (%s): install podman, or set AGENTRY_PODMAN to its binary", c.Bin)
		}
		return fmt.Errorf("docker CLI not found (%s), and no podman answers: the runner needs a container engine — install Docker Engine or podman, or set AGENTRY_DOCKER", c.Bin)
	}
	if c.Podman {
		if _, err := c.podmanInfo(ctx); err != nil {
			return err
		}
		return nil
	}
	out, err := c.run(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return fmt.Errorf("docker daemon not reachable: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return errors.New("docker daemon not reachable: docker version named no server")
	}
	return nil
}

// OperatingSystem is what the daemon says it runs on (`docker info`'s
// OperatingSystem): "Docker Desktop" for Docker Desktop, the host's
// distribution for Docker Engine. A scheduled run that must go on after
// its person signs out tells the two apart by it.
func (c *CLI) OperatingSystem(ctx context.Context) (string, error) {
	c.resolve(ctx)
	if c.Podman {
		info, err := c.podmanInfo(ctx)
		return info.Host.Distribution.Distribution, err
	}
	out, err := c.run(ctx, "info", "--format", "{{.OperatingSystem}}")
	return strings.TrimSpace(out), err
}

// Runtime is one entry of the daemon's runtime table: the binary it runs
// and the arguments its registration adds (daemon.json's runtimes).
type Runtime struct {
	Path string   `json:"path"`
	Args []string `json:"runtimeArgs"`
}

// HostUDSOpen is the gVisor flag that lets a sandboxed container open a
// Unix socket bind-mounted from the host: the gateway's way to its run's
// docker filter (docker.gatewayRuntime).
const HostUDSOpen = "--host-uds=open"

// Runtimes implements Client from `docker info`: the daemon's runtime
// table keyed by name. The names are what `--runtime` accepts. podman keeps
// no table: its answer is its one default runtime.
func (c *CLI) Runtimes(ctx context.Context) (map[string]Runtime, error) {
	c.resolve(ctx)
	if c.Podman {
		info, err := c.podmanInfo(ctx)
		if err != nil {
			return nil, err
		}
		rt := info.Host.OCIRuntime
		return map[string]Runtime{rt.Name: {Path: rt.Path}}, nil
	}
	out, err := c.run(ctx, "info", "--format", "{{json .Runtimes}}")
	if err != nil {
		return nil, fmt.Errorf("docker daemon not reachable: %w", err)
	}
	var table map[string]Runtime
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &table); err != nil {
		return nil, fmt.Errorf("docker info runtimes: %w", err)
	}
	return table, nil
}

// SortedRuntimeNames lists a runtime table's names, sorted, for a message.
func SortedRuntimeNames(table map[string]Runtime) []string {
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// CheckHostUDSRegistration says why the registration named gateway is not the
// one named runtime plus HostUDSOpen: the same binary, the runtime's own
// arguments and that flag, nothing more. The receipt says the gateway ran under
// that runtime, so a registration that runs runc, or adds a flag such as
// --network=host or --host-uds=all, would make the claim false. A name missing
// from the table, or a registration with no binary, is refused too. nil when it
// is right.
func CheckHostUDSRegistration(table map[string]Runtime, runtime, gateway string) error {
	rt, ok := table[runtime]
	if !ok {
		return fmt.Errorf("%q cannot be checked: the runtime %q is not registered", gateway, runtime)
	}
	gw, ok := table[gateway]
	switch {
	case !ok:
		return fmt.Errorf("%q is not registered: register %q again under that name with runtimeArgs [%q]", gateway, runtime, HostUDSOpen)
	case rt.Path == "" || gw.Path == "":
		return fmt.Errorf("%q cannot be checked: the daemon names no binary for %q or %q", gateway, runtime, gateway)
	case gw.Path != rt.Path:
		return fmt.Errorf("%q runs %s, not %s as %q does: it must be %q registered again with %s", gateway, gw.Path, rt.Path, runtime, runtime, HostUDSOpen)
	}
	extra, missing := argsDiff(gw.Args, append(slices.Clone(rt.Args), HostUDSOpen))
	if len(extra) > 0 || len(missing) > 0 {
		return fmt.Errorf("%q is registered with runtimeArgs %q: it must be %q's own (%q) plus %s, nothing else (extra %q, missing %q)", gateway, gw.Args, runtime, rt.Args, HostUDSOpen, extra, missing)
	}
	return nil
}

// argsDiff compares two argument lists as multisets: what got has beyond
// want, and what it lacks.
func argsDiff(got, want []string) (extra, missing []string) {
	count := map[string]int{}
	for _, a := range want {
		count[a]++
	}
	for _, a := range got {
		if count[a] > 0 {
			count[a]--
			continue
		}
		extra = append(extra, a)
	}
	for _, a := range want {
		if count[a] > 0 {
			count[a]--
			missing = append(missing, a)
		}
	}
	return extra, missing
}

// ContainerIP implements Client from `docker inspect`.
func (c *CLI) ContainerIP(ctx context.Context, name, network string) (string, error) {
	out, err := c.run(ctx, "inspect", "--format", `{{(index .NetworkSettings.Networks "`+network+`").IPAddress}}`, name)
	if err != nil {
		return "", fmt.Errorf("docker inspect %s: %w", name, err)
	}
	ip := strings.TrimSpace(out)
	if ip == "" || ip == "<no value>" {
		return "", fmt.Errorf("container %s has no address on network %s", name, network)
	}
	return ip, nil
}

// ImageExists implements Client.
func (c *CLI) ImageExists(ctx context.Context, ref string) (bool, error) {
	cmd := c.command(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if okErr := asExitError(err, &exitErr); okErr {
		return false, nil // inspect exits 1 for a missing image
	}
	return false, fmt.Errorf("docker image inspect %s: %w", ref, err)
}

// ServerArch implements Client.
//
// podman's CLI runs the engine itself and reports it as the client
// ({{.Client.OsArch}}, "linux/amd64").
func (c *CLI) ServerArch(ctx context.Context) (string, error) {
	c.resolve(ctx)
	if c.Podman {
		out, err := c.run(ctx, "version", "--format", "{{.Client.OsArch}}")
		if err != nil {
			return "", err
		}
		_, arch, _ := strings.Cut(strings.TrimSpace(out), "/")
		return arch, nil
	}
	out, err := c.run(ctx, "version", "--format", "{{.Server.Arch}}")
	if err != nil {
		return "", fmt.Errorf("docker daemon not reachable: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// ImageLabel implements Client.
func (c *CLI) ImageLabel(ctx context.Context, ref, key string) (string, error) {
	out, err := c.run(ctx, "image", "inspect", "--format", "{{json .Config.Labels}}", ref)
	if err != nil {
		return "", fmt.Errorf("docker image inspect %s: %w", ref, err)
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &labels); err != nil {
		return "", fmt.Errorf("docker image inspect %s labels: %w", ref, err)
	}
	return labels[key], nil
}

// ImageFile implements Client: `docker create` a container from the image
// (never started — the exec image has no shell to start with), `docker cp`
// the path out as a tar stream, `docker rm -fv` the container: -v, since
// creating it gave each VOLUME the image declares an anonymous volume.
func (c *CLI) ImageFile(ctx context.Context, ref, path string) ([]byte, error) {
	id, err := c.run(ctx, "create", ref)
	if err != nil {
		return nil, fmt.Errorf("docker create %s: %w", ref, err)
	}
	id = strings.TrimSpace(id)
	defer func() {
		// The removal must happen even when the caller's context is
		// already done (a Ctrl-C mid-copy), or the created container leaks.
		rm := c.command(context.WithoutCancel(ctx), "rm", "-fv", id)
		rm.Stdout, rm.Stderr = io.Discard, io.Discard
		_ = rm.Run()
	}()
	cp := c.command(ctx, "cp", id+":"+path, "-")
	var out, errb bytes.Buffer
	cp.Stdout, cp.Stderr = &out, &errb
	if err := cp.Run(); err != nil {
		return nil, fmt.Errorf("docker cp %s:%s: %w: %s", ref, path, err, strings.TrimSpace(errb.String()))
	}
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, fmt.Errorf("docker cp %s:%s: no file in the archive: %w", ref, path, err)
		}
		if h.Typeflag == tar.TypeReg {
			return io.ReadAll(tr)
		}
	}
}

// Pull implements Client. podman is given the reference docker would read
// (qualifiedRef): its own resolution of a short name may ask which registry
// to use, and a run has no one to answer.
func (c *CLI) Pull(ctx context.Context, ref string) error {
	c.resolve(ctx)
	if c.Podman {
		ref = qualifiedRef(ref)
	}
	_, err := c.run(ctx, "pull", ref)
	return err
}

// qualifiedRef is ref as docker reads it: a name with no registry is Docker
// Hub's, and a single-component one is under library/.
func qualifiedRef(ref string) string {
	first, _, hasSlash := strings.Cut(ref, "/")
	if !hasSlash {
		return "docker.io/library/" + ref
	}
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return ref
	}
	return "docker.io/" + ref
}

// Build implements Client.
func (c *CLI) Build(ctx context.Context, dir, tag string, buildArgs map[string]string) error {
	args := []string{"build", "-t", tag}
	for _, k := range sortedKeys(buildArgs) {
		args = append(args, "--build-arg", k+"="+buildArgs[k])
	}
	args = append(args, dir)
	_, err := c.run(ctx, args...)
	return err
}

// NetworkCreate implements Client.
func (c *CLI) NetworkCreate(ctx context.Context, name string, internal bool, labels map[string]string) error {
	args := []string{"network", "create", "--driver", "bridge"}
	if internal {
		args = append(args, "--internal")
	}
	for _, k := range sortedKeys(labels) {
		args = append(args, "--label", k+"="+labels[k])
	}
	args = append(args, name)
	_, err := c.run(ctx, args...)
	return err
}

// DefaultBridge is docker's name for its default network, the one a
// container reaches past the host through.
const DefaultBridge = "bridge"

// NetworkConnect implements Client. podman's default network has a name of
// its own ("podman" unless its configuration says otherwise), which podman
// gives in place of docker's.
func (c *CLI) NetworkConnect(ctx context.Context, network, container string) error {
	if c.resolve(ctx); c.Podman && network == DefaultBridge {
		info, err := c.podmanInfo(ctx)
		if err != nil {
			return err
		}
		if n := info.Host.NetworkBackendInfo.DefaultNetwork; n != "" {
			network = n
		}
	}
	_, err := c.run(ctx, "network", "connect", network, container)
	return err
}

// NetworkRemove implements Client.
func (c *CLI) NetworkRemove(ctx context.Context, name string) error {
	_, err := c.run(ctx, "network", "rm", name)
	if err != nil && isNotFound(err) {
		return nil
	}
	return err
}

// NetworksByLabel implements Client.
func (c *CLI) NetworksByLabel(ctx context.Context, label string) ([]string, error) {
	out, err := c.run(ctx, "network", "ls", "--filter", "label="+label, "--format", "{{.Name}}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			names = append(names, s)
		}
	}
	return names, nil
}

// VolumeCreate implements Client.
func (c *CLI) VolumeCreate(ctx context.Context, name string, labels map[string]string) error {
	args := []string{"volume", "create"}
	for _, k := range sortedKeys(labels) {
		args = append(args, "--label", k+"="+labels[k])
	}
	args = append(args, name)
	_, err := c.run(ctx, args...)
	return err
}

// VolumeRemove implements Client.
func (c *CLI) VolumeRemove(ctx context.Context, name string) error {
	_, err := c.run(ctx, "volume", "rm", "-f", name)
	if err != nil && isNotFound(err) {
		return nil
	}
	return err
}

// VolumesByLabel implements Client.
func (c *CLI) VolumesByLabel(ctx context.Context, label string) ([]string, error) {
	out, err := c.run(ctx, "volume", "ls", "--filter", "label="+label, "--format", "{{.Name}}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			names = append(names, s)
		}
	}
	return names, nil
}

// RunDetached implements Client.
func (c *CLI) RunDetached(ctx context.Context, r RunSpec) error {
	r, err := c.dialectSpec(ctx, r)
	if err != nil {
		return err
	}
	cmd := c.command(ctx, r.argsFor(true, c.Podman)...)
	cmd.Env = append(cmd.Env, r.envPairsFor(c.Podman)...)
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s run %s: %w: %s", c.name(), r.Name, err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// RunForeground implements Client.
func (c *CLI) RunForeground(ctx context.Context, r RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	r, err := c.dialectSpec(ctx, r)
	if err != nil {
		return -1, err
	}
	cmd := c.command(ctx, r.argsFor(false, c.Podman)...)
	cmd.Env = append(cmd.Env, r.envPairsFor(c.Podman)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	err = cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if asExitError(err, &exitErr) {
		code := exitErr.ExitCode()
		// 125/126/127 are docker-client failures, not the container's
		// own exit; report them as errors so the engine classifies them
		// as environment (13), not agent (14).
		if code >= 125 && ctx.Err() == nil {
			return code, fmt.Errorf("%s run %s failed (exit %d)", c.name(), r.Name, code)
		}
		return code, nil
	}
	return -1, fmt.Errorf("%s run %s: %w", c.name(), r.Name, err)
}

// Stop implements Client.
func (c *CLI) Stop(ctx context.Context, name string, timeoutSeconds int) error {
	_, err := c.run(ctx, "stop", "--timeout", strconv.Itoa(timeoutSeconds), name)
	if err != nil && isNotFound(err) {
		return nil
	}
	return err
}

// RemoveForce implements Client.
func (c *CLI) RemoveForce(ctx context.Context, name string) error {
	// -v: the container's anonymous volumes go with it, an image's
	// VOLUME (the gateway's /misc) included. Docker never
	// takes a named volume by -v (the session's among them) nor an
	// anonymous one another container still uses.
	_, err := c.run(ctx, "rm", "-fv", name)
	if err != nil && isNotFound(err) {
		err = nil
	}
	if err == nil && c.Podman {
		// The named volumes dialectSpec made in place of its anonymous
		// ones go with it too.
		vols, lerr := c.VolumesByLabel(ctx, volumeOfLabel+"="+name)
		if lerr != nil {
			return lerr
		}
		for _, v := range vols {
			if rerr := c.VolumeRemove(ctx, v); rerr != nil {
				return rerr
			}
		}
	}
	return err
}

// PSByLabel implements Client.
func (c *CLI) PSByLabel(ctx context.Context, label string) ([]string, error) {
	out, err := c.run(ctx, "ps", "-a", "--filter", "label="+label, "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			names = append(names, s)
		}
	}
	return names, nil
}

// PSLabels implements Client. Each key is read with docker's own
// {{.Label "k"}}, one tab-separated line per container, so a value is never
// split out of a comma-joined label list; the keys and the values this CLI
// writes carry no tabs or newlines.
func (c *CLI) PSLabels(ctx context.Context, label string, keys []string) ([]Labeled, error) {
	format := "{{.Names}}"
	for _, k := range keys {
		if strings.ContainsAny(k, "\t\n\"{}") {
			return nil, fmt.Errorf("docker ps: label key %q cannot be read back", k)
		}
		format += "\t{{.Label \"" + k + "\"}}"
	}
	out, err := c.run(ctx, "ps", "-a", "--filter", "label="+label, "--format", format)
	if err != nil {
		return nil, err
	}
	var found []Labeled
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != len(keys)+1 {
			return nil, fmt.Errorf("docker ps: %d fields in %q, want %d", len(f), line, len(keys)+1)
		}
		l := Labeled{Name: strings.TrimSpace(f[0]), Labels: map[string]string{}}
		for i, k := range keys {
			l.Labels[k] = f[i+1]
		}
		found = append(found, l)
	}
	return found, nil
}

// Logs implements Client. Both streams are returned: a container's stderr
// (where the gateway writes its startup errors) comes out of `docker logs`
// on stderr, and a diagnostic that dropped it explained nothing.
func (c *CLI) Logs(ctx context.Context, name string, tail int) (string, error) {
	// docker's --tail 0 prints nothing; a non-positive tail here means the
	// whole log, which a startup check over the gateway's output needs.
	tailArg := "all"
	if c.resolve(ctx); c.Podman {
		tailArg = "-1"
	}
	if tail > 0 {
		tailArg = strconv.Itoa(tail)
	}
	cmd := c.command(ctx, "logs", "--tail", tailArg, name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker logs: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// volumeOfLabel marks a named volume podman's dialect made for a container's
// anonymous one (dialectSpec), with the container's name.
const volumeOfLabel = "agentry.volume-of"

// dialectSpec is r as podman's CLI must be given it. podman's run refuses a
// label on an anonymous volume, so each of r.Volumes becomes a named volume
// with the same labels (and the container's name, for RemoveForce), made
// here and mounted by name: the run's teardown and sweeps find it by its
// labels either way. The docker CLI takes r as it is.
func (c *CLI) dialectSpec(ctx context.Context, r RunSpec) (RunSpec, error) {
	c.resolve(ctx)
	if !c.Podman || len(r.Volumes) == 0 {
		return r, nil
	}
	if r.Name == "" {
		return r, errors.New("podman: a container with labelled volumes needs a name")
	}
	vols := r.Volumes
	r.Volumes = nil
	for i, v := range vols {
		labels := maps.Clone(v.Labels)
		if labels == nil {
			labels = map[string]string{}
		}
		labels[volumeOfLabel] = r.Name
		name := r.Name + "-vol" + strconv.Itoa(i)
		if err := c.VolumeCreate(ctx, name, labels); err != nil {
			return r, fmt.Errorf("podman volume for %s:%s: %w", r.Name, v.Target, err)
		}
		r.named = append(r.named, namedVolume{Name: name, Target: v.Target})
	}
	return r, nil
}

// namedVolume is a named volume mounted at Target.
type namedVolume struct {
	Name   string
	Target string
}

// args assembles the docker run argv (without env values, see RunSpec.Env).
func (r RunSpec) args(detach bool) []string { return r.argsFor(detach, false) }

// argsFor assembles the run argv for docker's CLI or podman's. The two take
// the same spelling but for one form: a container on several networks with an
// alias on the first, which podman writes name:alias=… where docker writes
// name=…,alias=….
func (r RunSpec) argsFor(detach, podman bool) []string {
	args := []string{"run"}
	if detach {
		args = append(args, "-d")
	}
	if r.AutoRemove {
		args = append(args, "--rm")
	}
	if r.Interactive {
		args = append(args, "-i")
	}
	if r.Name != "" {
		args = append(args, "--name", r.Name)
	}
	if r.User != "" {
		args = append(args, "--user", r.User)
	}
	if r.Entrypoint != "" {
		args = append(args, "--entrypoint", r.Entrypoint)
	}
	if r.Runtime != "" {
		args = append(args, "--runtime", r.Runtime)
	}
	for _, h := range r.ExtraHosts {
		args = append(args, "--add-host", h)
	}
	var alias []string
	switch {
	case r.Network != "" && len(r.ExtraNetworks) > 0:
		// docker's long form scopes the alias to the network it names.
		first := "name=" + r.Network
		if r.Alias != "" {
			first += ",alias=" + r.Alias
		}
		if podman {
			first = r.Network
			if r.Alias != "" {
				first += ":alias=" + r.Alias
			}
		}
		args = append(args, "--network", first)
		for _, n := range r.ExtraNetworks {
			args = append(args, "--network", n)
		}
	case r.Network != "":
		args = append(args, "--network", r.Network)
		if r.Alias != "" {
			alias = []string{"--network-alias", r.Alias}
		}
	}
	if r.Hostname != "" {
		args = append(args, "--hostname", r.Hostname)
	}
	args = append(args, alias...)
	for _, k := range sortedKeys(r.Labels) {
		args = append(args, "--label", k+"="+r.Labels[k])
	}
	for _, pp := range r.Publish {
		hp := ""
		if pp.HostPort > 0 {
			hp = strconv.Itoa(pp.HostPort)
		}
		args = append(args, "-p", pp.HostIP+":"+hp+":"+strconv.Itoa(pp.ContainerPort))
	}
	reads := clientReads
	if podman {
		reads = podmanReads
	}
	for _, k := range sortedKeys(r.Env) {
		if reads(k) {
			args = append(args, "-e", k+"="+r.Env[k])
			continue
		}
		args = append(args, "-e", k) // value travels via process env
	}
	if r.DisableSELinuxLabel {
		args = append(args, "--security-opt", "label=disable")
	}
	for _, c := range r.CapDrop {
		args = append(args, "--cap-drop", c)
	}
	for _, c := range r.CapAdd {
		args = append(args, "--cap-add", c)
	}
	if r.NoNewPrivileges {
		args = append(args, "--security-opt", "no-new-privileges")
	}
	if r.ReadOnly {
		args = append(args, "--read-only")
	}
	for _, t := range r.Tmpfs {
		args = append(args, "--tmpfs", t)
	}
	if r.PidsLimit > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(r.PidsLimit))
	}
	if r.Memory != "" {
		args = append(args, "--memory", r.Memory)
	}
	if r.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(r.CPUs))
	}
	for _, m := range r.Mounts {
		spec := m.Source + ":" + m.Target
		var opts []string
		if m.ReadOnly {
			opts = append(opts, "ro")
		}
		if m.Relabel {
			// `z` (shared), never `Z` (private): a run mounts the same
			// directory into more than one of its containers — the config
			// dir into the gateway and the harness, the server store into
			// the builder and the runtime — and a private label would give
			// the first container an exclusive category that locks the
			// others out.
			opts = append(opts, "z")
		}
		if len(opts) > 0 {
			spec += ":" + strings.Join(opts, ",")
		}
		args = append(args, "-v", spec)
	}
	for _, v := range r.Volumes {
		fields := []string{"type=volume", mountField("dst=" + v.Target)}
		for _, k := range sortedKeys(v.Labels) {
			fields = append(fields, mountField("volume-label="+k+"="+v.Labels[k]))
		}
		args = append(args, "--mount", strings.Join(fields, ","))
	}
	for _, v := range r.named {
		args = append(args, "--mount", strings.Join([]string{"type=volume", mountField("src=" + v.Name), mountField("dst=" + v.Target)}, ","))
	}
	args = append(args, r.Image)
	return append(args, r.Argv...)
}

// envPairs renders NAME=value pairs for the docker client's environment,
// leaving out the names the client reads itself (args carries those).
func (r RunSpec) envPairs() []string { return r.envPairsFor(false) }

// envPairsFor is envPairs for docker's CLI or podman's.
func (r RunSpec) envPairsFor(podman bool) []string {
	reads := clientReads
	if podman {
		reads = podmanReads
	}
	pairs := make([]string, 0, len(r.Env))
	for _, k := range sortedKeys(r.Env) {
		if reads(k) {
			continue
		}
		pairs = append(pairs, k+"="+r.Env[k])
	}
	return pairs
}

// clientReads says whether the docker client takes name from its own
// environment. HOME (USERPROFILE on Windows) locates ~/.docker/config.json
// and with it the current context, so the gateway's HOME=/root, passed by
// name, sent `docker run` to the default socket; on Docker Desktop for
// macOS, which reaches its engine only through its context, there is none
// and every run failed at the gateway. PATH is where the
// client finds its credential helpers, and DOCKER_* is its own namespace.
// Windows' names are case-insensitive, so the match is too.
func clientReads(name string) bool {
	u := strings.ToUpper(name)
	switch u {
	case "HOME", "USERPROFILE", "PATH":
		return true
	}
	return strings.HasPrefix(u, "DOCKER_")
}

// podmanReads is clientReads for podman's CLI, which runs the engine in its
// own process and so reads more of its environment: where it keeps its
// storage, its configuration and its temporary files (XDG_*, TMPDIR,
// CONTAINERS_*, STORAGE_*, the registries' credentials) and which service it
// talks to (CONTAINER_HOST and the like). A container's variable of such a
// name, set in podman's environment, would reconfigure podman itself.
func podmanReads(name string) bool {
	if clientReads(name) {
		return true
	}
	u := strings.ToUpper(name)
	switch u {
	case "TMPDIR", "REGISTRY_AUTH_FILE", "NOTIFY_SOCKET", "LISTEN_FDS", "LISTEN_PID", "LISTEN_FDNAMES":
		return true
	}
	for _, p := range []string{"XDG_", "CONTAINERS_", "CONTAINER_", "_CONTAINERS_", "PODMAN_", "STORAGE_", "BUILDAH_", "REGISTRIES_"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func isNotFound(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no such container") || strings.Contains(s, "not found")
}

func asExitError(err error, target **exec.ExitError) bool {
	return errors.As(err, target)
}

// PortPublish is one -p mapping.
type PortPublish struct {
	HostIP        string
	HostPort      int
	ContainerPort int
}

// Port implements Client via `docker port`.
func (c *CLI) Port(ctx context.Context, name string, containerPort int) (int, error) {
	out, err := c.run(ctx, "port", name, strconv.Itoa(containerPort)+"/tcp")
	if err != nil {
		return 0, fmt.Errorf("docker port %s: %w", name, err)
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.LastIndex(line, ":"); i >= 0 {
			if p, perr := strconv.Atoi(line[i+1:]); perr == nil && p > 0 {
				return p, nil
			}
		}
	}
	return 0, fmt.Errorf("docker port %s: no published port for %d/tcp in %q", name, containerPort, strings.TrimSpace(out))
}
