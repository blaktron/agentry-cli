package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/session/protocol"
)

// A job the service runs outlives the SSH session that sent it: it runs under
// the service, its frames are kept for whoever attaches, and it ends when its
// run does, when it is cancelled, or when the service stops. A device that
// drops off reattaches with `attach`, and collects the sealed output and the
// sealed envelope once the job is done.

// runhostMaxReplay bounds the frames a job keeps for a later attach; the
// oldest go first, and an attach that asks for them is told how many.
const runhostMaxReplay = 8 << 20

// runhostLive is a job running under the service.
type runhostLive struct {
	id, ref, device string
	session         bool
	started         time.Time
	cancel          context.CancelFunc
	// stdin takes a session's commands; nil for a run.
	stdin io.WriteCloser
	// inputs is a session's inputs folder, where a device's added files land.
	inputs string

	mu      sync.Mutex
	frames  []runhost.Frame
	size    int
	dropped int
	changed chan struct{}
	done    bool
	code    int
	err     error
}

var runhostLiveJobs = struct {
	sync.Mutex
	m map[string]*runhostLive
}{m: map[string]*runhostLive{}}

func newRunhostLive(j *job.Job, cancel context.CancelFunc) *runhostLive {
	l := &runhostLive{id: j.ID, ref: j.Release.Ref(), device: j.Device, session: j.Run.Session,
		started: time.Now().UTC(), cancel: cancel, changed: make(chan struct{})}
	runhostLiveJobs.Lock()
	runhostLiveJobs.m[j.ID] = l
	runhostLiveJobs.Unlock()
	return l
}

func liveJob(id string) *runhostLive {
	runhostLiveJobs.Lock()
	defer runhostLiveJobs.Unlock()
	return runhostLiveJobs.m[id]
}

// runningJob is the live job id while it runs: once it has finished, its
// sealed output and envelope are on disk, though the job may not have been
// taken out of the registry yet.
func runningJob(id string) *runhostLive {
	l := liveJob(id)
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return nil
	}
	return l
}

// forget takes a finished job out of the registry: its record and sealed
// output on disk answer for it from then on.
func (l *runhostLive) forget() {
	runhostLiveJobs.Lock()
	if runhostLiveJobs.m[l.id] == l {
		delete(runhostLiveJobs.m, l.id)
	}
	runhostLiveJobs.Unlock()
}

// emit keeps f for every attach, now and later.
func (l *runhostLive) emit(f runhost.Frame) {
	n := len(f.Text) + len(f.Result) + 64
	l.mu.Lock()
	l.frames = append(l.frames, f)
	l.size += n
	for l.size > runhostMaxReplay && len(l.frames) > 1 {
		l.size -= len(l.frames[0].Text) + len(l.frames[0].Result) + 64
		l.frames = l.frames[1:]
		l.dropped++
	}
	close(l.changed)
	l.changed = make(chan struct{})
	l.mu.Unlock()
}

func (l *runhostLive) out(format string, args ...any) {
	l.emit(runhost.Frame{Type: runhost.FrameOut, Text: fmt.Sprintf(format, args...)})
}

func (l *runhostLive) finish(code int, err error) {
	l.mu.Lock()
	l.done, l.code, l.err = true, code, err
	close(l.changed)
	l.changed = make(chan struct{})
	l.mu.Unlock()
}

// wait returns once the job is done, or ctx is.
func (l *runhostLive) wait(ctx context.Context) (done bool) {
	for {
		l.mu.Lock()
		d, ch := l.done, l.changed
		l.mu.Unlock()
		if d {
			return true
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

// follow writes the job's frames from index from to fw as they come, and
// once the job is done its result. It returns early, writing nothing more,
// when gone closes (the device left) or ctx is done (the service stops).
func (l *runhostLive) follow(ctx context.Context, fw *runhost.FrameWriter, from int, gone <-chan struct{}) (int, error) {
	next := from
	for {
		l.mu.Lock()
		if next < l.dropped {
			skipped := l.dropped - next
			next = l.dropped
			l.mu.Unlock()
			if err := fw.Out("(%d earlier lines of the job's narration are no longer kept on the runhost)", skipped); err != nil {
				return 0, nil
			}
			continue
		}
		pending := append([]runhost.Frame(nil), l.frames[next-l.dropped:]...)
		done, code, jerr, ch := l.done, l.code, l.err, l.changed
		l.mu.Unlock()
		for _, f := range pending {
			if err := fw.Frame(f); err != nil {
				return 0, nil
			}
			next++
		}
		if done {
			if jerr != nil {
				return engine.ExitCLI, jerr
			}
			return code, fw.Result(runhostJobResult{Job: l.id, ExitCode: code, Done: true, Next: next})
		}
		select {
		case <-ch:
		case <-gone:
			return 0, nil
		case <-ctx.Done():
			return 0, nil
		}
	}
}

// relaySession takes a session's input from the device: each line is a
// command for the session, or a chunk of a file the device adds, written to
// the session's inputs folder before the command that names it. It returns at
// the end of the input, which is when the device left: the session itself
// goes on to its idle timeout, as a local session does when its chat goes
// quiet.
func (l *runhostLive) relaySession(in io.Reader) {
	br := bufio.NewReaderSize(in, 64<<10)
	var open *os.File
	var openName string
	defer func() {
		if open != nil {
			_ = open.Close()
		}
	}()
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var m runhostSessionInput
			if json.Unmarshal(line, &m) != nil || (m.Command == nil) == (m.File == nil) {
				l.out("the runhost refused a line of the session's input that is neither a command nor a file")
			} else if m.File != nil {
				if err := l.takeFile(m.File, &open, &openName); err != nil {
					l.out("the runhost refused the file %q: %v", m.File.Name, err)
				}
			} else if l.stdin != nil {
				_, _ = l.stdin.Write(append(append([]byte(nil), *m.Command...), '\n'))
			}
		}
		if err != nil {
			return
		}
	}
}

// runhostSessionInput is one line of a session's input from the device, and
// runhostSessionFile a chunk of a file it adds.
type (
	runhostSessionInput = runhost.SessionInput
	runhostSessionFile  = runhost.SessionFile
)

// runhostMaxSessionFile bounds one file a device adds to a session.
const runhostMaxSessionFile = runhost.MaxSessionFile

func (l *runhostLive) takeFile(f *runhostSessionFile, open **os.File, openName *string) error {
	if l.inputs == "" {
		return errors.New("this job is not a session")
	}
	if *open == nil || *openName != f.Name {
		if *open != nil {
			_ = (*open).Close()
			*open = nil
		}
		if f.Name == "" || len(f.Name) > 200 || filepath.Base(f.Name) != f.Name || f.Name == "." || f.Name == ".." {
			return errors.New("not one file name")
		}
		w, err := os.OpenFile(filepath.Join(l.inputs, f.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY|openNoFollow, 0o600) //nolint:gosec // the session's own inputs folder, a checked name
		if err != nil {
			return err
		}
		*open, *openName = w, f.Name
	}
	if st, err := (*open).Stat(); err == nil && st.Size()+int64(len(f.Data)) > runhostMaxSessionFile {
		return fmt.Errorf("over %d bytes", runhostMaxSessionFile)
	}
	if _, err := (*open).Write(f.Data); err != nil {
		return err
	}
	if !f.More {
		err := (*open).Close()
		*open, *openName = nil, ""
		return err
	}
	return nil
}

// runhostAttachVerb answers `attach <job> [from]`: a running job's frames
// from index from, live until it ends, and its result; a finished job's
// result alone.
func runhostAttachVerb(ctx context.Context, req runhost.Request, in io.Reader, fw *runhost.FrameWriter) (int, error) {
	if len(req.Args) == 0 || len(req.Args) > 2 || !job.ValidID(req.Args[0]) {
		return engine.ExitUsage, errors.New("attach takes a job's id, and then where to resume its narration")
	}
	from := 0
	if len(req.Args) == 2 {
		n, err := strconv.Atoi(req.Args[1])
		if err != nil || n < 0 {
			return engine.ExitUsage, errors.New("attach resumes from a frame's index")
		}
		from = n
	}
	gone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, in)
		close(gone)
	}()
	if l := liveJob(req.Args[0]); l != nil {
		if err := fw.Started(l.id); err != nil {
			return 0, nil
		}
		return l.follow(ctx, fw, from, gone)
	}
	done, err := readJobDone(req.Args[0])
	if err != nil {
		return runhostExitNoOutput, err
	}
	return done.ExitCode, fw.Result(runhostJobResult{Job: done.Job, ExitCode: done.ExitCode, Done: true})
}

// runhostCancelVerb answers `cancel <job>`: the job's run is interrupted and
// tears down, and the answer comes once it has ended, sealed like any other.
func runhostCancelVerb(ctx context.Context, req runhost.Request, _ io.Reader, fw *runhost.FrameWriter) (int, error) {
	if len(req.Args) != 1 || !job.ValidID(req.Args[0]) {
		return engine.ExitUsage, errors.New("cancel takes a job's id")
	}
	l := liveJob(req.Args[0])
	if l == nil {
		if done, err := readJobDone(req.Args[0]); err == nil {
			return 0, fw.Result(runhostJobResult{Job: done.Job, ExitCode: done.ExitCode, Done: true})
		}
		return runhostExitNoOutput, fmt.Errorf("the runhost runs no job %s", req.Args[0])
	}
	l.out("cancelled from a device")
	l.cancel()
	if l.stdin != nil {
		_ = l.stdin.Close()
	}
	waitCtx, stop := context.WithTimeout(ctx, 2*runhostCancelGrace+time.Minute)
	defer stop()
	if !l.wait(waitCtx) {
		return engine.ExitCLI, fmt.Errorf("job %s has not ended yet; it is still being torn down", l.id)
	}
	l.mu.Lock()
	code := l.code
	l.mu.Unlock()
	return 0, fw.Result(runhostJobResult{Job: l.id, ExitCode: code, Done: true})
}

func readJobDone(id string) (runhostJobDone, error) {
	var done runhostJobDone
	if err := readJSONFile(filepath.Join(runhostJobsDir, id, runhostJobRecord), &done); err != nil {
		return done, fmt.Errorf("the runhost holds no job %s: it was collected, has expired, or never finished", id)
	}
	return done, nil
}

// runhostJobInfo is one job as `status` lists it.
type runhostJobInfo struct {
	Job        string `json:"job"`
	Ref        string `json:"ref"`
	Device     string `json:"device"`
	State      string `json:"state"`
	Session    bool   `json:"session,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
	ExitCode   *int   `json:"exitCode,omitempty"`
}

// runhostJobs lists the jobs the service runs and the finished ones whose
// output waits for collection.
func runhostJobs() []runhostJobInfo {
	out := []runhostJobInfo{}
	runhostLiveJobs.Lock()
	running := map[string]bool{}
	for _, l := range runhostLiveJobs.m {
		running[l.id] = true
		out = append(out, runhostJobInfo{Job: l.id, Ref: l.ref, Device: l.device, State: "running", Session: l.session, StartedAt: l.started.Format(time.RFC3339)})
	}
	runhostLiveJobs.Unlock()
	entries, _ := os.ReadDir(runhostJobsDir)
	for _, e := range entries {
		if running[e.Name()] {
			continue
		}
		done, err := readJobDone(e.Name())
		if err != nil {
			continue
		}
		code := done.ExitCode
		out = append(out, runhostJobInfo{Job: done.Job, Ref: done.Ref, Device: done.Device, State: "finished", Session: done.Session, FinishedAt: done.FinishedAt, ExitCode: &code})
	}
	return out
}

// sessionEventFrame is a session's event as the device takes it: the line
// the session protocol wrote, masked of the job's values.
func sessionEventFrame(line []byte, values []string) (runhost.Frame, bool) {
	masked := []byte(maskValues(string(line), values))
	var ev protocol.Event
	if json.Unmarshal(masked, &ev) != nil {
		return runhost.Frame{}, false
	}
	return runhost.Frame{Type: runhost.FrameEvent, Result: masked}, true
}

// sealedEnvelopeJob is the job string the envelope is sealed under, apart
// from the output's, so neither opens as the other.
func sealedEnvelopeJob(id string) string { return runhost.SealedEnvelopeJob(id) }
