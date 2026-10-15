package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/removal"
)

// RunLockFile is the lock a run holds on its own run directory,
// <RunRoot>/runs/<id>/run.lock, from before anything sensitive is written
// into the directory to the end of its teardown. The kernel releases it
// when the process dies however it dies, so a later run that can take the
// lock knows the directory's run is over: that is how a run killed before
// its teardown (SIGKILL, an OOM kill, a supervisor's timeout, a power cut)
// has its secrets file, its connections' tokens and its other sensitive
// files removed by the next run of the same install. Unlike the owner
// labels the container sweep judges by, the lock needs no process record,
// so it holds on every platform and across pid namespaces. It is a lock on
// the open file (flock, LockFileEx), so two runs of one process exclude
// each other as two processes do; the runner home is taken to be on a
// local file system, where that holds.
const RunLockFile = "run.lock"

// runLockRecord is what a run writes into its lock once it holds it. A
// lock file without one is a run that died before writing it, or one
// whose writer is between creating the file and locking it, and is
// never swept: nothing sensitive is written before the record.
type runLockRecord struct {
	RunID string `json:"run_id"`
	// Keep is the run's --keep-run-dir: its directory outlives it, all but
	// the sensitive files.
	Keep bool `json:"keep"`
}

// runLock is a held run lock.
type runLock struct {
	f    *os.File
	path string
}

// runLockTries and runLockDelay bound how long a run waits for its own
// lock: a sweeping run can hold it for the moment it takes to read an
// empty record.
var (
	runLockTries = 40
	runLockDelay = 50 * time.Millisecond
)

// holdRunLock takes the lock of run directory root, which must exist, and
// writes the run's record into it.
func holdRunLock(root, runID string, keep bool) (*runLock, error) {
	path := filepath.Join(root, RunLockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600) //nolint:gosec // fixed name in the run's own directory
	if err != nil {
		return nil, fmt.Errorf("lock the run directory %s: %w", root, err)
	}
	for i := 0; ; i++ {
		if err = tryLockFile(f); err == nil || i+1 >= runLockTries || !errors.Is(err, errLockHeld) {
			break
		}
		time.Sleep(runLockDelay)
	}
	if err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		if errors.Is(err, errLockHeld) {
			err = errors.New("another process holds its lock")
		}
		return nil, fmt.Errorf("lock the run directory %s: %w", root, err)
	}
	b, _ := json.Marshal(runLockRecord{RunID: runID, Keep: keep})
	if _, err := f.Write(b); err != nil {
		_ = unlockFile(f)
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("lock the run directory %s: %w", root, err)
	}
	_ = f.Sync()
	return &runLock{f: f, path: path}, nil
}

// release lets the lock go. A kept directory loses its lock file too, so
// no later sweep takes the directory for a dead run's. The file is closed
// first: on Windows an open file cannot be deleted, nor the directory
// holding it.
func (l *runLock) release(keep bool) {
	if l == nil || l.f == nil {
		return
	}
	_ = unlockFile(l.f)
	_ = l.f.Close()
	l.f = nil
	if keep {
		_ = removal.Remove(l.path, false)
	}
}

// sensitivePath is one thing in a run directory that must not outlive the
// run, with whether it is a directory to remove whole.
type sensitivePath struct {
	path string
	all  bool
}

// sensitive is what teardown deletes even from a kept run directory: the
// secrets file, the connections' folders, the model sign-in's token, the
// copies of the knowledge packs, the target hosts' sign-ins and the
// browser's files.
func (rd *runDirs) sensitive() []sensitivePath {
	return []sensitivePath{
		{rd.secretsPath, false},
		{rd.connections(), true},
		{rd.modelSignInDir(), true},
		{rd.knowledge, true},
		{rd.hostsDir(), true},
		{rd.browserDir(), true},
	}
}

// runDirsAt is the paths of run directory root, as prepareRunDirs lays
// them out, for what is found there after its run. Only the fields that
// derive from root are set.
func runDirsAt(root string) *runDirs {
	return &runDirs{
		root:        root,
		workspace:   filepath.Join(root, "workspace"),
		inputs:      filepath.Join(root, "inputs"),
		inputsText:  filepath.Join(root, "inputs-text"),
		knowledge:   filepath.Join(root, "knowledge"),
		cfg:         filepath.Join(root, "cfg"),
		secretsPath: filepath.Join(root, "cfg", "gateway", "secrets.env"),
	}
}

// sweepRunDirs removes what runs whose process died left in the install's
// run directories, <runRoot>/runs/<id>: the sensitive files of each, and
// the whole directory unless the run kept it. A directory is a dead run's
// when its lock can be taken and holds a run's record: a live run, in this
// process or any other, holds its lock, and is never touched. A directory
// with no lock file is a dead run's only when deadRuns names it, the runs whose containers the container sweep found
// with a dead owner; only its sensitive files go then, since whether it was
// kept is not recorded.
//
// Best effort: the sweep never fails a run. It says what it removed, and
// names a sensitive file it could not delete. It returns the ids of the
// runs it swept, every one proven dead: by its free lock and its record, or
// by deadRuns.
func sweepRunDirs(runRoot string, deadRuns map[string]bool, stderr io.Writer) []string {
	if runRoot == "" {
		return nil
	}
	runs := filepath.Join(runRoot, "runs")
	if fi, err := os.Lstat(runs); err != nil || !fi.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(runs)
	if err != nil {
		return nil
	}
	var swept, left []string
	for _, e := range entries {
		// A real directory only: a link standing in a run's place is not
		// followed out of the runner home.
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(runs, e.Name())
		ok, still := sweepRunDir(dir, deadRuns[e.Name()])
		if ok {
			swept = append(swept, e.Name())
		}
		left = append(left, still...)
	}
	sort.Strings(swept)
	if stderr == nil {
		return swept
	}
	if len(swept) > 0 {
		_, _ = fmt.Fprintf(stderr, "removed what %d run(s) whose process had died left in their run directories: %s\n", len(swept), strings.Join(swept, ", "))
	}
	if len(left) > 0 {
		_, _ = fmt.Fprintf(stderr, "warning: could not delete %s, left by a run whose process had died: it may hold secret values in the clear; delete it by hand\n", strings.Join(left, "; "))
	}
	return swept
}

// RunsInUse lists, sorted, the runs under runRoot/runs whose lock a live
// run holds, in this process or any other: what a client asks before it
// removes something a run of the same release may be using, such as the
// release's profile. A directory whose lock is free, or that has none, is
// not listed.
func RunsInUse(runRoot string) []string {
	if runRoot == "" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(runRoot, "runs"))
	if err != nil {
		return nil
	}
	var live []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		lockPath := filepath.Join(runRoot, "runs", e.Name(), RunLockFile)
		if fi, err := os.Lstat(lockPath); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		f, err := os.OpenFile(lockPath, os.O_RDWR, 0) //nolint:gosec // fixed name in a run directory
		if err != nil {
			continue
		}
		err = tryLockFile(f)
		if err == nil {
			_ = unlockFile(f)
		}
		_ = f.Close()
		if errors.Is(err, errLockHeld) {
			live = append(live, e.Name())
		}
	}
	sort.Strings(live)
	return live
}

// sweepRunDir sweeps one run directory as sweepRunDirs says, reporting
// whether it was a dead run's and the sensitive paths it could not delete.
func sweepRunDir(dir string, deadByContainers bool) (bool, []string) {
	lockPath := filepath.Join(dir, RunLockFile)
	fi, err := os.Lstat(lockPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !deadByContainers {
			return false, nil
		}
		return true, removeSensitiveFiles(dir)
	case err != nil || !fi.Mode().IsRegular():
		return false, nil
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0) //nolint:gosec // fixed name in a run directory
	if err != nil {
		return false, nil
	}
	if err := tryLockFile(f); err != nil {
		// Held: the run is live. Or the lock cannot be taken at all, which
		// proves nothing either.
		_ = f.Close()
		return false, nil
	}
	var rec runLockRecord
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil || json.Unmarshal(b, &rec) != nil || rec.RunID != filepath.Base(dir) {
		_ = unlockFile(f)
		_ = f.Close()
		return false, nil
	}
	left := removeSensitiveFiles(dir)
	// The lock goes before the directory: on Windows neither can be
	// deleted while it is open. A run that takes it in between finds the
	// same dead run, and removes the same files.
	_ = unlockFile(f)
	_ = f.Close()
	switch {
	case len(left) > 0:
		// What stands keeps the lock and its record, so the next sweep
		// tries it again.
	case rec.Keep:
		_ = removal.Remove(lockPath, false)
	default:
		_ = removal.Remove(dir, true)
	}
	return true, left
}

// removeSensitiveFiles deletes run directory dir's sensitive files, returning
// those it could not.
func removeSensitiveFiles(dir string) []string {
	var left []string
	for _, s := range runDirsAt(dir).sensitive() {
		if err := removal.Remove(s.path, s.all); err != nil && !removal.Gone(s.path) {
			left = append(left, s.path)
		}
	}
	return left
}
