// Fake is the test double for Client: it records every call in order and
// lets tests script failures and the agent container's behavior. The
// engine's whole lifecycle — ordering, teardown-on-every-path, env/mount
// resolution — is asserted against this recorder, so the
// execute path is proven without a docker daemon.
package dockerx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"slices"
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
	// Exec, when set, answers ExecStream (Execer) for a running
	// container: the test plays the command's side of its stdio.
	Exec func(ctx context.Context, container string, argv []string, stdin io.Reader, stdout io.Writer) (int, error)
	// RuntimeNames is what Runtimes answers when RuntimeTable is nil, each
	// name with an empty registration; nil answers the daemon's usual pair
	// ("io.containerd.runc.v2", "runc").
	RuntimeNames []string
	// RuntimeTable, when set, is what Runtimes answers, each name with
	// its binary and arguments: what a gateway registration is checked
	// against.
	RuntimeTable map[string]Runtime
	// RuntimesErr fails Runtimes.
	RuntimesErr error
	// IPs answers ContainerIP by container name; a name with no entry gets
	// a made-up address, so a test need not script every container.
	IPs map[string]string
	// ExistingImages marks refs ImageExists reports true for.
	ExistingImages map[string]bool
	// Labels answers ImageLabel by ref, then key.
	Labels map[string]map[string]string
	// Arch is what ServerArch answers; "" answers amd64.
	Arch string
	// ImageFiles answers ImageFile by "ref:path"; a missing entry is an error.
	ImageFiles map[string]string
	// BuildArgs records the build args of the last Build of each tag.
	BuildArgs map[string]map[string]string
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
	// Containers is what PSLabels lists: each is returned when it carries
	// the asked label ("k=v", or "k" for any value), with the asked keys.
	Containers []Labeled
	// Networks is the docker host's networks with their labels:
	// NetworkCreate adds one, NetworkRemove takes it away, and
	// NetworksByLabel lists them as PSLabels lists Containers.
	Networks []Labeled
	// Volumes is the docker host's labelled volumes: VolumeCreate with
	// labels adds one, VolumeRemove takes it away, and VolumesByLabel lists
	// them beside LeftoverVolumes.
	Volumes []Labeled
	// OnDetached, when set, runs with the spec of every detached container
	// as it starts — how a test gives a container side effects, such as
	// the relays a gateway creates.
	OnDetached func(r RunSpec)
	// OnForeground, when set, runs with the spec of every foreground
	// container as it starts — how a test makes something happen once the
	// agent is running, such as a relay the gateway starts late.
	OnForeground func(r RunSpec)
	// EngineInfo, when set, is what Engine answers; nil answers
	// LocalDockerEngine. EngineErr fails Engine.
	EngineInfo *Engine
	EngineErr  error
	// DropRuntime makes ContainerRuntime answer DroppedRuntime for every
	// container, whatever its spec asked for: an engine that takes a
	// runtime and does not apply it. "" is the engine's default.
	DropRuntime    bool
	DroppedRuntime string
	// PodmanService, when set, answers PodmanServiceCommand (PodmanServicer)
	// with the command a test wants run as the run's podman API service;
	// nil says this client has none.
	PodmanService func(runtime, socket string) *exec.Cmd
	// created are the specs Create was given, by name, for Start,
	// StartAttached and ContainerRuntime.
	created map[string]RunSpec
}

// PodmanServiceCommand implements PodmanServicer for Fake.
func (f *Fake) PodmanServiceCommand(_ context.Context, runtime, socket string) (*exec.Cmd, error) {
	f.record("PodmanServiceCommand", runtime+"@"+socket, RunSpec{})
	if f.PodmanService == nil {
		return nil, errors.New("fake docker: no podman API service")
	}
	return f.PodmanService(runtime, socket), nil
}

// Engine implements EngineDescriber. It is not recorded: a run reads it before
// it touches the daemon, and the call sequences tests assert are the daemon's.
func (f *Fake) Engine(context.Context) (Engine, error) {
	if f.EngineErr != nil {
		return Engine{}, f.EngineErr
	}
	if f.EngineInfo != nil {
		return *f.EngineInfo, nil
	}
	return LocalDockerEngine(), nil
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
	// Print, when set, runs after Before and its answer is written to the
	// caller's stdout ahead of Stdout: what a container prints of the work
	// Before played.
	Print func(r RunSpec) string
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

// FindRun returns the first Run* or Create call whose name contains
// substr: a container checked for its runtime is created, then started.
func (f *Fake) FindRun(substr string) (RunSpec, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.Calls {
		if (strings.HasPrefix(c.Op, "Run") || c.Op == "Create") && strings.Contains(c.Run.Name, substr) {
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

// Runtimes implements Client from RuntimeTable, else RuntimeNames.
func (f *Fake) Runtimes(context.Context) (map[string]Runtime, error) {
	f.record("Runtimes", "", RunSpec{})
	if f.RuntimesErr != nil {
		return nil, f.RuntimesErr
	}
	if f.RuntimeTable != nil {
		return maps.Clone(f.RuntimeTable), nil
	}
	names := f.RuntimeNames
	if names == nil {
		names = []string{"io.containerd.runc.v2", "runc"}
	}
	table := make(map[string]Runtime, len(names))
	for _, n := range names {
		table[n] = Runtime{}
	}
	return table, nil
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

// ServerArch implements Client from Arch.
func (f *Fake) ServerArch(context.Context) (string, error) {
	f.record("ServerArch", "", RunSpec{})
	if f.Arch == "" {
		return "amd64", nil
	}
	return f.Arch, nil
}

// ImageLabel implements Client from Labels.
func (f *Fake) ImageLabel(_ context.Context, ref, key string) (string, error) {
	f.record("ImageLabel", ref+":"+key, RunSpec{})
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Labels[ref][key], nil
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
func (f *Fake) Build(_ context.Context, dir, tag string, args map[string]string) error {
	f.record("Build", tag+"<-"+dir, RunSpec{})
	f.mu.Lock()
	if f.BuildArgs == nil {
		f.BuildArgs = map[string]map[string]string{}
	}
	f.BuildArgs[tag] = args
	f.mu.Unlock()
	return f.FailBuild[tag]
}

// NetworkCreate implements Client.
func (f *Fake) NetworkCreate(_ context.Context, name string, internal bool, labels map[string]string) error {
	f.record("NetworkCreate", fmt.Sprintf("%s(internal=%v)", name, internal), RunSpec{})
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Networks = append(f.Networks, Labeled{Name: name, Labels: maps.Clone(labels)})
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Networks = slices.DeleteFunc(f.Networks, func(n Labeled) bool { return n.Name == name })
	return nil
}

// NetworksByLabel implements Client.
func (f *Fake) NetworksByLabel(_ context.Context, label string) ([]string, error) {
	f.record("NetworksByLabel", label, RunSpec{})
	k, v, exact := strings.Cut(label, "=")
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, n := range f.Networks {
		if got, has := n.Labels[k]; has && (!exact || got == v) {
			names = append(names, n.Name)
		}
	}
	return names, nil
}

// NetworkState is a copy of the networks the fake holds now, each with
// its labels.
func (f *Fake) NetworkState() []Labeled {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Labeled, len(f.Networks))
	for i, n := range f.Networks {
		out[i] = Labeled{Name: n.Name, Labels: maps.Clone(n.Labels)}
	}
	return out
}

// VolumeCreate implements Client. A labelled volume is kept in Volumes.
func (f *Fake) VolumeCreate(_ context.Context, name string, labels map[string]string) error {
	f.record("VolumeCreate", name, RunSpec{})
	if len(labels) > 0 {
		f.mu.Lock()
		f.Volumes = append(f.Volumes, Labeled{Name: name, Labels: maps.Clone(labels)})
		f.mu.Unlock()
	}
	return nil
}

// VolumeRemove implements Client.
func (f *Fake) VolumeRemove(_ context.Context, name string) error {
	f.record("VolumeRemove", name, RunSpec{})
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Volumes = slices.DeleteFunc(f.Volumes, func(v Labeled) bool { return v.Name == name })
	return nil
}

// VolumesByLabel implements Client: LeftoverVolumes, and the Volumes that
// carry the label.
func (f *Fake) VolumesByLabel(_ context.Context, label string) ([]string, error) {
	f.record("VolumesByLabel", label, RunSpec{})
	k, v, exact := strings.Cut(label, "=")
	f.mu.Lock()
	defer f.mu.Unlock()
	names := slices.Clone(f.LeftoverVolumes)
	for _, vol := range f.Volumes {
		if got, has := vol.Labels[k]; has && (!exact || got == v) {
			names = append(names, vol.Name)
		}
	}
	return names, nil
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
	return f.play(ctx, r, stdin, stdout, stderr)
}

// play is the scripted behaviour of a foreground container, whether it was
// run or created and then started.
func (f *Fake) play(ctx context.Context, r RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if f.OnForeground != nil {
		f.OnForeground(r)
	}
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
	if fg.Print != nil {
		_, _ = io.WriteString(stdout, fg.Print(r))
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

// PSLabels implements Client.
func (f *Fake) PSLabels(_ context.Context, label string, keys []string) ([]Labeled, error) {
	f.record("PSLabels", label, RunSpec{})
	k, v, exact := strings.Cut(label, "=")
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Labeled
	for _, c := range f.Containers {
		got, has := c.Labels[k]
		if !has || (exact && got != v) {
			continue
		}
		l := Labeled{Name: c.Name, Labels: map[string]string{}}
		for _, key := range keys {
			l.Labels[key] = c.Labels[key]
		}
		out = append(out, l)
	}
	return out, nil
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
