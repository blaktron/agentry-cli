package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/blaktron/agentry-cli/pkg/removal"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/owner"
)

// On podman the gateway reaches the engine through a Docker-compatible API
// that drops a container's runtime, so a run whose policy names one gets
// an API service of its own: `podman --runtime <name> system service` on
// a socket in the run's directory, whose default runtime is the policy's
// for every container created through it. The run's docker filter dials
// that socket in place of podman's own, the gateway's tools servers,
// declared servers and relays land under the runtime with nothing
// translated, and the filter reads each one's runtime back natively to
// make sure (dockerfilter.go). The service is stopped at teardown, and a
// run whose process died leaves a pid file the next run's sweep acts on.

// podmanServicePIDFile is the run directory's record of its service: the
// pid, its start time and the socket it serves.
const podmanServicePIDFile = "podman-service.pid"

// podmanServiceLog is the service's own output, kept in the receipts.
const podmanServiceLog = "podman-service.log"

// podmanServiceWait bounds how long the service may take to answer.
const podmanServiceWait = 20 * time.Second

// podmanServiceTempPrefix names the directory under the system's temp
// directory that holds the service's socket when the runner home's path is
// too long to bind.
const podmanServiceTempPrefix = "agentry-pm-"

// podmanServiceRecord is the pid file's content.
type podmanServiceRecord struct {
	PID    int    `json:"pid"`
	Start  uint64 `json:"start"`
	Socket string `json:"socket"`
}

// podmanService is a running per-run service.
type podmanService struct {
	process *os.Process
	done    chan error
	socket  string
	pidFile string
	log     *os.File
}

// podmanServiceSocket places the service's socket beside the filter's
// (filterSocketPath): under the runner home, short enough to bind.
func podmanServiceSocket(rd *runDirs, runID, runnerHome string) (string, error) {
	if runnerHome == "" {
		return filepath.Join(rd.root, "podman.sock"), nil
	}
	dir := filepath.Join(runnerHome, "s")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", Classify(ExitGateway, fmt.Errorf("podman service directory: %w", err))
	}
	sock := filepath.Join(dir, runID+"-podman.sock")
	if len(sock) > maxSocketPath {
		tmp, err := os.MkdirTemp("", podmanServiceTempPrefix)
		if err != nil {
			return "", Classify(ExitGateway, fmt.Errorf("podman service directory: %w", err))
		}
		sock = filepath.Join(tmp, "podman.sock")
	}
	return sock, nil
}

// startPodmanService starts the run's service with runtime as its default
// and waits for it to answer. The service outlives the caller's context:
// teardown stops it, after the filter that dials it has closed.
func startPodmanService(ctx context.Context, d dockerx.Client, rd *runDirs, runID, runnerHome, runtime string) (*podmanService, error) {
	servicer, ok := d.(dockerx.PodmanServicer)
	if !ok {
		return nil, Classify(ExitGateway, errors.New("the policy's docker.runtime on podman needs a podman API service of the run's own, which this engine client cannot start"))
	}
	sock, err := podmanServiceSocket(rd, runID, runnerHome)
	if err != nil {
		return nil, err
	}
	_ = os.Remove(sock)
	cmd, err := servicer.PodmanServiceCommand(context.WithoutCancel(ctx), runtime, sock)
	if err != nil {
		return nil, Classify(ExitGateway, err)
	}
	logf, err := os.OpenFile(filepath.Join(rd.receiptsDir, podmanServiceLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // a receipt the run owns
	if err != nil {
		return nil, Classify(ExitGateway, fmt.Errorf("podman service log: %w", err))
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return nil, Classify(ExitGateway, fmt.Errorf("start the run's podman API service: %w", err))
	}
	s := &podmanService{process: cmd.Process, done: make(chan error, 1), socket: sock, pidFile: filepath.Join(rd.root, podmanServicePIDFile), log: logf}
	go func() { s.done <- cmd.Wait() }()
	start, _ := owner.StartTime(cmd.Process.Pid)
	rec, _ := json.Marshal(podmanServiceRecord{PID: cmd.Process.Pid, Start: start, Socket: sock})
	if err := os.WriteFile(s.pidFile, append(rec, '\n'), 0o600); err != nil {
		s.stop()
		return nil, Classify(ExitGateway, fmt.Errorf("record the run's podman API service: %w", err))
	}
	deadline := time.Now().Add(podmanServiceWait)
	for {
		if err := dockerx.PingSocket(ctx, sock); err == nil {
			return s, nil
		}
		select {
		case werr := <-s.done:
			s.done <- werr
			s.stop()
			return nil, Classify(ExitGateway, fmt.Errorf("the run's podman API service exited before it answered (%v): see receipts/%s", werr, podmanServiceLog))
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			s.stop()
			return nil, Classify(ExitGateway, fmt.Errorf("the run's podman API service did not answer on %s within %s: see receipts/%s", sock, podmanServiceWait, podmanServiceLog))
		}
	}
}

// stop ends the service, gently then not, and removes its socket and
// record.
func (s *podmanService) stop() {
	if s == nil {
		return
	}
	stopProcess(s.process, s.done)
	_ = os.Remove(s.socket)
	_ = os.Remove(s.pidFile)
	removeServiceTempDir(s.socket)
	if s.log != nil {
		_ = s.log.Close()
	}
}

// stopProcess asks a process to end and kills it when it does not within
// a few seconds; done, when given, is where its Wait reports.
func stopProcess(p *os.Process, done chan error) {
	if p == nil {
		return
	}
	_ = p.Signal(syscall.SIGTERM)
	if done == nil {
		time.Sleep(time.Second)
		_ = p.Kill()
		return
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = p.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
}

// removeServiceTempDir removes the agentry-pm-* directory a service socket
// was placed in, once the socket is gone; a socket anywhere else has no
// directory of its own.
func removeServiceTempDir(socket string) {
	if dir := filepath.Dir(socket); strings.HasPrefix(filepath.Base(dir), podmanServiceTempPrefix) {
		_ = os.Remove(dir)
	}
}

// StopLeftPodmanService ends the podman API service the run directory
// dir of run runID names in its record, when the run that started it is
// gone (the directory's lock proved it, runlock.go), before the directory
// and the record go. dir is the caller's to trust; the record in it is a
// file the person can write, and a sweep that signals a process and
// unlinks a file holds it to the one shape a run's service can have rather
// than believing it. The socket is removed only at the one path the run
// gives its service (podmanServiceSocket: under the runner home's s/
// directory by the run's id, the short path under the system's temp
// directory, or the run directory's own when there is no runner home). The
// process is signalled only when its start time is the recorded one (a
// reused pid is never signalled), its own command line is a podman binary
// running the `system service` subcommand, and that command line names
// the run's own socket, so the service of another run is never ended by a
// record naming its pid. The record always goes. Off Linux no start time or
// command line can be read, so no process is touched there. Best effort:
// nothing here fails the sweep. Exported for the desktop app, whose own
// sweeps reach a dead run's directory the same way.
func StopLeftPodmanService(dir, runnerHome, runID string) {
	if runID == "" || runID != filepath.Base(runID) {
		return
	}
	path := filepath.Join(dir, podmanServicePIDFile)
	b, err := os.ReadFile(path) //nolint:gosec // a fixed name in a run directory
	if err != nil {
		return
	}
	var rec podmanServiceRecord
	if json.Unmarshal(b, &rec) != nil || rec.PID <= 0 {
		return
	}
	socket := filepath.Clean(rec.Socket)
	if serviceSocketOfRun(socket, dir, runnerHome, runID) {
		if start, ok := owner.StartTime(rec.PID); ok && start == rec.Start && isPodmanServiceProcess(rec.PID, socket) {
			if p, err := os.FindProcess(rec.PID); err == nil {
				stopProcess(p, nil)
			}
		}
		_ = removal.Remove(socket, false)
		removeServiceTempDir(socket)
	}
	_ = removal.Remove(path, false)
}

// isPodmanServiceProcess reads the process's own command line and asks
// isPodmanServiceArgv. Where the command line cannot be read, no.
func isPodmanServiceProcess(pid int, socket string) bool {
	argv, ok := owner.CommandLine(pid)
	return ok && isPodmanServiceArgv(argv, socket)
}

// isPodmanServiceArgv says whether a command line is a podman binary
// running the `system service` subcommand on socket, as
// dockerx.PodmanServiceCommand spells it: the binary is named podman, or
// is the one AGENTRY_PODMAN names; after it come only global flags, each
// `--flag=value` or `--flag value`, then `system service`, then the
// service's own flags and unix://<socket>. `podman run … system service`
// is a container's command, not a service, and is not one.
func isPodmanServiceArgv(argv []string, socket string) bool {
	if len(argv) == 0 {
		return false
	}
	if filepath.Base(argv[0]) != "podman" {
		if named := os.Getenv(dockerx.EnvPodman); named == "" || argv[0] != named {
			return false
		}
	}
	rest := argv[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		if strings.Contains(rest[0], "=") || len(rest) == 1 {
			rest = rest[1:]
			continue
		}
		rest = rest[2:]
	}
	if len(rest) < 2 || rest[0] != "system" || rest[1] != "service" {
		return false
	}
	want := dockerx.SchemeUnix + "://" + socket
	for _, a := range rest[2:] {
		if a == want || (filepath.IsAbs(a) && filepath.Clean(a) == socket) {
			return true
		}
	}
	return false
}

// serviceSocketOfRun says whether path, cleaned, is where the run's service
// puts its socket (podmanServiceSocket): <runner home>/s/<run id>-podman.sock,
// or, when that is too long to bind, podman.sock in a directory named
// podmanServiceTempPrefix* under the system's temp directory, or, with no
// runner home, podman.sock in the run directory itself. Anything else is
// not removed.
func serviceSocketOfRun(path, dir, runnerHome, runID string) bool {
	if path == "" || !filepath.IsAbs(path) || path != filepath.Clean(path) {
		return false
	}
	if runnerHome == "" {
		return path == filepath.Join(dir, "podman.sock")
	}
	if path == filepath.Join(runnerHome, "s", runID+"-podman.sock") {
		return true
	}
	parent, base := filepath.Split(path)
	parent = filepath.Clean(parent)
	return base == "podman.sock" && filepath.Dir(parent) == filepath.Clean(os.TempDir()) && strings.HasPrefix(filepath.Base(parent), podmanServiceTempPrefix)
}
