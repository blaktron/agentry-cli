// Package pastruns reads the runs a person has made from their output
// folders: every folder directly under an output directory (./agentry-out
// by default) that holds a result.json, read as the run's envelope, with its
// timing from the receipts' meta.json. Nothing here changes a folder.
package pastruns

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// DefaultDir is where agentry run writes when --out is not given, relative
// to the working folder.
const DefaultDir = "agentry-out"

// Run is one past run as its folder records it.
type Run struct {
	// Dir is the run's output folder, as given (relative when the output
	// directory was).
	Dir string `json:"dir"`
	// RunID, Ref, Status, ExitCode and ExitClass are the envelope's; the
	// class's words come from the exit-class table.
	RunID            string `json:"runId"`
	Ref              string `json:"ref"`
	Status           string `json:"status"`
	ExitCode         int    `json:"exitCode"`
	ExitClass        string `json:"exitClass"`
	ExitClassMeaning string `json:"exitClassMeaning"`
	RetryLikelyHelps bool   `json:"retryLikelyHelps"`
	// Error is the envelope's error, on a failed run.
	Error string `json:"error,omitempty"`
	// Started and Ended are from the receipts' meta.json; when it is
	// missing, both are the time result.json was written.
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended"`
	// WallMS is the run's measured duration, from the envelope's usage.
	WallMS int64 `json:"wallMs"`
	// Deliverables are the run's deliverables by path, as the envelope lists
	// them; empty for a run that failed or listed none.
	Deliverables []string `json:"deliverables,omitempty"`
}

// Outcome is the run's outcome in one word: the class name, or ok.
func (r Run) Outcome() string {
	if r.Status == "ok" {
		return "ok"
	}
	return r.ExitClass
}

// List reads the runs under dir, newest first. A folder without a
// result.json, or with one that is not an envelope, is not a run and is
// left out. A dir that does not exist lists nothing and is no error: the
// person has made no run from here.
func List(dir string) ([]Run, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var runs []Run
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		r, ok := read(filepath.Join(dir, e.Name()))
		if ok {
			runs = append(runs, r)
		}
	}
	sort.SliceStable(runs, func(i, j int) bool {
		if !runs[i].Started.Equal(runs[j].Started) {
			return runs[i].Started.After(runs[j].Started)
		}
		return runs[i].Dir > runs[j].Dir
	})
	return runs, nil
}

// Read is the run in one folder, or an error saying why the folder is not
// a run.
func Read(dir string) (Run, error) {
	r, ok := read(dir)
	if !ok {
		return Run{}, fmt.Errorf("%s is not a run folder: it has no result.json a run wrote", dir)
	}
	return r, nil
}

// read is the run in dir; false when dir holds no envelope.
func read(dir string) (Run, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "result.json")) //nolint:gosec // the person's own run folder
	if err != nil {
		return Run{}, false
	}
	var env receipt.Envelope
	if json.Unmarshal(b, &env) != nil || env.Agentry == "" {
		return Run{}, false
	}
	class := receipt.ExitClassOf(env.ExitCode)
	r := Run{
		Dir: dir, RunID: env.RunID, Ref: env.Ref, Status: env.Status, ExitCode: env.ExitCode,
		ExitClass: class.Name, ExitClassMeaning: class.Meaning, RetryLikelyHelps: class.RetryLikelyHelps,
		Error: env.Error, WallMS: env.Usage.WallMS,
	}
	if env.Deliverables != nil {
		for _, f := range env.Deliverables.Files {
			r.Deliverables = append(r.Deliverables, f.Path)
		}
	}
	r.Started, r.Ended = times(dir)
	return r, true
}

// times is when the run started and ended, from the receipts' meta.json,
// else the envelope file's modification time for both.
func times(dir string) (started, ended time.Time) {
	var meta struct {
		Started time.Time `json:"started"`
		Ended   time.Time `json:"ended"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, "receipts", "meta.json")); err == nil { //nolint:gosec // the run's own receipts
		if json.Unmarshal(b, &meta) == nil && !meta.Started.IsZero() {
			if meta.Ended.IsZero() {
				meta.Ended = meta.Started
			}
			return meta.Started, meta.Ended
		}
	}
	st, err := os.Stat(filepath.Join(dir, "result.json"))
	if err != nil {
		return time.Time{}, time.Time{}
	}
	return st.ModTime(), st.ModTime()
}

// Find is the folder the name means: a run folder's path as given, or the
// name of a folder under dir, or a run id (whole or its first characters)
// of a run under dir. One match is answered; none, or more than one, is an
// error that says so.
func Find(dir, name string) (Run, error) {
	if name == "" {
		return Run{}, errors.New("name the run: its folder, or its run id")
	}
	if st, err := os.Stat(name); err == nil && st.IsDir() {
		return Read(name)
	}
	runs, err := List(dir)
	if err != nil {
		return Run{}, err
	}
	var matched []Run
	for _, r := range runs {
		if filepath.Base(r.Dir) == name || r.RunID == name {
			return r, nil
		}
		if len(name) >= 4 && (strings.HasPrefix(r.RunID, name) || strings.HasSuffix(filepath.Base(r.Dir), name)) {
			matched = append(matched, r)
		}
	}
	switch len(matched) {
	case 1:
		return matched[0], nil
	case 0:
		return Run{}, fmt.Errorf("no run %q under %s: agentry runs lists them", name, dir)
	default:
		var names []string
		for _, r := range matched {
			names = append(names, filepath.Base(r.Dir))
		}
		return Run{}, fmt.Errorf("%q names %d runs under %s (%s): give more of the id", name, len(matched), dir, strings.Join(names, ", "))
	}
}
