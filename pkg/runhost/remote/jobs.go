package remote

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/blaktron/agentry-cli/pkg/runhost"
)

// LocalJob is one job this device sent, until it is collected.
type LocalJob struct {
	Job       string `json:"job"`
	Runhost   string `json:"runhost"`
	Ref       string `json:"ref"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Digest    string `json:"digest"`
	// Out is the output folder the run was given, empty for the default.
	Out string `json:"out,omitempty"`
	// DeliverTo is the folder the device copies the deliverables to.
	DeliverTo string `json:"deliverTo,omitempty"`
	Session   bool   `json:"session,omitempty"`
	StartedAt string `json:"startedAt"`
	// Adapter is the adapter the job named, and AdapterAsked the one the
	// person asked for when the runhost runs another in its place.
	Adapter      string `json:"adapter,omitempty"`
	AdapterAsked string `json:"adapterAsked,omitempty"`
	// UnsafeMode is whether the runhost's Unsafe Mode setting was on when
	// the job was sent.
	UnsafeMode bool `json:"unsafeMode,omitempty"`
}

const JobsFile = "runhost-jobs.json"

func LoadJobs(home string) ([]LocalJob, error) {
	var doc struct {
		Jobs []LocalJob `json:"jobs"`
	}
	b, err := os.ReadFile(filepath.Join(home, JobsFile)) //nolint:gosec // the runner home's own file
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", JobsFile, err)
	}
	return doc.Jobs, nil
}

func saveJobs(home string, jobs []LocalJob) error {
	b, err := json.MarshalIndent(struct {
		Jobs []LocalJob `json:"jobs"`
	}{jobs}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(home, "."+JobsFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(home, JobsFile))
}

func PutJob(home string, j LocalJob) error {
	jobs, err := LoadJobs(home)
	if err != nil {
		return err
	}
	jobs = slices.DeleteFunc(jobs, func(x LocalJob) bool { return x.Job == j.Job })
	return saveJobs(home, append(jobs, j))
}

func DropJob(home, id string) {
	jobs, err := LoadJobs(home)
	if err != nil {
		return
	}
	_ = saveJobs(home, slices.DeleteFunc(jobs, func(x LocalJob) bool { return x.Job == id }))
}

func FindJob(home, runhostName, id string) (LocalJob, bool) {
	jobs, _ := LoadJobs(home)
	for _, j := range jobs {
		if j.Job == id && j.Runhost == runhostName {
			return j, true
		}
	}
	return LocalJob{}, false
}

// jobOf is the local record of a job this device sent; a job sent from
// another device, or one whose record is gone, gets one from the runhost's
// own answer.
func JobOf(home string, rec *runhost.Record, id string, ref string) LocalJob {
	if lj, ok := FindJob(home, rec.Name, id); ok {
		return lj
	}
	lj := LocalJob{Job: id, Runhost: rec.Name, Ref: ref}
	if parsed, err := ParseExactRef(ref); err == nil {
		lj.Namespace, lj.Name, lj.Version = parsed[0], parsed[1], parsed[2]
	}
	return lj
}

// ParseExactRef splits ns/name@version.
func ParseExactRef(ref string) ([3]string, error) {
	var out [3]string
	slash := bytes.IndexByte([]byte(ref), '/')
	at := bytes.LastIndexByte([]byte(ref), '@')
	if slash <= 0 || at <= slash+1 || at == len(ref)-1 {
		return out, fmt.Errorf("%q is not ns/name@version", ref)
	}
	out[0], out[1], out[2] = ref[:slash], ref[slash+1:at], ref[at+1:]
	return out, nil
}
