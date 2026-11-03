package runhost

import (
	"encoding/json"
	"time"
)

// The schedule verb's wire. `schedule put` takes a ScheduleHeader line, then
// the job document every occurrence runs a copy of, then the release when the
// service asks for it (as a job's), then the job's payload. `schedule list`
// answers a []ScheduleInfo; `schedule remove <id>` and `schedule run <id>`
// name one.

// ScheduleEntry is one thing a schedule keeps in the runhost's store, as
// the consent named it.
type ScheduleEntry struct {
	// Kind is "secret", "model-key", "destination", "target-host",
	// "connection" or "source-token".
	Kind string `json:"kind"`
	// Name is what the person knows it by: a secret's variable, a
	// provider, a destination's or a server's name, a host.
	Name string `json:"name"`
}

// ScheduleHeader precedes a schedule's job document on `schedule put`.
type ScheduleHeader struct {
	// Name is the schedule's name; When is its recurrence, as
	// schedule.Recurrence marshals it.
	Name string          `json:"name"`
	When json.RawMessage `json:"when"`
	// Entries are what the schedule keeps in the store, besides the
	// release: the consent's list.
	Entries []ScheduleEntry `json:"entries"`
	// Connections are the release's OAuth connections, handed to the
	// runhost to refresh itself (connection.Handed, as JSON).
	Connections []json.RawMessage `json:"connections,omitempty"`
}

// ScheduleInfo is one schedule as the service lists it: never a secret.
type ScheduleInfo struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Ref     string          `json:"ref"`
	Device  string          `json:"device"`
	When    json.RawMessage `json:"when"`
	Next    *time.Time      `json:"next,omitempty"`
	Ended   bool            `json:"ended,omitempty"`
	Paused  bool            `json:"paused,omitempty"`
	Created time.Time       `json:"created"`
	Entries []ScheduleEntry `json:"entries"`
	// Fires are its latest occurrences, newest first: each run's job, which
	// a device collects as it collects its own.
	Fires []ScheduleFire `json:"fires,omitempty"`
}

// ScheduleFire is one occurrence of a runhost schedule.
type ScheduleFire struct {
	Occurrence time.Time `json:"occurrence"`
	Outcome    string    `json:"outcome"`
	Job        string    `json:"job,omitempty"`
	ExitCode   int       `json:"exitCode"`
	Message    string    `json:"message,omitempty"`
}
