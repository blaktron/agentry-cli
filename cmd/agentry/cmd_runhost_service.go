package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage"
)

// The runhost's service, on the server: `agentry runhost service` is the
// systemd user service of the agentry account, and `service prepare` is the
// install's last step, run as that account.

func newRunhostServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "service",
		Short:  "The runhost's service on the server (run by systemd)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRunhostService(cmd)
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "prepare",
		Short: "Render the runhost's policy for gVisor and ready the images runs need",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRunhostPrepare(cmd)
		},
	})
	return cmd
}

// runhostStatus is the status verb's answer.
type runhostStatus struct {
	Name           string          `json:"name"`
	Version        string          `json:"version"`
	Arch           string          `json:"arch"`
	OS             string          `json:"os"`
	GVisor         string          `json:"gvisor"`
	Owner          runhost.Owner   `json:"owner"`
	UnsafeMode     bool            `json:"unsafeMode"`
	Runtime        string          `json:"runtime"`
	GatewayRuntime string          `json:"gatewayRuntime"`
	Devices        []runhostDevice `json:"devices"`
	Model          string          `json:"model"`
	Capabilities   map[string]bool `json:"capabilities"`
}

type runhostDevice struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	PairedAt string `json:"pairedAt"`
}

func runRunhostService(cmd *cobra.Command) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	logger := log.New(os.Stderr, "", 0)
	s := &runhost.Service{
		Socket: runhost.ServeSocket(),
		Log:    logger.Printf,
		Handlers: map[string]runhost.Handler{
			"version": func(_ context.Context, _ runhost.Request, _ net.Conn, fw *runhost.FrameWriter) (int, error) {
				return 0, fw.Result(map[string]string{"version": version.Version, "arch": runtime.GOARCH})
			},
			"status": func(_ context.Context, _ runhost.Request, _ net.Conn, fw *runhost.FrameWriter) (int, error) {
				st, err := readRunhostStatus()
				if err != nil {
					return 1, err
				}
				return 0, fw.Result(st)
			},
			"doctor": func(ctx context.Context, _ runhost.Request, _ net.Conn, fw *runhost.FrameWriter) (int, error) {
				rep := runhostDoctor(ctx)
				if err := fw.Result(rep); err != nil {
					return 1, err
				}
				if n := rep.Failed(); n > 0 {
					return 1, fmt.Errorf("%d check(s) failed", n)
				}
				return 0, nil
			},
		},
	}
	l, err := s.Listen()
	if err != nil {
		return err
	}
	logger.Printf("agentry runhost service %s listening on %s", version.Version, s.Socket)
	return s.Serve(ctx, l)
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path) //nolint:gosec // the service's own state
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func readRunhostStatus() (runhostStatus, error) {
	var cfg runhost.ServerConfig
	if err := readJSONFile(runhost.ConfigFile, &cfg); err != nil {
		return runhostStatus{}, fmt.Errorf("the runhost's configuration: %w", err)
	}
	var devs runhost.Devices
	if err := readJSONFile(runhost.DevicesFile, &devs); err != nil {
		return runhostStatus{}, fmt.Errorf("the runhost's devices: %w", err)
	}
	st := runhostStatus{
		Name: cfg.Name, Version: version.Version, Arch: runtime.GOARCH, OS: osPretty(), GVisor: cfg.GVisor,
		Owner: cfg.Owner, UnsafeMode: cfg.UnsafeMode, Devices: []runhostDevice{},
		Capabilities: map[string]bool{"runs": false, "sessions": false, "schedules": false},
	}
	for _, d := range devs.Devices {
		st.Devices = append(st.Devices, runhostDevice{ID: d.ID, Label: d.Label, PairedAt: d.PairedAt})
	}
	if path, err := policy.DefaultPath(); err == nil {
		if pol, _, err := policy.Load(path); err == nil {
			st.Runtime, st.GatewayRuntime = pol.Docker.Runtime, pol.Docker.GatewayRuntime
			if pol.Model.Provider != "" {
				st.Model = pol.Model.Provider + "/" + pol.Model.Name
			}
		}
	}
	return st, nil
}

func osPretty() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return runtime.GOOS
}

// runhostDoctorChecks are the CLI doctor's checks a runhost reports: its
// policy, its engine and runtimes, and its images. The model and the
// registry are a run's own concerns and are left to it.
func runhostDoctorCheck(name string) bool {
	return name == "policy" || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "image:")
}

// runhostDoctor checks the runhost: the CLI doctor's engine and image checks,
// then that every container class runs under gVisor as the runhost's
// registrations make it.
func runhostDoctor(ctx context.Context) *doctorReport {
	rep := &doctorReport{}
	self, err := os.Executable()
	if err != nil {
		rep.failFix("runhost:cli", "reinstall the runhost with `agentry runhost add`", "the service cannot find its own CLI: %v", err)
		return rep
	}
	out, _ := exec.CommandContext(ctx, self, "doctor", "--json").Output() //nolint:gosec // the service's own binary
	var cli doctorReport
	if err := json.Unmarshal(out, &cli); err != nil {
		rep.failFix("runhost:cli", "run `agentry runhost add` again", "the CLI's doctor gave no report: %v", err)
	}
	for _, c := range cli.Checks {
		if runhostDoctorCheck(c.Name) {
			rep.Checks = append(rep.Checks, c)
		}
	}
	path, _ := policy.DefaultPath()
	pol, _, perr := policy.Load(path)
	switch {
	case perr != nil:
		rep.failFix("runhost:gvisor", "run `agentry runhost add` again", "the policy is not readable: %v", perr)
	case pol.Docker.Runtime != runhost.RunscRuntime || pol.Docker.GatewayRuntime != runhost.RunscUDSRuntime:
		rep.failFix("runhost:gvisor", "run `agentry runhost add` again to render the policy", "the policy runs containers under %q and the gateway under %q; a runhost runs them under %s and %s", pol.Docker.Runtime, pol.Docker.GatewayRuntime, runhost.RunscRuntime, runhost.RunscUDSRuntime)
	default:
		rep.ok("runhost:gvisor", "every container runs under %s, the gateway under %s", runhost.RunscRuntime, runhost.RunscUDSRuntime)
	}
	d := newDockerClient()
	if table, err := d.Runtimes(ctx); err == nil {
		for _, name := range []string{runhost.RunscRuntime, runhost.RunscUDSRuntime} {
			rt, ok := table[name]
			switch {
			case !ok:
				rep.failFix("runhost:"+name, "run `agentry runhost add` again", "the Docker daemon does not register %s", name)
			case rt.Path != runhost.RunscPath:
				rep.failFix("runhost:"+name, "run `agentry runhost add` again", "%s is registered at %s, not the runhost's own %s", name, rt.Path, runhost.RunscPath)
			default:
				rep.ok("runhost:"+name, "registered at %s", rt.Path)
			}
		}
	}
	if v, err := exec.CommandContext(ctx, runhost.RunscPath, "--version").Output(); err != nil { //nolint:gosec // the runhost's own runsc
		rep.failFix("runhost:runsc", "run `agentry runhost add` again", "runsc does not run: %v", err)
	} else {
		rep.ok("runhost:runsc", "%s", firstLineOf(string(v)))
	}
	return rep
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// runRunhostPrepare renders the runhost's policy (every container under
// gVisor, the host's own name servers, no model) and readies the images.
func runRunhostPrepare(cmd *cobra.Command) error {
	home, err := runnerHome()
	if err != nil {
		return err
	}
	path, err := policy.DefaultPath()
	if err != nil {
		return err
	}
	pol, _, err := policy.EnsureDefault(path)
	if err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}
	pol.Docker.Runtime = runhost.RunscRuntime
	pol.Docker.GatewayRuntime = runhost.RunscUDSRuntime
	if len(pol.Docker.Resolvers) == 0 {
		pol.Docker.Resolvers = upstreamResolvers()
	}
	if err := pol.Validate(); err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}
	if err := policy.Save(path, pol); err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}
	stages := newStageLog(cmd)
	if err := gateway.EnsureInstalled(cmd.Context(), newDockerClient(), pol,
		pol.RunnerImageTag(runnerimage.ContentVersion()), homeImageDir(home), stages, true); err != nil {
		stages.Fail()
		return engine.Classify(engine.ExitGateway, err)
	}
	stages.Close()
	return nil
}

// upstreamResolvers are the server's own upstream name servers, never a
// loopback stub: systemd-resolved's list of them when it runs, else
// resolv.conf's.
func upstreamResolvers() []string {
	for _, f := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		b, err := os.ReadFile(f) //nolint:gosec // the host's resolver files
		if err != nil {
			continue
		}
		var out []string
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 2 || fields[0] != "nameserver" {
				continue
			}
			a, err := netip.ParseAddr(fields[1])
			if err != nil || !a.Is4() || a.IsLoopback() || a.IsUnspecified() || a.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, a.String())
			if len(out) == 3 {
				break
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}
