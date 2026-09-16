// Package dockerx wraps the docker CLI behind the smallest interface the
// engine needs. The CLI, not an SDK: the runner targets "plain docker CE
// on a server" (RUNNER-PLAN R14 heritage), the CLI is the one interface
// guaranteed present there, and an interface this small stays fakeable —
// the whole execute path is unit-tested against Fake (RUNNER-DEVIATIONS
// DR-7 records why that matters on this machine).
package dockerx

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// LogsAll asks Logs for the whole log.
const LogsAll = 0

// Client is what the engine talks to.
type Client interface {
	// Available errors when no usable docker daemon answers.
	Available(ctx context.Context) error
	// Runtimes lists the OCI runtimes the daemon has registered (runc,
	// runsc, …), by name — what a policy's docker.runtime may name.
	Runtimes(ctx context.Context) ([]string, error)
	// ContainerIP is a running container's address on one network.
	ContainerIP(ctx context.Context, name, network string) (string, error)
	// ImageExists reports whether ref is present locally.
	ImageExists(ctx context.Context, ref string) (bool, error)
	// ImageFile reads one file out of an image without running it (a
	// stopped container is created, copied from, removed).
	ImageFile(ctx context.Context, ref, path string) ([]byte, error)
	// Pull pulls ref (first-use install of pinned images, RQ2).
	Pull(ctx context.Context, ref string) error
	// Build builds dir into tag with the given build args.
	Build(ctx context.Context, dir, tag string, buildArgs map[string]string) error
	// NetworkCreate creates a bridge network; internal networks have no
	// route out (§R4 agent topology).
	NetworkCreate(ctx context.Context, name string, internal bool, labels map[string]string) error
	// NetworkConnect attaches an existing container to another network.
	NetworkConnect(ctx context.Context, network, container string) error
	// NetworkRemove removes a network.
	NetworkRemove(ctx context.Context, name string) error
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
	// RemoveForce force-removes a container; not-found is not an error.
	RemoveForce(ctx context.Context, name string) error
	// PSByLabel lists container names carrying label (leak sweeps).
	PSByLabel(ctx context.Context, label string) ([]string, error)
	// Logs returns a container's output: the last tail lines, or all of
	// it for LogsAll (gateway diagnostics and startup checks).
	Logs(ctx context.Context, name string, tail int) (string, error)
	// Port returns the host port a container port was published on
	// (RunSpec.Publish), for a host-run harness to reach the run.
	Port(ctx context.Context, name string, containerPort int) (int, error)
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
	Env map[string]string
	// Mounts are bind mounts.
	Mounts []Mount
	// Network and Alias attach the container to the run network.
	Network string
	Alias   string
	// Hostname sets the container's hostname when it must differ from
	// the container name (the gateway looks itself up by hostname).
	Hostname string
	// Labels tag the container for teardown sweeps.
	Labels map[string]string
	// User runs the container's process as this uid:gid (`--user`), for a
	// container that writes into a directory the invoking user owns and
	// must be able to remove afterwards (the MCP server installer,
	// agentry-cli#112). Empty leaves the image's own user.
	User string
	// Runtime names the daemon's OCI runtime for this container
	// (`--runtime`): a policy's docker.runtime, so the container that runs
	// the BriefAgent's code can sit behind gVisor on a host that has it
	// (DR-23). Empty leaves the daemon's default, which is what every local
	// run had before the knob.
	Runtime string
	// ExtraHosts pins names to addresses in the container's /etc/hosts
	// (`--add-host name:ip`). A sandboxed runtime's network stack does not
	// reach docker's embedded DNS, so the run network's aliases travel
	// this way when a runtime is set (DR-23).
	ExtraHosts []string
	// Entrypoint replaces the image's entrypoint (`--entrypoint`): for a
	// one-shot that borrows an image for a shell command, such as the
	// session store's ownership step (agentry-cli#90). Empty keeps it.
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
	// access to every container on the machine instead of this one
	// (agentry-cli#85). Every other container in a run stays confined. On a
	// daemon built without SELinux support the option is accepted and does
	// nothing.
	DisableSELinuxLabel bool
}

// Mount is one bind mount, already placeholder-resolved.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
	// Relabel asks docker to give the source an SELinux label the
	// container can read (the `z` option, agentry-cli#85). Set it for
	// directories THIS RUN created and nothing else: relabeling is a write
	// to the host's file labels, so pointing it at a path that belongs to
	// the machine — the docker socket above all — would change what every
	// other container on that host may touch. On a daemon built without
	// SELinux support docker accepts the option and ignores it (measured:
	// the source's label is untouched), so it costs nothing to carry.
	Relabel bool
}

// CLI is the real client.
type CLI struct {
	// Bin is the docker binary ("docker" resolved via PATH by default;
	// AGENTRY_DOCKER overrides for odd installs).
	Bin string
}

// New returns a CLI client.
func New() *CLI {
	bin := os.Getenv("AGENTRY_DOCKER")
	if bin == "" {
		bin = "docker"
	}
	return &CLI{Bin: bin}
}

func (c *CLI) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Bin, args...) //nolint:gosec // fixed binary, structured args
	cmd.Env = os.Environ()
	return cmd
}

// run executes a docker command, returning stdout and a described error.
func (c *CLI) run(ctx context.Context, args ...string) (string, error) {
	cmd := c.command(ctx, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Available implements Client.
func (c *CLI) Available(ctx context.Context) error {
	if _, err := exec.LookPath(c.Bin); err != nil {
		return fmt.Errorf("docker CLI not found (%s): the runner needs a docker engine — install docker or set AGENTRY_DOCKER", c.Bin)
	}
	if _, err := c.run(ctx, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return fmt.Errorf("docker daemon not reachable: %w", err)
	}
	return nil
}

// Runtimes implements Client from `docker info`: the daemon's runtime
// table keyed by name. The names are what `--runtime` accepts.
func (c *CLI) Runtimes(ctx context.Context) ([]string, error) {
	out, err := c.run(ctx, "info", "--format", "{{json .Runtimes}}")
	if err != nil {
		return nil, fmt.Errorf("docker daemon not reachable: %w", err)
	}
	var table map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &table); err != nil {
		return nil, fmt.Errorf("docker info runtimes: %w", err)
	}
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
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

// ImageFile implements Client: `docker create` a container from the image
// (never started — the exec image has no shell to start with), `docker cp`
// the path out as a tar stream, `docker rm` the container.
func (c *CLI) ImageFile(ctx context.Context, ref, path string) ([]byte, error) {
	id, err := c.run(ctx, "create", ref)
	if err != nil {
		return nil, fmt.Errorf("docker create %s: %w", ref, err)
	}
	id = strings.TrimSpace(id)
	defer func() {
		// The removal must happen even when the caller's context is
		// already done (a Ctrl-C mid-copy), or the created container leaks.
		rm := c.command(context.WithoutCancel(ctx), "rm", "-f", id)
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

// Pull implements Client.
func (c *CLI) Pull(ctx context.Context, ref string) error {
	_, err := c.run(ctx, "pull", ref)
	return err
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

// NetworkConnect implements Client.
func (c *CLI) NetworkConnect(ctx context.Context, network, container string) error {
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
	cmd := c.command(ctx, r.args(true)...)
	cmd.Env = append(cmd.Env, r.envPairs()...)
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker run %s: %w: %s", r.Name, err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// RunForeground implements Client.
func (c *CLI) RunForeground(ctx context.Context, r RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := c.command(ctx, r.args(false)...)
	cmd.Env = append(cmd.Env, r.envPairs()...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	err := cmd.Run()
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
			return code, fmt.Errorf("docker run %s failed (exit %d)", r.Name, code)
		}
		return code, nil
	}
	return -1, fmt.Errorf("docker run %s: %w", r.Name, err)
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
	_, err := c.run(ctx, "rm", "-f", name)
	if err != nil && isNotFound(err) {
		return nil
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

// Logs implements Client. Both streams are returned: a container's stderr
// (where the gateway writes its startup errors) comes out of `docker logs`
// on stderr, and a diagnostic that dropped it explained nothing
// (agentry-cli#41).
func (c *CLI) Logs(ctx context.Context, name string, tail int) (string, error) {
	// docker's --tail 0 prints nothing; a non-positive tail here means the
	// whole log, which a startup check over the gateway's output needs.
	tailArg := "all"
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

// args assembles the docker run argv (without env values, see RunSpec.Env).
func (r RunSpec) args(detach bool) []string {
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
	if r.Network != "" {
		args = append(args, "--network", r.Network)
	}
	if r.Hostname != "" {
		args = append(args, "--hostname", r.Hostname)
	}
	if r.Alias != "" {
		args = append(args, "--network-alias", r.Alias)
	}
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
	for _, k := range sortedKeys(r.Env) {
		args = append(args, "-e", k) // value travels via process env
	}
	if r.DisableSELinuxLabel {
		args = append(args, "--security-opt", "label=disable")
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
	args = append(args, r.Image)
	return append(args, r.Argv...)
}

// envPairs renders NAME=value pairs for the docker client's environment.
func (r RunSpec) envPairs() []string {
	pairs := make([]string, 0, len(r.Env))
	for _, k := range sortedKeys(r.Env) {
		pairs = append(pairs, k+"="+r.Env[k])
	}
	return pairs
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
