package remote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/clirelease"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/payload"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// errDetached ends a job's call once the runhost has said it runs the job,
// for --detach.
var errDetached = errors.New("detached")

// Start sends the job, and its release and files when the runhost asks
// for them, over one session, and follows the job's frames. started says
// whether the runhost said it runs the job: an error after that is a
// connection lost while the job runs on, which next and attach resume. With
// detach, it returns as soon as the job runs. each, when set, takes the
// frames a session's driver reads (events), and moreInput, when set, writes
// the session's commands after the files.
func Start(ctx context.Context, c *runhost.Client, j *job.Job, archive []byte, files *payload.Packed, detach bool,
	say func(string), each func(runhost.Frame), moreInput func(io.Writer) error) (res runhost.JobResult, started bool, next int, err error) {
	answered := make(chan string, 1)
	stop := make(chan struct{})
	defer close(stop)
	send := func(w io.Writer) error {
		if err := job.Write(w, j); err != nil {
			return err
		}
		select {
		case a := <-answered:
			if a == runhost.ReleaseNeeded {
				if _, err := w.Write(archive); err != nil {
					return err
				}
			}
		case <-stop:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
		// The run's files follow the release, exactly the size the job names.
		if files != nil {
			f, err := files.Open()
			if err != nil {
				return err
			}
			n, err := io.Copy(w, f)
			_ = f.Close()
			if err == nil && n != files.Size {
				err = fmt.Errorf("the run's files are %d bytes, not the %d the job names", n, files.Size)
			}
			if err != nil {
				return err
			}
		}
		if moreInput != nil {
			return moreInput(w)
		}
		// The input stays open while the device follows the job: its end is
		// how the runhost knows the device left.
		select {
		case <-stop:
		case <-ctx.Done():
		}
		return nil
	}
	frame := func(f runhost.Frame) error {
		switch f.Type {
		case runhost.FrameOut:
			next++
			if say != nil {
				say(f.Text)
			}
		case runhost.FrameEvent:
			next++
			if each != nil {
				each(f)
			}
		case runhost.FrameStarted:
			if f.Text != j.ID {
				return fmt.Errorf("the runhost started job %q, not this run's", f.Text)
			}
			started = true
			if detach {
				return errDetached
			}
		case runhost.FrameRelease:
			if f.Text != runhost.ReleaseNeeded && f.Text != runhost.ReleaseHeld {
				return fmt.Errorf("the runhost answered the job with %q", f.Text)
			}
			select {
			case answered <- f.Text:
			default:
				return errors.New("the runhost answered the job twice")
			}
		}
		return nil
	}
	a, err := c.Call(ctx, "job", nil, send, frame)
	if errors.Is(err, errDetached) {
		return runhost.JobResult{Job: j.ID}, true, next, nil
	}
	if err != nil {
		return runhost.JobResult{}, started, next, err
	}
	if len(a.Result) == 0 || json.Unmarshal(a.Result, &res) != nil || !res.Done {
		if !started {
			return runhost.JobResult{}, false, next, &Refusal{Code: a.Code, Reason: nonEmptyOr(a.Error, "no reason given")}
		}
		return runhost.JobResult{}, true, next, fmt.Errorf("the runhost ended the job's answer without its result (exit %d): %s", a.Code, nonEmptyOr(a.Error, "no reason given"))
	}
	if res.Job != j.ID {
		return runhost.JobResult{}, true, next, fmt.Errorf("the runhost answered for job %q, not this run's", res.Job)
	}
	return res, true, next, nil
}

// Follow attaches to a job from frame index from and follows it to its
// end.
func Follow(ctx context.Context, c *runhost.Client, id string, from int, say func(string), each func(runhost.Frame)) (runhost.JobResult, int, error) {
	next := from
	// The input stays open while the device follows: its end is how the
	// runhost knows the device left.
	stop := make(chan struct{})
	defer close(stop)
	hold := func(io.Writer) error {
		select {
		case <-stop:
		case <-ctx.Done():
		}
		return nil
	}
	a, err := c.Call(ctx, "attach", []string{id, fmt.Sprint(from)}, hold, func(f runhost.Frame) error {
		switch f.Type {
		case runhost.FrameOut:
			next++
			if say != nil {
				say(f.Text)
			}
		case runhost.FrameEvent:
			next++
			if each != nil {
				each(f)
			}
		}
		return nil
	})
	if err != nil {
		return runhost.JobResult{}, next, err
	}
	var res runhost.JobResult
	if len(a.Result) == 0 || json.Unmarshal(a.Result, &res) != nil || !res.Done {
		return runhost.JobResult{}, next, &Refusal{Code: a.Code, Reason: nonEmptyOr(a.Error, "the runhost gave no result")}
	}
	return res, next, nil
}

// ReattachWaits are how long the device waits before each attempt to
// reach a runhost again after losing it mid-job.
var ReattachWaits = []time.Duration{2 * time.Second, 10 * time.Second, 30 * time.Second}

// Reattach reaches the runhost again after the connection was lost while
// the job ran, and follows the job from where the device lost it. It answers
// the client it reached the runhost with.
func Reattach(ctx context.Context, rec *runhost.Record, keys *runhost.DeviceKeys, id string, from int, errOut io.Writer, say func(string), each func(runhost.Frame)) (runhost.JobResult, *runhost.Client, error) {
	var last error
	for _, wait := range ReattachWaits {
		_, _ = fmt.Fprintf(errOut, "lost %s mid-run; the job goes on there. Reaching it again in %s…\n", rec.Name, wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return runhost.JobResult{}, nil, ctx.Err()
		}
		c, err := runhost.DialRunhost(ctx, *rec, keys)
		if err != nil {
			last = err
			continue
		}
		res, n, err := Follow(ctx, c, id, from, say, each)
		from = n
		if err == nil {
			return res, c, nil
		}
		_ = c.Close()
		last = err
		var r *Refusal
		if errors.As(err, &r) {
			return runhost.JobResult{}, nil, err
		}
	}
	return runhost.JobResult{}, nil, last
}

// Refusal is a job the runhost answered without running it, with the
// exit class it gave.
type Refusal struct {
	Code   int
	Reason string
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("the runhost refused the run (exit %d): %s", r.Code, r.Reason)
}

// SendClass is the exit class of a job that did not run: a refusal the
// runhost classed as usage, verify or policy keeps that class, and anything
// else, a transport failure among them, is the gateway's.
func SendClass(err error) int {
	var r *Refusal
	if errors.As(err, &r) {
		switch r.Code {
		case engine.ExitUsage, engine.ExitVerify, engine.ExitPolicy:
			return r.Code
		}
	}
	return engine.ExitGateway
}

// NewJobID is a fresh job id.
func NewJobID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// Version is the version verb's answer.
type Version struct {
	Version string `json:"version"`
	// Arch is the runhost's architecture in Go's words.
	Arch string `json:"arch,omitempty"`
	// UnsafeMode is the runhost's own Unsafe Mode setting.
	UnsafeMode bool `json:"unsafeMode"`
}

// VersionSkew is the refusal of a run on runhost name, whose CLI is
// server, from a device whose CLI is device: both versions, and what brings
// them together. A runhost follows the newest of its devices, so an older
// device is the one to update.
func VersionSkew(name, server, device string) error {
	if cmp, ok := clirelease.CompareVersions(server, device); ok && cmp > 0 {
		return &SkewError{fmt.Sprintf("%s runs CLI %s and this device %s; a run needs the same version on both, and a runhost follows the newest of its devices: update this device's CLI to %s", name, server, device, server)}
	}
	return &SkewError{fmt.Sprintf("%s runs CLI %s and this device %s; a run needs the same version on both: agentry runhost update %s brings it to %s", name, server, device, name, device)}
}

// SkewError is a refusal for a version skew between a runhost and this
// device, which only an update ends.
type SkewError struct{ msg string }

func (e *SkewError) Error() string { return e.msg }

// SameVersion refuses a runhost whose CLI is not this device's
// version: the run's plan, policy and envelope are this version's. It
// returns what the runhost said of itself.
func SameVersion(ctx context.Context, c *runhost.Client, name string) (Version, error) {
	a, err := c.Ask(ctx, "version", nil, nil)
	if err != nil {
		return Version{}, fmt.Errorf("ask %s its version: %w", name, err)
	}
	var v Version
	if a.Code != 0 || json.Unmarshal(a.Result, &v) != nil {
		return Version{}, fmt.Errorf("%s did not say its version: %s", name, a.Error)
	}
	if v.Version != version.Version {
		return Version{}, VersionSkew(name, v.Version, version.Version)
	}
	return v, nil
}

// ModelKey is the device policy's model key, which the run carries in
// flight: the variable the policy names, else its keychain entry. A model
// paid with Agentry credits or a plan's sign-in carries none: the runhost
// pays with the credits token, or the sign-in, it keeps (agentry runhost
// model). A model a runhost cannot be handed this way is refused here.
func ModelKey(pol *policy.Policy) (string, error) {
	switch {
	case pol.Model.Provider == policy.ProviderAgentry:
		return "", nil
	case policy.IsModelSignIn(pol.Model.APIKeyKind):
		// The runhost keeps the plan's sign-in itself (agentry runhost
		// model --sign-in): nothing of this computer's travels.
		return "", nil
	case pol.Model.APIKeyKind == policy.KeyKindSubscription:
		return "", errors.New("a Claude subscription token runs only the installed claude, which a runhost does not run: sign the runhost in with agentry runhost model <name> --sign-in claude, and choose your Claude subscription with agentry setup")
	}
	key, err := modelkey.Personal(pol.Model.APIKeyEnv, pol.Model.APIKeySource, pol.Model.KeyProvider())
	if err != nil {
		return "", err
	}
	if key == "" {
		return "", fmt.Errorf("no key for the model %s/%s: set $%s, or keep it in the keychain with agentry setup", pol.Model.Provider, pol.Model.Name, pol.Model.APIKeyEnv)
	}
	return key, nil
}

// nonEmptyOr is s, or def when s is empty.
func nonEmptyOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
