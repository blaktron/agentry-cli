package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/payload"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// A schedule on a runhost is the job every one of its occurrences runs a
// copy of, sent once with everything it carries: the runhost keeps it in its
// encrypted store and runs it with no device present. Entries says what the
// store will keep, for the consent that names each of them and the server.

// Entries are what a schedule of j keeps in the runhost's store, besides
// the release.
func Entries(j *job.Job) []runhost.ScheduleEntry {
	var out []runhost.ScheduleEntry
	add := func(kind string, names []string) {
		slices.Sort(names)
		for _, n := range names {
			out = append(out, runhost.ScheduleEntry{Kind: kind, Name: n})
		}
	}
	if j.Run.ModelKey != "" {
		out = append(out, runhost.ScheduleEntry{Kind: "model-key", Name: j.Run.Model.Provider})
	}
	var names []string
	for k := range j.Run.Secrets {
		names = append(names, k)
	}
	add("secret", names)
	names = nil
	for _, d := range j.Run.Destinations {
		names = append(names, d.Name)
	}
	add("destination", names)
	names = nil
	for id := range j.Run.TargetHosts {
		names = append(names, id)
	}
	add("target-host", names)
	names = nil
	for k := range j.Run.SourceTokens {
		names = append(names, k)
	}
	add("source-token", names)
	names = nil
	for server := range j.Run.Connections {
		names = append(names, server)
	}
	add("connection", names)
	return out
}

// PutSchedule sends the schedule h with its job, its release when the
// runhost asks for it, and its files. say takes the runhost's narration.
func PutSchedule(ctx context.Context, c *runhost.Client, h runhost.ScheduleHeader, j *job.Job, archive []byte, files *payload.Packed, say func(string)) (runhost.ScheduleInfo, error) {
	var info runhost.ScheduleInfo
	answered := make(chan string, 1)
	stop := make(chan struct{})
	defer close(stop)
	a, err := c.Call(ctx, "schedule", []string{"put"}, func(w io.Writer) error {
		line, err := json.Marshal(h)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
		if err := job.Write(w, j); err != nil {
			return err
		}
		select {
		case got := <-answered:
			if got == runhost.ReleaseNeeded {
				if _, err := w.Write(archive); err != nil {
					return err
				}
			}
		case <-stop:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
		if files != nil {
			f, err := files.Open()
			if err != nil {
				return err
			}
			n, err := io.Copy(w, f)
			_ = f.Close()
			if err == nil && n != files.Size {
				err = fmt.Errorf("the schedule's files are %d bytes, not the %d the job names", n, files.Size)
			}
			return err
		}
		return nil
	}, func(f runhost.Frame) error {
		switch f.Type {
		case runhost.FrameOut:
			if say != nil {
				say(f.Text)
			}
		case runhost.FrameRelease:
			select {
			case answered <- f.Text:
			default:
				return errors.New("the runhost answered the schedule twice")
			}
		}
		return nil
	})
	if err != nil {
		return info, err
	}
	if a.Code != 0 {
		return info, &Refusal{Code: a.Code, Reason: nonEmptyOr(a.Error, "the runhost did not keep the schedule")}
	}
	return info, json.Unmarshal(a.Result, &info)
}

// TakesSchedules says whether the runhost takes schedules (its store's key
// sealed by its systemd), as its status says, and why not.
func TakesSchedules(ctx context.Context, c *runhost.Client) (bool, error) {
	return capable(ctx, c, "schedules")
}

// MovesScheduleModels says whether the runhost's CLI moves a schedule to
// another of its saved models (MoveSchedule); an older one does not.
func MovesScheduleModels(ctx context.Context, c *runhost.Client) (bool, error) {
	return capable(ctx, c, "scheduleModels")
}

// capable says whether the runhost's status names the capability.
func capable(ctx context.Context, c *runhost.Client, name string) (bool, error) {
	a, err := c.Ask(ctx, "status", nil, nil)
	if err != nil {
		return false, err
	}
	var st struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if a.Code != 0 || json.Unmarshal(a.Result, &st) != nil {
		return false, errors.New(nonEmptyOr(a.Error, "the runhost did not say its status"))
	}
	return st.Capabilities[name], nil
}

// Schedules lists the runhost's schedules.
func Schedules(ctx context.Context, c *runhost.Client) ([]runhost.ScheduleInfo, error) {
	var list []runhost.ScheduleInfo
	err := askSchedule(ctx, c, []string{"list"}, &list)
	return list, err
}

// RemoveSchedule takes schedule id off the runhost, with what it kept.
func RemoveSchedule(ctx context.Context, c *runhost.Client, id string) (runhost.ScheduleInfo, error) {
	var info runhost.ScheduleInfo
	err := askSchedule(ctx, c, []string{"remove", id}, &info)
	return info, err
}

// RunSchedule runs an occurrence of schedule id now; its job is collected
// as any other.
func RunSchedule(ctx context.Context, c *runhost.Client, id string) (runhost.ScheduleFire, error) {
	var f runhost.ScheduleFire
	err := askSchedule(ctx, c, []string{"run", id}, &f)
	return f, err
}

// MoveSchedule has schedule id run on the runhost's saved model ref, by
// name or id, from its next occurrence, or on the runhost's default at each
// occurrence when ref is "default". It sends no credential: the runhost
// reads the model and its key from its own list and store at each
// occurrence. say takes the runhost's narration; a refusal keeps the
// runhost's exit code.
func MoveSchedule(ctx context.Context, c Caller, id, ref string, say func(string)) (runhost.ScheduleInfo, error) {
	var info runhost.ScheduleInfo
	a, err := c.Call(ctx, "schedule", []string{"model", id, ref}, nil, func(f runhost.Frame) error {
		if f.Type == runhost.FrameOut && say != nil {
			say(f.Text)
		}
		return nil
	})
	if err != nil {
		return info, engine.Classify(engine.ExitGateway, err)
	}
	if a.Code != 0 {
		return info, engine.Classify(a.Code, errors.New(nonEmptyOr(a.Error, "the runhost did not move the schedule")))
	}
	return info, json.Unmarshal(a.Result, &info)
}

func askSchedule(ctx context.Context, c *runhost.Client, args []string, out any) error {
	a, err := c.Ask(ctx, "schedule", args, nil)
	if err != nil {
		return err
	}
	if a.Code != 0 {
		return errors.New(nonEmptyOr(a.Error, "the runhost refused"))
	}
	return json.Unmarshal(a.Result, out)
}
