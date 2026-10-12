package schedule

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Schedule is one calendar entry for a run on this computer: what to run,
// how, and when (plan §4). It never holds a credential (invariant 7): the
// model key stays in the keychain and secrets in the policy's source.
type Schedule struct {
	ID   string `yaml:"id" json:"id"`
	Name string `yaml:"name" json:"name"`

	// Ref is ns/name of a registry release, pinned to Version and Digest
	// (D3): a fire runs exactly the version consented to when the schedule
	// was made. Local is a QuickAgent directory instead of a ref.
	Ref      string `yaml:"ref,omitempty" json:"ref,omitempty"`
	Version  string `yaml:"version,omitempty" json:"version,omitempty"`
	Digest   string `yaml:"digest,omitempty" json:"digest,omitempty"`
	Local    string `yaml:"local,omitempty" json:"local,omitempty"`
	Registry string `yaml:"registry,omitempty" json:"registry,omitempty"`

	Run  RunSpec    `yaml:"run" json:"run"`
	When Recurrence `yaml:"when" json:"when"`

	// SignedOut: the schedule also runs while its person is signed out
	// (Linux only, D17–D19): its alarm is started by a lingering user
	// manager and reads its keys from the encrypted credential beside it
	// (signedout.go), never from the keychain.
	SignedOut bool `yaml:"signedOut,omitempty" json:"signedOut,omitempty"`

	Paused  bool      `yaml:"paused,omitempty" json:"paused,omitempty"`
	Created time.Time `yaml:"created" json:"created"`
}

// RunSpec is the run setup's answers a fire passes to `agentry run`.
type RunSpec struct {
	// OutDir is the folder each scheduled run's own folder is made in.
	OutDir string `yaml:"outDir" json:"outDir"`
	// Input is a task file read at each fire; Task is the schedule's own
	// task text, kept beside the schedule as task.txt.
	Input string `yaml:"input,omitempty" json:"input,omitempty"`
	Task  bool   `yaml:"task,omitempty" json:"task,omitempty"`

	Files     []string `yaml:"files,omitempty" json:"files,omitempty"`
	Knowledge []string `yaml:"knowledge,omitempty" json:"knowledge,omitempty"`
	// Intake answers (id=value), never a secret question's.
	Intake  []string `yaml:"intake,omitempty" json:"intake,omitempty"`
	Voice   bool     `yaml:"voice,omitempty" json:"voice,omitempty"`
	NoVoice bool     `yaml:"noVoice,omitempty" json:"noVoice,omitempty"`

	Timeout  int     `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	MaxTurns int     `yaml:"maxTurns,omitempty" json:"maxTurns,omitempty"`
	MaxCost  float64 `yaml:"maxCost,omitempty" json:"maxCost,omitempty"`
	Adapter  string  `yaml:"adapter,omitempty" json:"adapter,omitempty"`
	Policy   string  `yaml:"policy,omitempty" json:"policy,omitempty"`

	// Hosts are the target hosts the schedule was readied for, when it was
	// made (plan runtime-hosts.md D12; agentry-cli#863): the answer is in
	// Intake; each keeps where its sign-in is saved and the host key the
	// person accepted. Never a credential.
	Hosts []Host `yaml:"hosts,omitempty" json:"hosts,omitempty"`

	// The consents a person gave, explicitly, when the schedule was made;
	// a fire passes only these (plan §5).
	AllowUnsigned      bool `yaml:"allowUnsigned,omitempty" json:"allowUnsigned,omitempty"`
	AllowUnscanned     bool `yaml:"allowUnscanned,omitempty" json:"allowUnscanned,omitempty"`
	AllowVulnerableMCP bool `yaml:"allowVulnerableMcp,omitempty" json:"allowVulnerableMcp,omitempty"`
}

// Host is a target host a schedule was made for. A fire reaches it only
// with the sign-in saved under Account (service agentry) and only if it
// offers the key HostKey names; anything else refuses the run (exit 11).
type Host struct {
	// ID is the intake question that asks for it.
	ID string `yaml:"id" json:"id"`
	// Account is the keychain account its sign-in is saved under
	// (host:<user>@<host>:<port>): a name, not a secret.
	Account string `yaml:"account" json:"account"`
	// HostKey is the accepted host key's SHA256 fingerprint.
	HostKey string `yaml:"hostKey" json:"hostKey"`
}

// State is what fires have done to a schedule; only fire writes it, so an
// edit and a fire never race over one file.
type State struct {
	// Last is the latest occurrence handled (ran, failed or missed).
	Last time.Time `json:"last,omitempty"`
	// Ended is set once no occurrence is left.
	Ended bool `json:"ended,omitempty"`
	// Runs counts the occurrences fire ran (succeeded or failed), for an
	// end after N runs; a missed occurrence and a Run now are not counted.
	Runs int `json:"runs,omitempty"`
}

// Outcome is what became of one occurrence.
type Outcome string

const (
	// Ran: the run finished with exit 0.
	Ran Outcome = "ran"
	// Failed: the run, or what it needed, failed; ExitCode says how.
	Failed Outcome = "failed"
	// Missed: the computer was off, asleep or signed out when it fell due,
	// and the next chance came after the following occurrence.
	Missed Outcome = "missed"
)

// Fire is the record of one occurrence, one line of fires.ndjson.
type Fire struct {
	Occurrence time.Time `json:"occurrence"`
	Outcome    Outcome   `json:"outcome"`
	// Manual marks a Run now, which is no occurrence.
	Manual    bool      `json:"manual,omitempty"`
	Started   time.Time `json:"started,omitzero"`
	Ended     time.Time `json:"ended,omitzero"`
	RunID     string    `json:"runId,omitempty"`
	ExitCode  int       `json:"exitCode"`
	ExitClass string    `json:"exitClass,omitempty"`
	OutDir    string    `json:"outDir,omitempty"`
	Message   string    `json:"message,omitempty"`
}

// Store is the folder of schedules under the runner home. It is the truth
// about them; the operating system's entries are only alarms (D4).
type Store struct{ Dir string }

// StoreIn is the store under a runner home.
func StoreIn(runnerHome string) Store { return Store{Dir: filepath.Join(runnerHome, "schedules")} }

// idExpr is a schedule id: what newID draws.
const idExpr = `[a-z0-9]{10}`

var idPattern = regexp.MustCompile(`^` + idExpr + `$`)

// ValidID reports whether id is a schedule id; every path and every OS
// entry name is built from one, so nothing else may reach them.
func ValidID(id string) bool { return idPattern.MatchString(id) }

func newID() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("draw a schedule id: %w", err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b), nil
}

func (s Store) dir(id string) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("%q is not a schedule id", id)
	}
	return filepath.Join(s.Dir, id), nil
}

// ErrNotFound is a schedule the store does not hold.
var ErrNotFound = errors.New("no such schedule")

// Create gives sc an id and keeps it, with its task text when task is
// not nil.
func (s Store) Create(sc *Schedule, task []byte) error {
	id, err := newID()
	if err != nil {
		return err
	}
	sc.ID = id
	d, _ := s.dir(id) // newID's alphabet and length are ValidID's
	if err := os.MkdirAll(d, 0o700); err != nil {
		return fmt.Errorf("make the schedule's folder: %w", err)
	}
	if task != nil {
		if err := writeAtomic(filepath.Join(d, "task.txt"), task, 0o600); err != nil {
			return err
		}
		sc.Run.Task = true
	}
	return s.Save(*sc)
}

// Save writes sc's definition.
func (s Store) Save(sc Schedule) error {
	d, err := s.dir(sc.ID)
	if err != nil {
		return err
	}
	b, err := yaml.Marshal(sc)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(d, "schedule.yaml"), b, 0o600)
}

// Get reads one schedule.
func (s Store) Get(id string) (Schedule, error) {
	d, err := s.dir(id)
	if err != nil {
		return Schedule{}, err
	}
	b, err := os.ReadFile(filepath.Join(d, "schedule.yaml"))
	if errors.Is(err, fs.ErrNotExist) {
		return Schedule{}, fmt.Errorf("%s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Schedule{}, err
	}
	var sc Schedule
	if err := yaml.Unmarshal(b, &sc); err != nil {
		return Schedule{}, fmt.Errorf("schedule %s: %w", id, err)
	}
	if sc.ID != id {
		return Schedule{}, fmt.Errorf("schedule %s: its file names %q", id, sc.ID)
	}
	return sc, nil
}

// List reads every schedule, oldest first. A folder that does not hold a
// readable schedule is skipped and reported in bad.
func (s Store) List() (out []Schedule, bad []error, err error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !ValidID(e.Name()) {
			continue
		}
		sc, gerr := s.Get(e.Name())
		if gerr != nil {
			bad = append(bad, gerr)
			continue
		}
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, bad, nil
}

// Remove deletes a schedule and its record.
func (s Store) Remove(id string) error {
	d, err := s.dir(id)
	if err != nil {
		return err
	}
	if _, err := os.Stat(d); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", id, ErrNotFound)
	}
	return os.RemoveAll(d)
}

// TaskPath is where a schedule's own task text is kept.
func (s Store) TaskPath(id string) (string, error) {
	d, err := s.dir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "task.txt"), nil
}

// State reads what fires have done; none yet is the zero State.
func (s Store) State(id string) (State, error) {
	d, err := s.dir(id)
	if err != nil {
		return State{}, err
	}
	b, err := os.ReadFile(filepath.Join(d, "state.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("schedule %s's record of its runs: %w", id, err)
	}
	return st, nil
}

// SaveState writes what fires have done.
func (s Store) SaveState(id string, st State) error {
	d, err := s.dir(id)
	if err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(d, "state.json"), b, 0o600)
}

// Claim takes one occurrence for this process: true once, false for every
// later caller, so two triggers for one occurrence run it once
// (invariant 3).
func (s Store) Claim(id string, occurrence time.Time) (bool, error) {
	d, err := s.dir(id)
	if err != nil {
		return false, err
	}
	cd := filepath.Join(d, "claims")
	if err := os.MkdirAll(cd, 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(filepath.Join(cd, strconv.FormatInt(occurrence.Unix(), 10)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, f.Close()
}

// Prune removes the claims of occurrences before before: they only stop a
// duplicate fire of an occurrence that has long been handled.
func (s Store) Prune(id string, before time.Time) {
	d, err := s.dir(id)
	if err != nil {
		return
	}
	entries, _ := os.ReadDir(filepath.Join(d, "claims"))
	for _, e := range entries {
		if n, err := strconv.ParseInt(e.Name(), 10, 64); err == nil && time.Unix(n, 0).Before(before) {
			_ = os.Remove(filepath.Join(d, "claims", e.Name()))
		}
	}
}

// AppendFire adds one occurrence's record.
func (s Store) AppendFire(id string, f Fire) error {
	d, err := s.dir(id)
	if err != nil {
		return err
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	fh, err := os.OpenFile(filepath.Join(d, "fires.ndjson"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := fh.Write(append(b, '\n')); err != nil {
		_ = fh.Close()
		return err
	}
	return fh.Close()
}

// Fires reads a schedule's records, newest last; limit keeps only the
// newest that many (0: all). A line that does not parse is skipped.
func (s Store) Fires(id string, limit int) ([]Fire, error) {
	d, err := s.dir(id)
	if err != nil {
		return nil, err
	}
	fh, err := os.Open(filepath.Join(d, "fires.ndjson"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	var out []Fire
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var f Fire
		if json.Unmarshal([]byte(line), &f) == nil {
			out = append(out, f)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, sc.Err()
}

func writeAtomic(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// Due is what a fire at now should do: run the latest occurrence that fell
// due since the last one handled, and record every earlier one as missed
// (D5: a missed occurrence runs once at the next chance, if that comes
// before the following occurrence). run is zero when nothing is due.
func Due(r Recurrence, st State, now time.Time) (run time.Time, missed []time.Time) {
	from := st.Last
	if from.IsZero() || from.Before(r.Start) {
		from = r.Start.Add(-time.Nanosecond)
	}
	due := r.Between(from, now, 0)
	if len(due) == 0 {
		return time.Time{}, nil
	}
	return due[len(due)-1], due[:len(due)-1]
}
