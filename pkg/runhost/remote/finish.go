package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// FinishOptions are the device's choices in collecting a finished job.
type FinishOptions struct {
	// Home is the runner home whose job records hold the job.
	Home string
	// OutDir is the output folder for the run id the envelope names: the
	// folder the job was sent with, or the client's default.
	OutDir func(runID string) (string, error)
	// Warn tells the person something that did not stop the collection.
	Warn func(string)
	// CollectHint names how the person collects the job later, for an
	// error that leaves it on the runhost.
	CollectHint string
}

func (o FinishOptions) warn(s string) {
	if o.Warn != nil {
		o.Warn(s)
	}
}

// Finish collects a finished job: its sealed envelope and its sealed
// output, opened into the output folder; the envelope the runhost's CLI
// printed is this device's record, with the runhost it ran on; the
// deliverables are copied to the device's deliver folder; the runhost is
// told to delete the job, and the device forgets it. A run that failed
// answers its envelope and its class.
func Finish(ctx context.Context, c *runhost.Client, rec *runhost.Record, keys *runhost.DeviceKeys, lj LocalJob, code int, o FinishOptions) (*receipt.Envelope, error) {
	keep := int(runhost.OutputKeep.Hours() / 24)
	raw, err := CollectEnvelope(ctx, c, lj.Job, rec.DeviceID, keys)
	if err != nil {
		return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("collect the run's envelope from %s: %w (the job stays there, sealed, for %d days%s)", rec.Name, err, keep, o.CollectHint))
	}
	// The envelope is the runhost CLI's stdout; a run that printed none still
	// has its output collected, and fails as the CLI's exit says.
	var env receipt.Envelope
	if json.Unmarshal(raw, &env) != nil || env.Agentry == "" {
		env = receipt.Envelope{Status: "error", ExitCode: code, Ref: lj.Ref, Digest: lj.Digest,
			Error: fmt.Sprintf("the run on %s gave no envelope", rec.Name)}
	}
	// The CLI's exit status is what the run ended in, whatever its stdout
	// says.
	if env.ExitCode != code {
		env.ExitCode = code
		if code != 0 {
			env.Status = "error"
		}
	}
	runID := env.RunID
	if runID == "" {
		runID = lj.Job[:12]
	}
	outDir, err := o.OutDir(runID)
	if err != nil {
		return nil, err
	}
	if err := CollectOutput(ctx, c, lj.Job, rec.DeviceID, keys, outDir); err != nil {
		return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("collect the run's output from %s: %w (it stays there, sealed, for %d days%s)", rec.Name, err, keep, o.CollectHint))
	}
	if _, err := c.Ask(ctx, "collect", []string{lj.Job, "done"}, nil); err != nil {
		o.warn(fmt.Sprintf("warning: %s keeps the sealed output of job %s until it expires: %v", rec.Name, lj.Job, err))
	}
	Envelope(&env, rec, outDir, lj.Job)
	env.Runhost.Adapter, env.Runhost.AdapterAsked, env.Runhost.UnsafeMode = lj.Adapter, lj.AdapterAsked, lj.UnsafeMode
	if w := Deliver(&env, outDir, lj.DeliverTo); w != "" {
		o.warn(w)
	}
	if err := env.Write(outDir); err != nil {
		return nil, err
	}
	if o.Home != "" {
		DropJob(o.Home, lj.Job)
	}
	if env.ExitCode != 0 {
		return &env, engine.Classify(env.ExitCode, errors.New(nonEmptyOr(env.Error, "the run on "+rec.Name+" failed")))
	}
	return &env, nil
}
