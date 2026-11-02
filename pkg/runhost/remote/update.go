package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/clirelease"
	"github.com/blaktron/agentry-cli/pkg/runhost/setup"
)

// A runhost runs this device's exact CLI version, so after every update of
// the CLI the device updates each runhost it pairs with: over the run key,
// with the signed release it checked itself, which the runhost checks again.
// A runhost on a newer CLI than this device stays as it is: the device is
// the one to update. What only the admin sign-in can change is reported as
// needing attention, never attempted.

// UpdateOptions are what Update needs.
type UpdateOptions struct {
	// Version is this device's CLI version; empty is this build's.
	Version string
	// Keys are the update-signing keys a release is checked against.
	Keys *clirelease.KeySet
	// HTTP downloads the release and gVisor.
	HTTP *http.Client
	// FetchCLI and FetchGVisor default to those downloads, each checked;
	// tests replace them.
	FetchCLI    func(ctx context.Context, version, arch string) (*clirelease.Binary, error)
	FetchGVisor func(ctx context.Context, arch string) ([]byte, error)
	// Say narrates the runhost's side, one line at a time.
	Say func(string)
	// Restart bounds the wait for the service to restart into its new CLI;
	// zero is a minute.
	Restart time.Duration
}

// UpdateOutcome is what Update did to one runhost.
type UpdateOutcome struct {
	Name string `json:"name"`
	// From and To are the runhost's CLI before and after.
	From string `json:"from"`
	To   string `json:"to"`
	// Updated is true when the runhost took a new CLI.
	Updated bool `json:"updated"`
	// WaitsForJobs is how many running jobs the runhost finishes before it
	// restarts into the new CLI; To is its version already.
	WaitsForJobs int `json:"waitsForJobs,omitempty"`
	// GVisor is the runhost's gVisor release after, and GVisorUpdated true
	// when it changed.
	GVisor        string `json:"gvisor,omitempty"`
	GVisorUpdated bool   `json:"gvisorUpdated,omitempty"`
	// DeviceBehind is the runhost's CLI when it is newer than this
	// device's: update this device before running there.
	DeviceBehind string `json:"deviceBehind,omitempty"`
	// NeedsAttention says what only the admin sign-in can change.
	NeedsAttention []string `json:"needsAttention,omitempty"`
}

// Update brings the runhost of rec to this device's CLI version and its
// gVisor release, then has its doctor check it. It updates rec's versions;
// the caller saves the records.
func Update(ctx context.Context, rec *runhost.Record, keys *runhost.DeviceKeys, o UpdateOptions) (UpdateOutcome, error) {
	out := UpdateOutcome{Name: rec.Name}
	if o.Version == "" {
		o.Version = version.Version
	}
	if !clirelease.ValidVersion(o.Version) {
		return out, setup.ErrNotARelease
	}
	if o.HTTP == nil {
		o.HTTP = http.DefaultClient
	}
	if o.FetchCLI == nil {
		o.FetchCLI = func(ctx context.Context, v, arch string) (*clirelease.Binary, error) {
			return clirelease.Fetch(ctx, o.HTTP, o.Keys, v, arch)
		}
	}
	if o.FetchGVisor == nil {
		o.FetchGVisor = func(ctx context.Context, arch string) ([]byte, error) {
			return setup.FetchGVisorArchive(ctx, o.HTTP, arch)
		}
	}
	if o.Restart <= 0 {
		o.Restart = time.Minute
	}
	say := func(s string) {
		if o.Say != nil {
			o.Say(s)
		}
	}
	repair := fmt.Sprintf("agentry runhost add %s ssh://<admin>@%s, with the admin sign-in, repairs it", rec.Name, rec.Host)

	c, err := runhost.DialRunhost(ctx, *rec, keys)
	if err != nil {
		return out, err
	}
	defer func() { _ = c.Close() }()
	v, err := askVersion(ctx, c, rec.Name)
	if err != nil {
		return out, err
	}
	out.From, out.To = v.Version, v.Version
	cmp, ok := clirelease.CompareVersions(o.Version, v.Version)
	switch {
	case !ok:
		return out, fmt.Errorf("%s runs CLI %q, which this device cannot order against its own %s", rec.Name, v.Version, o.Version)
	case cmp < 0:
		out.DeviceBehind = v.Version
		return out, nil
	case cmp > 0:
		arch := nonEmptyOr(v.Arch, rec.Arch)
		bin, err := o.FetchCLI(ctx, o.Version, arch)
		if err != nil {
			return out, err
		}
		up, err := sendUpdate(ctx, c, []string{"cli", o.Version}, runhost.UpdateHeader{Name: bin.Name, Sums: bin.Sums, Sig: bin.Sig, Size: int64(len(bin.Bytes))}, bin.Bytes, say)
		var unanswered *notAnswered
		if errors.As(err, &unanswered) {
			out.NeedsAttention = append(out.NeedsAttention, fmt.Sprintf("its CLI %s takes no update over the run key: %s", v.Version, repair))
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out.To, out.Updated, out.WaitsForJobs = up.To, up.Installed, up.WaitsForJobs
		rec.CLIVersion = up.To
		if up.WaitsForJobs > 0 {
			// The new CLI runs once those jobs finish; the rest of the
			// update waits for the next one.
			return out, nil
		}
		_ = c.Close()
		if c, err = waitForVersion(ctx, rec, keys, o.Version, o.Restart); err != nil {
			return out, err
		}
		defer func() { _ = c.Close() }()
	}

	var st struct {
		GVisor string `json:"gvisor"`
	}
	if a, err := c.Ask(ctx, "status", nil, nil); err == nil && a.Code == 0 {
		_ = json.Unmarshal(a.Result, &st)
	}
	out.GVisor = st.GVisor
	if st.GVisor != setup.GVisorRelease {
		archive, err := o.FetchGVisor(ctx, unameArch(nonEmptyOr(v.Arch, rec.Arch)))
		if err != nil {
			return out, err
		}
		up, err := sendUpdate(ctx, c, []string{"gvisor", setup.GVisorRelease}, runhost.UpdateHeader{Size: int64(len(archive))}, archive, say)
		if err != nil {
			return out, err
		}
		out.GVisor, out.GVisorUpdated = up.To, up.Installed
		rec.GVisor = up.To
	}

	doc, err := c.Ask(ctx, "doctor", nil, nil)
	if err != nil {
		return out, err
	}
	if doc.Code != 0 {
		out.NeedsAttention = append(out.NeedsAttention, fmt.Sprintf("its doctor reports %s: %s", nonEmptyOr(doc.Error, "a failed check"), repair))
	}
	return out, nil
}

// notAnswered is a runhost whose service does not know the verb asked.
type notAnswered struct{ msg string }

func (e *notAnswered) Error() string { return e.msg }

func askVersion(ctx context.Context, c *runhost.Client, name string) (Version, error) {
	a, err := c.Ask(ctx, "version", nil, nil)
	if err != nil {
		return Version{}, fmt.Errorf("ask %s its version: %w", name, err)
	}
	var v Version
	if a.Code != 0 || json.Unmarshal(a.Result, &v) != nil {
		return Version{}, fmt.Errorf("%s did not say its version: %s", name, a.Error)
	}
	return v, nil
}

// sendUpdate sends one update: its header line and its bytes.
func sendUpdate(ctx context.Context, c *runhost.Client, args []string, h runhost.UpdateHeader, body []byte, say func(string)) (runhost.Updated, error) {
	var up runhost.Updated
	a, err := c.Call(ctx, "update", args, func(w io.Writer) error {
		line, err := json.Marshal(h)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
		_, err = w.Write(body)
		return err
	}, func(f runhost.Frame) error {
		if f.Type == runhost.FrameOut {
			say(f.Text)
		}
		return nil
	})
	if err != nil {
		return up, err
	}
	if a.Code == 2 && strings.Contains(a.Error, "does not answer") {
		return up, &notAnswered{a.Error}
	}
	if a.Code != 0 {
		return up, fmt.Errorf("the runhost refused the update: %s", nonEmptyOr(a.Error, "no reason given"))
	}
	if err := json.Unmarshal(a.Result, &up); err != nil {
		return up, fmt.Errorf("the update's answer: %w", err)
	}
	return up, nil
}

// waitForVersion dials the runhost until its service answers as want, for
// at most limit.
func waitForVersion(ctx context.Context, rec *runhost.Record, keys *runhost.DeviceKeys, want string, limit time.Duration) (*runhost.Client, error) {
	deadline := time.Now().Add(limit)
	last := ""
	for {
		c, err := runhost.DialRunhost(ctx, *rec, keys)
		if err == nil {
			v, verr := askVersion(ctx, c, rec.Name)
			if verr == nil && v.Version == want {
				return c, nil
			}
			_ = c.Close()
			last = v.Version
			if verr != nil {
				last = verr.Error()
			}
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s took CLI %s but its service has not restarted into it (%s)", rec.Name, want, last)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// unameArch is uname -m's name for a Go architecture.
func unameArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return goarch
}

// Unpair takes this device off the runhost of rec: the runhost drops its
// run key and sealing key, and revokes the credits token this device
// minted. The caller then drops the device's keys and record.
func Unpair(ctx context.Context, rec runhost.Record, keys *runhost.DeviceKeys, say func(string)) (Unpaired, error) {
	var out Unpaired
	c, err := runhost.DialRunhost(ctx, rec, keys)
	if err != nil {
		return out, err
	}
	defer func() { _ = c.Close() }()
	a, err := c.Ask(ctx, "unpair", nil, say)
	if err != nil {
		return out, err
	}
	if a.Code != 0 {
		return out, errors.New(nonEmptyOr(a.Error, "the runhost did not unpair this device"))
	}
	if err := json.Unmarshal(a.Result, &out); err != nil {
		return out, fmt.Errorf("the unpair's answer: %w", err)
	}
	return out, nil
}
