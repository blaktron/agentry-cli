// Package dockerx wraps the docker CLI behind the smallest interface the
// engine needs. The CLI, not an SDK: the runner targets "plain docker CE
// on a server" (RUNNER-PLAN R14 heritage), the CLI is the one interface
// guaranteed present there, and an interface this small stays fakeable —
// the whole execute path is unit-tested against Fake (RUNNER-DEVIATIONS
// DR-7 records why that matters on this machine).
package dockerx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// Client is what the engine talks to.
type Client interface {
	// Available errors when no usable docker daemon answers.
	Available(ctx context.Context) error
	// ImageExists reports whether ref is present locally.
	ImageExists(ctx context.Context, ref string) (bool, error)
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
	// Logs returns a container's recent output (gateway diagnostics).
	Logs(ctx context.Context, name string, tail int) (string, error)
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
	// Labels tag the container for teardown sweeps.
	Labels map[string]string
	// Interactive attaches stdin (foreground runs).
	Interactive bool
	// AutoRemove passes --rm; teardown still force-removes by name in
	// case the client process died before docker could reap.
	AutoRemove bool
}

// Mount is one bind mount, already placeholder-resolved.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
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
	cmd := c.command(ctx, "logs", "--tail", strconv.Itoa(tail), name)
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
	if r.Network != "" {
		args = append(args, "--network", r.Network)
	}
	if r.Alias != "" {
		args = append(args, "--network-alias", r.Alias)
	}
	for _, k := range sortedKeys(r.Labels) {
		args = append(args, "--label", k+"="+r.Labels[k])
	}
	for _, k := range sortedKeys(r.Env) {
		args = append(args, "-e", k) // value travels via process env
	}
	for _, m := range r.Mounts {
		spec := m.Source + ":" + m.Target
		if m.ReadOnly {
			spec += ":ro"
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
