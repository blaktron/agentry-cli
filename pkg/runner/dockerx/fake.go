// Fake is the test double for Client: it records every call in order and
// lets tests script failures and the agent container's behavior. The
// engine's whole lifecycle — ordering, teardown-on-every-path, env/mount
// resolution — is asserted against this recorder (RUNNER-DEVIATIONS DR-7:
// no docker on the framework author's machine, so this coverage is the
// execute path's proof until the acceptance scripts run on a real engine).
package dockerx

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// FakeCall is one recorded client call.
type FakeCall struct {
	// Op is the method name (e.g. "RunDetached").
	Op string
	// Arg is the primary argument (image ref, network or container name).
	Arg string
	// Run is the RunSpec for Run* ops.
	Run RunSpec
}

// Fake implements Client.
type Fake struct {
	mu    sync.Mutex
	Calls []FakeCall

	// AvailableErr fails Available.
	AvailableErr error
	// RuntimeNames is what Runtimes answers; nil answers the daemon's
	// usual pair ("io.containerd.runc.v2", "runc"). RuntimesErr fails it.
	RuntimeNames []string
	RuntimesErr  error
	// IPs answers ContainerIP by container name; a name with no entry gets
	// a made-up address, so a test need not script every container.
	IPs map[string]string
	// ExistingImages marks refs ImageExists reports true for.
	ExistingImages map[string]bool
	// ImageFiles answers ImageFile by "ref:path"; a missing entry is an error.
	ImageFiles map[string]string
	// FailPull/FailBuild/FailDetached script failures by ref/name.
	FailPull     map[string]error
	FailBuild    map[string]error
	FailDetached map[string]error
	// Foreground scripts RunForeground behavior keyed by container name
	// prefix; nil behavior = exit 0, no output.
	Foreground map[string]*FakeForeground
	// LogsByName scripts Logs.
	LogsByName map[string]string
	// Ports answers Port by "name:port" or by container name; a published container with
	// no entry gets 41000 + its container port modulo 1000.
	Ports map[string]int
	// PortFailures makes that many Port calls fail before one succeeds.
	PortFailures int
	// Leftover names PSByLabel returns (leak-check tests); LeftoverVolumes
	// what VolumesByLabel returns.
	Leftover        []string
	LeftoverVolumes []string
	// ByLabel, when it has the label, is what PSByLabel answers for it
	// instead of Leftover.
	ByLabel map[string][]string
	// OnDetached, when set, runs with the spec of every detached container
	// as it starts — how a test gives a container side effects, such as
	// the relays a gateway creates.
	OnDetached func(r RunSpec)
}

// FakeForeground scripts one foreground container.
type FakeForeground struct {
	// Stdout is written to the caller's stdout writer, Stderr to its
	// stderr writer.
	Stdout string
	Stderr string
	// WantStdin, when set, asserts what the container received on stdin
	// (recorded into GotStdin for the test to inspect).
	GotStdin string
	// ExitCode is returned.
	ExitCode int
	// Err is returned as a docker-level failure.
	Err error
	// Block, when set, waits for ctx cancellation and then returns
	// (exit 137) — how tests exercise timeout/budget kills.
	Block bool
	// Before, when set, runs with the spec before the scripted result is
	// produced — how tests leave files in a mounted directory the way a
	// real builder would.
	Before func(r RunSpec)
	// Handler, when set, plays the container itself on the attached
	// streams (an interactive harness for session tests); the scripted
	// fields above are ignored.
	Handler func(ctx context.Context, r RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error)
}

// record appends a call.
func (f *Fake) record(op, arg string, run RunSpec) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{Op: op, Arg: arg, Run: run})
}

// Ops lists recorded operation names in order, "Op:Arg" formatted.
func (f *Fake) Ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.Calls))
	for i, c := range f.Calls {
		out[i] = c.Op + ":" + c.Arg
	}
	return out
}

// FindRun returns the first Run* call whose name contains substr.
func (f *Fake) FindRun(substr string) (RunSpec, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.Calls {
		if strings.HasPrefix(c.Op, "Run") && strings.Contains(c.Run.Name, substr) {
			return c.Run, true
		}
	}
	return RunSpec{}, false
}

// Available implements Client.
func (f *Fake) Available(context.Context) error {
	f.record("Available", "", RunSpec{})
	return f.AvailableErr
}

// Runtimes implements Client from RuntimeNames.
func (f *Fake) Runtimes(context.Context) ([]string, error) {
	f.record("Runtimes", "", RunSpec{})
	if f.RuntimesErr != nil {
		return nil, f.RuntimesErr
	}
	if f.RuntimeNames == nil {
		return []string{"io.containerd.runc.v2", "runc"}, nil
	}
	return append([]string(nil), f.RuntimeNames...), nil
}

// ContainerIP implements Client from IPs.
func (f *Fake) ContainerIP(_ context.Context, name, network string) (string, error) {
	f.record("ContainerIP", name+"@"+network, RunSpec{})
	f.mu.Lock()
	defer f.mu.Unlock()
	if ip, ok := f.IPs[name]; ok {
		return ip, nil
	}
	return fmt.Sprintf("172.30.0.%d", 10+len(name)%200), nil
}

// ImageFile implements Client from ImageFiles.
func (f *Fake) ImageFile(_ context.Context, ref, path string) ([]byte, error) {
	f.record("ImageFile", ref+":"+path, RunSpec{})
	if b, ok := f.ImageFiles[ref+":"+path]; ok {
		return []byte(b), nil
	}
	return nil, fmt.Errorf("fake docker: %s has no %s", ref, path)
}

// ImageExists implements Client.
func (f *Fake) ImageExists(_ context.Context, ref string) (bool, error) {
	f.record("ImageExists", ref, RunSpec{})
	return f.ExistingImages[ref], nil
}

// Pull implements Client.
func (f *Fake) Pull(_ context.Context, ref string) error {
	f.record("Pull", ref, RunSpec{})
	return f.FailPull[ref]
}

// Build implements Client.
func (f *Fake) Build(_ context.Context, dir, tag string, _ map[string]string) error {
	f.record("Build", tag+"<-"+dir, RunSpec{})
	return f.FailBuild[tag]
}

// NetworkCreate implements Client.
func (f *Fake) NetworkCreate(_ context.Context, name string, internal bool, _ map[string]string) error {
	f.record("NetworkCreate", fmt.Sprintf("%s(internal=%v)", name, internal), RunSpec{})
	return nil
}

// NetworkConnect implements Client.
func (f *Fake) NetworkConnect(_ context.Context, network, container string) error {
	f.record("NetworkConnect", network+"<-"+container, RunSpec{})
	return nil
}

// NetworkRemove implements Client.
func (f *Fake) NetworkRemove(_ context.Context, name string) error {
	f.record("NetworkRemove", name, RunSpec{})
	return nil
}

// VolumeCreate implements Client.
func (f *Fake) VolumeCreate(_ context.Context, name string, _ map[string]string) error {
	f.record("VolumeCreate", name, RunSpec{})
	return nil
}

// VolumeRemove implements Client.
func (f *Fake) VolumeRemove(_ context.Context, name string) error {
	f.record("VolumeRemove", name, RunSpec{})
	return nil
}

// VolumesByLabel implements Client.
func (f *Fake) VolumesByLabel(_ context.Context, label string) ([]string, error) {
	f.record("VolumesByLabel", label, RunSpec{})
	return f.LeftoverVolumes, nil
}

// RunDetached implements Client.
func (f *Fake) RunDetached(_ context.Context, r RunSpec) error {
	f.record("RunDetached", r.Name, r)
	for prefix, err := range f.FailDetached {
		if strings.Contains(r.Name, prefix) {
			return err
		}
	}
	if f.OnDetached != nil {
		f.OnDetached(r)
	}
	return nil
}

// RunForeground implements Client.
func (f *Fake) RunForeground(ctx context.Context, r RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f.record("RunForeground", r.Name, r)
	var fg *FakeForeground
	f.mu.Lock()
	for prefix, b := range f.Foreground {
		if strings.Contains(r.Name, prefix) {
			fg = b
			break
		}
	}
	f.mu.Unlock()
	if fg == nil {
		return 0, nil
	}
	if fg.Before != nil {
		fg.Before(r)
	}
	if fg.Handler != nil {
		return fg.Handler(ctx, r, stdin, stdout, stderr)
	}
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		f.mu.Lock()
		fg.GotStdin = string(b)
		f.mu.Unlock()
	}
	if fg.Stdout != "" {
		_, _ = io.WriteString(stdout, fg.Stdout)
	}
	if fg.Stderr != "" && stderr != nil {
		_, _ = io.WriteString(stderr, fg.Stderr)
	}
	if fg.Block {
		<-ctx.Done()
		return 137, nil
	}
	return fg.ExitCode, fg.Err
}

// Stop implements Client.
func (f *Fake) Stop(_ context.Context, name string, _ int) error {
	f.record("Stop", name, RunSpec{})
	return nil
}

// RemoveForce implements Client.
func (f *Fake) RemoveForce(_ context.Context, name string) error {
	f.record("RemoveForce", name, RunSpec{})
	return nil
}

// PSByLabel implements Client.
func (f *Fake) PSByLabel(_ context.Context, label string) ([]string, error) {
	f.record("PSByLabel", label, RunSpec{})
	if names, ok := f.ByLabel[label]; ok {
		return names, nil
	}
	return f.Leftover, nil
}

// Logs implements Client.
func (f *Fake) Logs(_ context.Context, name string, _ int) (string, error) {
	f.record("Logs", name, RunSpec{})
	return f.LogsByName[name], nil
}

// Port implements Client.
func (f *Fake) Port(_ context.Context, name string, containerPort int) (int, error) {
	f.record("Port", name, RunSpec{})
	// PortFailures reproduces the real daemon's timing: a container on an
	// internal network has no port mapping until docker has finished
	// programming the one the bridge connect asked for, so the first reads
	// legitimately find nothing.
	f.mu.Lock()
	if f.PortFailures > 0 {
		f.PortFailures--
		f.mu.Unlock()
		return 0, fmt.Errorf("fake docker: no public port for %d/tcp in %s yet", containerPort, name)
	}
	f.mu.Unlock()
	// "name:port" wins over "name", so a container publishing two ports
	// can answer each; the plain key stays for single-port containers.
	if p, ok := f.Ports[fmt.Sprintf("%s:%d", name, containerPort)]; ok {
		return p, nil
	}
	if p, ok := f.Ports[name]; ok {
		return p, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.Calls {
		if c.Run.Name == name && len(c.Run.Publish) > 0 {
			return 41000 + containerPort%1000, nil
		}
	}
	return 0, fmt.Errorf("fake docker: %s publishes no port", name)
}
