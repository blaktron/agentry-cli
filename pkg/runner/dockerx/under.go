package dockerx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// A policy's runtime is enforced, never assumed. The compiled plan asks for
// it with `--runtime`, and both CLIs either apply it or refuse the run
// loudly; an API route may drop it instead (podman's Docker-compatible API
// answers the constant "oci" for every container and applies the service's
// default), and a receipt that named a sandbox the container did not run
// under would be false. So a container that must run under a runtime is
// created, inspected, and started only when the engine's own account of it
// names that runtime: Creator is the client capability that makes the
// three steps possible, and RunDetachedUnder and RunForegroundUnder are the
// one place the check lives.

// Creator splits a run into its two halves and reads a container's runtime
// between them: an optional capability of a Client, asked for with a type
// assertion as Execer is, so a Client that lacks it still satisfies Client.
// The CLI client has it, and so does Fake.
type Creator interface {
	// Create creates r's container without starting it, as `docker
	// create` does: everything RunDetached and RunForeground would pass to
	// `run`, the attached stdin included, so Start and StartAttached find
	// the container ready. r.Name is required.
	Create(ctx context.Context, r RunSpec) error
	// Start starts a created container in the background.
	Start(ctx context.Context, name string) error
	// StartAttached starts a created container with its stdio attached and
	// returns its exit code, as RunForeground does; interactive says stdin
	// is attached.
	StartAttached(ctx context.Context, name string, interactive bool, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	// ContainerRuntime is the OCI runtime the engine created a container
	// with, by the name the engine registers it under: docker's
	// HostConfig.Runtime, podman's OCIRuntime.
	ContainerRuntime(ctx context.Context, name string) (string, error)
}

// RuntimeMismatch is why a run stopped: a container that had to run under
// the policy's runtime was created under another, and the engine did not
// apply the runtime it was asked for.
type RuntimeMismatch struct {
	// Class is the container's role in the run, in the receipt's words.
	Class string
	// Name is the container, Want the policy's runtime and Got the one the
	// engine created it with.
	Name, Want, Got string
}

func (e *RuntimeMismatch) Error() string {
	got := fmt.Sprintf("%q", e.Got)
	if e.Got == "" {
		got = "the engine's default"
	}
	return fmt.Sprintf("the %s (%s) was created under %s, not the policy's docker.runtime %q: the engine did not apply the runtime it was asked for, so the run stops rather than carry a receipt naming a sandbox it did not have", e.Class, e.Name, got, e.Want)
}

// RunDetachedUnder is RunDetached for a container that may have to run
// under the policy's runtime (r.Runtime). Without one it is RunDetached.
// With one, the container is created, its runtime read back from the
// engine and compared, and only then started; a mismatch removes it and
// returns a *RuntimeMismatch naming class. On a podman engine on an
// SELinux host the container is created with its label disabled, since
// gVisor refuses a labelled container there. A client that cannot create
// and inspect (no Creator) is refused a runtime: the receipt says each
// container was checked, so an unchecked one never starts.
func RunDetachedUnder(ctx context.Context, d Client, r RunSpec, class string) error {
	cr, err := createUnder(ctx, d, &r, class)
	if err != nil {
		return err
	}
	if cr == nil {
		return d.RunDetached(ctx, r)
	}
	return cr.Start(ctx, r.Name)
}

// RunForegroundUnder is RunForeground with RunDetachedUnder's check.
func RunForegroundUnder(ctx context.Context, d Client, r RunSpec, class string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cr, err := createUnder(ctx, d, &r, class)
	if err != nil {
		return -1, err
	}
	if cr == nil {
		return d.RunForeground(ctx, r, stdin, stdout, stderr)
	}
	return cr.StartAttached(ctx, r.Name, r.Interactive, stdin, stdout, stderr)
}

// createUnder is the half the two helpers share: for a spec with a runtime,
// r is adjusted for the engine (underEngine), created and checked, and the
// Creator to start it with is returned; nil says the caller runs r itself,
// because it names no runtime.
func createUnder(ctx context.Context, d Client, r *RunSpec, class string) (Creator, error) {
	if r.Runtime == "" {
		return nil, nil
	}
	cr, ok := d.(Creator)
	if !ok {
		return nil, fmt.Errorf("the %s must run under %s, and this engine client cannot read a container's runtime back to check it", class, r.Runtime)
	}
	adjusted, err := underEngine(ctx, d, *r)
	if err != nil {
		return nil, err
	}
	*r = adjusted
	if err := createChecked(ctx, d, cr, *r, class); err != nil {
		return nil, err
	}
	return cr, nil
}

// createChecked creates r and reads its runtime back; on a mismatch the
// container goes, under a context that survives the caller's, and the
// mismatch is the error.
func createChecked(ctx context.Context, d Client, cr Creator, r RunSpec, class string) error {
	if r.Name == "" {
		return fmt.Errorf("the %s runs under %s and needs a name to be checked by", class, r.Runtime)
	}
	if err := cr.Create(ctx, r); err != nil {
		return err
	}
	got, err := cr.ContainerRuntime(ctx, r.Name)
	if err != nil {
		_ = d.RemoveForce(context.WithoutCancel(ctx), r.Name)
		return fmt.Errorf("read the runtime the %s (%s) was created with: %w", class, r.Name, err)
	}
	if got != r.Runtime {
		_ = d.RemoveForce(context.WithoutCancel(ctx), r.Name)
		return &RuntimeMismatch{Class: class, Name: r.Name, Want: r.Runtime, Got: got}
	}
	return nil
}

// underEngine is r as the engine must be given it to run under r.Runtime:
// on podman on an SELinux host with its label disabled (SandboxUnlabelled).
func underEngine(ctx context.Context, d Client, r RunSpec) (RunSpec, error) {
	e, err := DescribeEngine(ctx, d)
	if err != nil {
		return r, err
	}
	if e.SandboxUnlabelled(r.Runtime) {
		r.DisableSELinuxLabel = true
	}
	return r, nil
}

// selinuxRoot exists exactly when this host runs SELinux.
const selinuxRoot = "/sys/fs/selinux"

// selinuxActive says whether this host runs SELinux; a variable so a test
// can be any host.
var selinuxActive = func() bool {
	_, err := os.Stat(selinuxRoot)
	return err == nil
}

// SandboxUnlabelled says whether a container under runtime on engine e must
// be created with its SELinux label disabled: on podman on an SELinux
// host, where gVisor refuses to start a labelled container ("SELinux is not
// supported"). The runtime's own isolation stands in for the label's; the
// receipt says so. Docker Engine labels nothing under gVisor itself, and a
// host without SELinux has no label to disable.
func (e Engine) SandboxUnlabelled(runtime string) bool {
	return runtime != "" && e.Kind == KindPodman && selinuxActive()
}

// Create implements Creator: `docker create` with `run`'s arguments.
func (c *CLI) Create(ctx context.Context, r RunSpec) error {
	if r.Name == "" {
		return errors.New("create: a container to be started later needs a name")
	}
	named, err := c.podmanPrepare(ctx, r)
	if err != nil {
		return err
	}
	args := c.dialectArgs(false, r, named)
	args[0] = "create"
	cmd := c.command(ctx, args...)
	cmd.Env = append(cmd.Env, r.envPairsFor(c.Podman)...)
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &errb
	if err := cmd.Run(); err != nil {
		c.dropVolumes(ctx, named)
		return fmt.Errorf("%s create %s: %w: %s", c.name(), r.Name, err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// Start implements Creator.
func (c *CLI) Start(ctx context.Context, name string) error {
	if _, err := c.run(ctx, "start", name); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	return nil
}

// StartAttached implements Creator: `docker start -a` (and -i for an
// attached stdin) streams the container's stdio and exits with its code,
// as `run` does, and a container created with --rm goes as it would have.
func (c *CLI) StartAttached(ctx context.Context, name string, interactive bool, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	args := []string{"start", "-a"}
	if interactive {
		args = append(args, "-i")
	}
	cmd := c.command(ctx, append(args, name)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if asExitError(err, &exitErr) {
		code := exitErr.ExitCode()
		if code >= 125 && ctx.Err() == nil {
			return code, fmt.Errorf("%s start %s failed (exit %d)", c.name(), name, code)
		}
		return code, nil
	}
	return -1, fmt.Errorf("%s start %s: %w", c.name(), name, err)
}

// ContainerRuntime implements Creator. podman's own CLI names the runtime
// it created the container with (OCIRuntime); the docker CLI reads the
// daemon's HostConfig.Runtime, which Docker Engine fills with the name it
// applied and podman's Docker-compatible API with the constant "oci",
// whatever it ran: through that route the runtime cannot be read, and
// the check that reads it refuses the run.
func (c *CLI) ContainerRuntime(ctx context.Context, name string) (string, error) {
	c.resolve(ctx)
	format := "{{.HostConfig.Runtime}}"
	if c.Podman {
		format = "{{.OCIRuntime}}"
	}
	out, err := c.run(ctx, "inspect", "--format", format, name)
	if err != nil {
		return "", fmt.Errorf("%s inspect %s: %w", c.name(), name, err)
	}
	return strings.TrimSpace(out), nil
}

// Create implements Creator for Fake: recorded as "Create", the spec kept
// for Start, StartAttached and ContainerRuntime.
func (f *Fake) Create(_ context.Context, r RunSpec) error {
	f.record("Create", r.Name, r)
	for prefix, err := range f.FailDetached {
		if strings.Contains(r.Name, prefix) {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.created == nil {
		f.created = map[string]RunSpec{}
	}
	f.created[r.Name] = r
	return nil
}

// Start implements Creator for Fake: recorded as "Start", with OnDetached
// run for the created spec as RunDetached would.
func (f *Fake) Start(_ context.Context, name string) error {
	f.mu.Lock()
	r, ok := f.created[name]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("fake docker: start %s: no such container", name)
	}
	f.record("Start", name, r)
	if f.OnDetached != nil {
		f.OnDetached(r)
	}
	return nil
}

// StartAttached implements Creator for Fake: recorded as "StartAttached",
// then played as RunForeground plays the created spec.
func (f *Fake) StartAttached(ctx context.Context, name string, _ bool, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f.mu.Lock()
	r, ok := f.created[name]
	f.mu.Unlock()
	if !ok {
		return -1, fmt.Errorf("fake docker: start %s: no such container", name)
	}
	f.record("StartAttached", name, r)
	return f.play(ctx, r, stdin, stdout, stderr)
}

// ContainerRuntime implements Creator for Fake: the runtime the created
// spec asked for, or DroppedRuntime for every container when DropRuntime
// is set, the way an engine that ignores the request answers.
func (f *Fake) ContainerRuntime(_ context.Context, name string) (string, error) {
	f.record("ContainerRuntime", name, RunSpec{})
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.created[name]
	if !ok {
		return "", fmt.Errorf("fake docker: inspect %s: no such container", name)
	}
	if f.DropRuntime {
		return f.DroppedRuntime, nil
	}
	return r.Runtime, nil
}
