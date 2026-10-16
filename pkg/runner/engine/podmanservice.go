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
		tmp, err := os.MkdirTemp("", "agentry-pm-")
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
	if dir := filepath.Dir(s.socket); filepath.Base(dir) != "s" && strings.HasPrefix(filepath.Base(dir), "agentry-pm-") {
		_ = os.Remove(dir)
	}
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

// stopLeftPodmanService ends the service a run directory's record names
// when the run that started it is gone (the directory's lock proved it,
// runlock.go): the process, if it is still the one the record describes
// (the same start time, so a reused pid is never killed), then its socket
// and the record. Best effort.
func stopLeftPodmanService(dir string) {
	path := filepath.Join(dir, podmanServicePIDFile)
	b, err := os.ReadFile(path) //nolint:gosec // a fixed name in a run directory
	if err != nil {
		return
	}
	var rec podmanServiceRecord
	if json.Unmarshal(b, &rec) != nil || rec.PID <= 0 {
		return
	}
	if start, ok := owner.StartTime(rec.PID); ok && start == rec.Start {
		if p, err := os.FindProcess(rec.PID); err == nil {
			stopProcess(p, nil)
		}
	}
	if rec.Socket != "" && filepath.IsAbs(rec.Socket) {
		_ = removal.Remove(rec.Socket, false)
	}
	_ = removal.Remove(path, false)
}
