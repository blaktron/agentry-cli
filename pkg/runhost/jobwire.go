package runhost

import (
	"encoding/json"
	"time"
)

// JobResult is the job and attach verbs' answer.
type JobResult struct {
	Job      string `json:"job"`
	ExitCode int    `json:"exitCode"`
	// Done says the job has ended: its sealed output and envelope wait for
	// collect. Next is the index after the last frame the answer carried.
	Done bool `json:"done"`
	Next int  `json:"next,omitempty"`
}

// SessionInput is one line of a session's input after the job: a command
// for the session protocol, or a chunk of a file the device adds.
type SessionInput struct {
	Command *json.RawMessage `json:"command,omitempty"`
	File    *SessionFile     `json:"file,omitempty"`
}

// SessionFile is a chunk of a file a device adds to a session: More says
// another chunk of the same file follows.
type SessionFile struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
	More bool   `json:"more,omitempty"`
}

const (
	// MaxSessionFile bounds one file a device adds to a session.
	MaxSessionFile = 512 << 20
	// MaxEnvelope bounds a run's envelope, the runhost CLI's stdout.
	MaxEnvelope = 16 << 20
	// OutputKeep is how long a finished job's sealed output waits for
	// collection.
	OutputKeep = 7 * 24 * time.Hour
)

// SealedEnvelopeJob is the job string a job's envelope is sealed under, so
// it never opens as the job's output.
func SealedEnvelopeJob(id string) string { return id + ":envelope" }
