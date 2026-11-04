package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/clirelease"
	"github.com/blaktron/agentry-cli/pkg/runhost/remote"
	"github.com/blaktron/agentry-cli/pkg/runhost/setup"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// The service's side of a runhost's update and of a device leaving it. The
// CLI and runsc live in the agentry account's own directory, so the service
// replaces them itself: only with a release whose signature verifies against
// the update-signing keys this CLI carries, built for this server, and no
// older than the one it runs.

var (
	runhostBinDir         = runhost.BinDir
	runhostCLIDir         = filepath.Join(runhost.Home, "cli")
	runhostGVisorDir      = filepath.Join(runhost.Home, "gvisor")
	runhostAuthorizedKeys = runhost.AuthorizedKeys
	runhostConfigFile     = runhost.ConfigFile
	runhostReleaseKeys    = clirelease.Carried
	runhostRunningVersion = func() string { return version.Version }
)

// runhostRestart is the restart an installed update asked for: the service
// takes no new job from then on, and stops once its running jobs finish.
var runhostRestart = struct {
	sync.Mutex
	to    string
	asked chan struct{}
}{asked: make(chan struct{}, 1)}

func askRunhostRestart(to string) {
	runhostRestart.Lock()
	runhostRestart.to = to
	runhostRestart.Unlock()
	select {
	case runhostRestart.asked <- struct{}{}:
	default:
	}
}

// runhostRestarting is the CLI version the service waits to restart into,
// or "".
func runhostRestarting() string {
	runhostRestart.Lock()
	defer runhostRestart.Unlock()
	return runhostRestart.to
}

// runningRunhostJobs is how many jobs are running under the service.
func runningRunhostJobs() int {
	runhostLiveJobs.Lock()
	ids := make([]string, 0, len(runhostLiveJobs.m))
	for id := range runhostLiveJobs.m {
		ids = append(ids, id)
	}
	runhostLiveJobs.Unlock()
	n := 0
	for _, id := range ids {
		if runningJob(id) != nil {
			n++
		}
	}
	return n
}

// awaitRunhostRestart stops the service through stop once a restart is
// asked for and no job runs, and reports that it did.
func awaitRunhostRestart(ctx context.Context, stop func(), logf func(string, ...any)) <-chan bool {
	done := make(chan bool, 1)
	go func() {
		select {
		case <-ctx.Done():
			done <- false
			return
		case <-runhostRestart.asked:
		}
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for runningRunhostJobs() > 0 {
			select {
			case <-ctx.Done():
				done <- false
				return
			case <-t.C:
			}
		}
		logf("restarting into CLI %s", runhostRestarting())
		done <- true
		stop()
	}()
	return done
}

func runhostUpdateVerb(ctx context.Context, req runhost.Request, in io.Reader, fw *runhost.FrameWriter) (int, error) {
	if len(req.Args) != 2 {
		return engine.ExitUsage, errors.New("update takes cli <version> or gvisor <release>")
	}
	br := bufio.NewReader(in)
	switch req.Args[0] {
	case "cli":
		return runhostUpdateCLI(ctx, req.Args[1], br, fw)
	case "gvisor":
		return runhostUpdateGVisor(ctx, req.Args[1], br, fw)
	}
	return engine.ExitUsage, fmt.Errorf("update does not take %q: cli <version> or gvisor <release>", req.Args[0])
}

// readUpdate reads an update's header line and the bytes it says follow.
func readUpdate(br *bufio.Reader) (runhost.UpdateHeader, []byte, error) {
	var h runhost.UpdateHeader
	line, err := br.ReadBytes('\n')
	if err != nil {
		return h, nil, fmt.Errorf("the update's header: %w", err)
	}
	if err := json.Unmarshal(line, &h); err != nil {
		return h, nil, fmt.Errorf("the update's header: %w", err)
	}
	if h.Size <= 0 || h.Size > runhost.MaxUpdate {
		return h, nil, fmt.Errorf("the update says it is %d bytes", h.Size)
	}
	b := make([]byte, h.Size)
	if _, err := io.ReadFull(br, b); err != nil {
		return h, nil, fmt.Errorf("the update's bytes: %w", err)
	}
	return h, b, nil
}

func runhostUpdateCLI(ctx context.Context, to string, br *bufio.Reader, fw *runhost.FrameWriter) (int, error) {
	from := runhostRunningVersion()
	if !clirelease.ValidVersion(to) {
		return engine.ExitUsage, fmt.Errorf("%q is not a released CLI version", to)
	}
	cmp, ok := clirelease.CompareVersions(to, from)
	switch {
	case !ok:
		return engine.ExitCLI, fmt.Errorf("this runhost runs CLI %q, which is not a release, so it is never replaced over the run key", from)
	case cmp < 0:
		return engine.ExitUsage, fmt.Errorf("CLI %s is older than the %s this runhost runs, and a runhost never moves back: update that device's CLI instead", to, from)
	case cmp == 0:
		return 0, fw.Result(runhost.Updated{What: "cli", From: from, To: to})
	}
	h, bin, err := readUpdate(br)
	if err != nil {
		return engine.ExitUsage, err
	}
	if want := clirelease.BinaryName(to, runtime.GOARCH); h.Name != want {
		return engine.ExitVerify, fmt.Errorf("the update is %s, but this runhost is linux/%s and runs %s", h.Name, runtime.GOARCH, want)
	}
	keys, err := runhostReleaseKeys()
	if err != nil {
		return engine.ExitCLI, err
	}
	if _, err := clirelease.Verify(keys, to, runtime.GOARCH, h.Sums, h.Sig, bin); err != nil {
		return engine.ExitVerify, err
	}
	_ = fw.Out("CLI %s: its signature and its SHA-256 for linux/%s verify", to, runtime.GOARCH)
	if err := installRunhostCLI(ctx, to, bin, func(line string) { _ = fw.Out("%s", line) }); err != nil {
		return engine.ExitCLI, err
	}
	waits := runningRunhostJobs()
	askRunhostRestart(to)
	if waits > 0 {
		_ = fw.Out("CLI %s is installed; the service restarts into it once its %d running job(s) finish", to, waits)
	} else {
		_ = fw.Out("CLI %s is installed; the service restarts into it now", to)
	}
	return 0, fw.Result(runhost.Updated{What: "cli", From: from, To: to, Installed: true, WaitsForJobs: waits})
}

// installRunhostCLI puts a verified CLI in its version's directory, checks
// that it runs and says its version, has it ready the images its runs need
// (`runhost service prepare`, the install's last step, whose lines go to
// say), and only then points the CLI's path at it, so a runhost whose new CLI
// cannot prepare stays on the one it runs. The service goes on running the
// CLI it started with until it restarts.
func installRunhostCLI(ctx context.Context, v string, bin []byte, say func(string)) error {
	dir := filepath.Join(runhostCLIDir, v)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the agentry account's own CLI directory, read by sshd's login
		return err
	}
	tmp := filepath.Join(dir, "agentry.new")
	if err := os.WriteFile(tmp, bin, 0o755); err != nil { //nolint:gosec // an executable
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, tmp, "--version").CombinedOutput() //nolint:gosec // the release just verified
	if err != nil || !strings.Contains(string(out), v) {
		_ = os.Remove(tmp)
		return fmt.Errorf("CLI %s does not run on this server: %v %s", v, err, strings.TrimSpace(string(out)))
	}
	if err := prepareRunhostCLI(ctx, tmp, say); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("CLI %s could not ready the images its runs need: %w", v, err)
	}
	final := filepath.Join(dir, "agentry")
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	return pointAt(filepath.Join(runhostBinDir, "agentry"), final)
}

// prepareTimeout bounds a new CLI's `runhost service prepare`, which builds
// the runner image and pulls the others.
var prepareTimeout = 30 * time.Minute

// prepareRunhostCLI runs cli's `runhost service prepare`, passing each line it
// writes to say; a failure names its last lines.
func prepareRunhostCLI(ctx context.Context, cli string, say func(string)) error {
	pctx, cancel := context.WithTimeout(ctx, prepareTimeout)
	defer cancel()
	cmd := exec.CommandContext(pctx, cli, "runhost", "service", "prepare") //nolint:gosec // the release just verified
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return err
	}
	var last []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			say(line)
			if last = append(last, line); len(last) > 3 {
				last = last[1:]
			}
		}
		_, _ = io.Copy(io.Discard, pr)
	}()
	err := cmd.Wait()
	_ = pw.Close()
	<-done
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.Join(last, "; "))
	}
	return nil
}

// pointAt makes link a symbolic link to target, relative, in one rename, so
// whoever opens link finds the old target or the new one.
func pointAt(link, target string) error {
	rel, err := filepath.Rel(filepath.Dir(link), target)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(link), "."+filepath.Base(link)+".new")
	_ = os.Remove(tmp)
	if err := os.Symlink(rel, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

// unameArch is the server's architecture as uname -m says it, which gVisor's
// releases are named by.
func unameArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return runtime.GOARCH
}

func runhostUpdateGVisor(_ context.Context, rel string, br *bufio.Reader, fw *runhost.FrameWriter) (int, error) {
	if rel != setup.GVisorRelease {
		return engine.ExitUsage, fmt.Errorf("this runhost's CLI pins gVisor %s, not %s: update its CLI first", setup.GVisorRelease, rel)
	}
	var cfg runhost.ServerConfig
	if err := readJSONFile(runhostConfigFile, &cfg); err != nil {
		return engine.ExitCLI, fmt.Errorf("the runhost's configuration: %w", err)
	}
	from := cfg.GVisor
	_, archive, err := readUpdate(br)
	if err != nil {
		return engine.ExitUsage, err
	}
	if err := setup.CheckGVisor(unameArch(), archive); err != nil {
		return engine.ExitVerify, err
	}
	repacked, err := setup.RepackGVisor(archive)
	if err != nil {
		return engine.ExitVerify, err
	}
	dir := filepath.Join(runhostGVisorDir, rel)
	if _, err := os.Stat(filepath.Join(dir, "runsc")); err != nil {
		_ = os.RemoveAll(dir + ".new")
		if err := setup.UnpackGVisor(repacked, dir+".new"); err != nil {
			return engine.ExitCLI, err
		}
		if err := os.Rename(dir+".new", dir); err != nil {
			return engine.ExitCLI, err
		}
	}
	runsc := filepath.Join(dir, "runsc")
	if out, err := exec.Command(runsc, "--version").CombinedOutput(); err != nil { //nolint:gosec // the release just checked
		return engine.ExitCLI, fmt.Errorf("runsc %s does not run on this server: %v %s", rel, err, strings.TrimSpace(string(out)))
	}
	if err := pointAt(filepath.Join(runhostBinDir, "runsc"), runsc); err != nil {
		return engine.ExitCLI, err
	}
	cfg.GVisor = rel
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return engine.ExitCLI, err
	}
	if err := writeRunhostFile(runhostConfigFile, b); err != nil {
		return engine.ExitCLI, err
	}
	_ = fw.Out("gVisor %s is installed: runs started from now use it", rel)
	return 0, fw.Result(runhost.Updated{What: "gvisor", From: from, To: rel, Installed: from != rel})
}

// writeRunhostFile replaces one of the service's own files, owner-only, in
// one rename.
func writeRunhostFile(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// runhostUnpairVerb takes the asking device off the runhost: its run key
// and its sealing key, and the credits token it minted, which is revoked.
// The device is the one its run key's forced command names, never one it
// says it is. The last device cannot leave: removing the runhost does that.
func runhostUnpairVerb(ctx context.Context, req runhost.Request, _ io.Reader, fw *runhost.FrameWriter) (int, error) {
	id := req.Device
	if id == "" {
		return engine.ExitUsage, errors.New("this run key names no device, so the runhost cannot tell which device leaves: update the runhost first")
	}
	var devs runhost.Devices
	if err := readJSONFile(runhostDevicesFile, &devs); err != nil {
		return engine.ExitCLI, fmt.Errorf("the runhost's devices: %w", err)
	}
	i := slices.IndexFunc(devs.Devices, func(d runhost.Device) bool { return d.ID == id })
	if i < 0 {
		return engine.ExitUsage, fmt.Errorf("device %s is not paired with this runhost", id)
	}
	if len(devs.Devices) == 1 {
		return engine.ExitUsage, errors.New("this is the runhost's only paired device: remove the runhost instead, which uninstalls it")
	}
	out := remote.Unpaired{Device: id}
	if st, err := openRunhostStore(); err == nil {
		c, err := revokeStoredCredits(ctx, st, id)
		if err != nil {
			return engine.ExitCLI, err
		}
		out.Credits = c
	}
	keys, err := os.ReadFile(runhostAuthorizedKeys) //nolint:gosec // the service's own file
	if err != nil {
		return engine.ExitCLI, err
	}
	var kept strings.Builder
	for _, l := range strings.SplitAfter(string(keys), "\n") {
		if strings.HasSuffix(strings.TrimSpace(l), " "+runhost.DeviceComment(id)) {
			continue
		}
		kept.WriteString(l)
	}
	if err := writeRunhostFile(runhostAuthorizedKeys, []byte(kept.String())); err != nil {
		return engine.ExitCLI, err
	}
	devs.Devices = slices.Delete(devs.Devices, i, i+1)
	b, err := json.MarshalIndent(devs, "", "  ")
	if err != nil {
		return engine.ExitCLI, err
	}
	if err := writeRunhostFile(runhostDevicesFile, b); err != nil {
		return engine.ExitCLI, err
	}
	_ = fw.Out("device %s's run key and sealing key are off this runhost", id)
	return 0, fw.Result(out)
}

// bindRunhostKeys names each run key's device in its forced command, where
// the forced command names none.
func bindRunhostKeys(logf func(string, ...any)) {
	b, err := os.ReadFile(runhostAuthorizedKeys) //nolint:gosec // the service's own file
	if err != nil {
		return
	}
	out, changed := runhost.BindAuthorizedKeys(b)
	if !changed {
		return
	}
	if err := writeRunhostFile(runhostAuthorizedKeys, out); err != nil {
		logf("name the devices in their run keys: %v", err)
		return
	}
	logf("each run key now names its device")
}

// sweepOldCLIs removes the CLI versions the CLI's path no longer points at.
func sweepOldCLIs() {
	target, err := filepath.EvalSymlinks(filepath.Join(runhostBinDir, "agentry"))
	if err != nil {
		return
	}
	keep := filepath.Base(filepath.Dir(target))
	ents, err := os.ReadDir(runhostCLIDir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() && e.Name() != keep {
			_ = os.RemoveAll(filepath.Join(runhostCLIDir, e.Name()))
		}
	}
}
