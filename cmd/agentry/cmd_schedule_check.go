// `agentry schedule add --check`: everything add decides before it keeps a
// schedule, with nothing kept, asked or changed. Agentry Desktop's When
// step keeps Schedule disabled until it says ready, then makes the
// schedule with this binary, whose path the system's alarm pins.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/remote"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// scheduleCheckJSON is what `agentry schedule add --check --json` prints.
type scheduleCheckJSON struct {
	Ref     string   `json:"ref,omitempty"`
	Version string   `json:"version,omitempty"`
	Digest  string   `json:"digest,omitempty"`
	Modes   []string `json:"modes"`
	// Ready: add, given the same flags, would keep the schedule; Gaps is
	// every reason it would not.
	Ready bool     `json:"ready"`
	Gaps  []string `json:"gaps"`
	// NeedsConsent: add would ask for consent to the release's
	// capabilities (or take --yes), for ConsentReason.
	NeedsConsent  bool   `json:"needsConsent"`
	ConsentReason string `json:"consentReason,omitempty"`
	// Refusals is every refusal a run with the flags given would meet, as
	// `run --plan` names them; each is also a gap.
	Refusals []runPlanRefusal `json:"refusals"`
	// Installed: the pinned version is laid down on this computer, so the
	// inputs its fires read were checked. A version that is not is
	// installed by add (the consent step), which checks them then: they
	// are no gap here, and add may still refuse with them.
	Installed bool `json:"installed"`
	// SignedOut is whether this computer can run any schedule while its
	// person is signed out, whatever --signed-out says.
	SignedOut signedOutAvailability `json:"signedOut"`
	Describe  string                `json:"describe,omitempty"`
	Next      *time.Time            `json:"next,omitempty"`
	// Runhost is the remote runhost a schedule with --on would run on, and
	// what its store would keep for it.
	Runhost *scheduleCheckRunhost `json:"runhost,omitempty"`
}

// scheduleCheckRunhost is a runhost schedule's part of the check: the
// runhost, whether it takes schedules, and the entries its store would keep
// (by name, never a value), which the consent names.
type scheduleCheckRunhost struct {
	Name           string                  `json:"name"`
	Host           string                  `json:"host"`
	Fingerprint    string                  `json:"hostKeyFingerprint"`
	TakesSchedules bool                    `json:"takesSchedules"`
	Entries        []runhost.ScheduleEntry `json:"entries"`
}

// scheduleCheck is add up to the point it would ask, install or keep
// anything. It fails only when it could not check: a release that cannot
// be resolved, or no run plan.
func scheduleCheck(cmd *cobra.Command, ref, name, out, on string, spec runSpecFlags, w whenFlags, now time.Time) error {
	res := scheduleCheckJSON{Modes: []string{}, Gaps: []string{}, Refusals: []runPlanRefusal{}, SignedOut: signedOutHere(cmd.Context())}
	gap := func(err error) {
		for _, line := range strings.Split(err.Error(), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				res.Gaps = append(res.Gaps, line)
			}
		}
	}
	when, err := w.recurrence(now)
	if err != nil {
		gap(err)
	} else {
		res.Describe = when.Describe()
		if n, ok := when.Next(now); ok {
			res.Next = &n
		}
	}
	sc, err := scheduleFromFlags(name, out, spec, when, now)
	if err != nil {
		gap(err)
	}
	if on == "" {
		if err := assignScheduleModel(&sc, spec.model); err != nil {
			// A saved model this computer does not have is the person's to
			// fix: it is a gap, and the rest is checked as the default would
			// be, so the check still names every other gap.
			gap(err)
			sc.Run.Model = ""
		}
	}
	if reason := scheduleHold(sc); reason != "" {
		gap(heldError(reason))
	}
	preview, err := schedulePreview(cmd, sc, ref)
	if err != nil {
		return err
	}
	res.Modes = append(res.Modes, preview.Modes...)
	res.NeedsConsent, res.ConsentReason = preview.NeedsConfirmation, preview.Reason
	if !slices.Contains(preview.Modes, string(manifest.ModeAutonomous)) {
		gap(fmt.Errorf("%s runs interactive only: a schedule runs with nobody there to answer, so it can only run an autonomous release", preview.Ref))
	}
	// The preview lists every refusal; a run meets those the flags given
	// do not lift.
	for _, r := range preview.Refusals {
		if r.Lift == "" || !liftGiven(r.Lift, sc.Run) {
			res.Refusals = append(res.Refusals, r)
			gap(errors.New("a run would refuse: " + r.Message))
		}
	}
	if sc, err = pinPreview(sc, preview); err != nil {
		return err
	}
	res.Ref, res.Version, res.Digest = sc.Ref, sc.Version, sc.Digest

	if on != "" {
		res.Runhost, err = checkRunhostSchedule(cmd, on, sc, preview, gap)
		if err != nil {
			return err
		}
		res.Ready = len(res.Gaps) == 0
		if flags.jsonOut {
			return emitJSON(cmd.OutOrStdout(), res)
		}
		o := cmd.OutOrStdout()
		if res.Ready {
			_, _ = fmt.Fprintf(o, "ready: %s@%s can be scheduled on runhost %s (%s); its store would keep %s\n", res.Ref, res.Version, on, res.Describe, scheduleEntriesSaid(res.Runhost.Entries))
		} else {
			_, _ = fmt.Fprintf(o, "not ready: a schedule of %s@%s on runhost %s would not have what it needs:\n  - %s\n", res.Ref, res.Version, on, strings.Join(res.Gaps, "\n  - "))
		}
		return nil
	}
	res.Installed = installedHere(sc)
	if res.Installed {
		res.Gaps = append(res.Gaps, fireGaps(sc)...)
		if sc.SignedOut {
			res.Gaps = append(res.Gaps, signedOutGaps(cmd.Context(), sc)...)
		}
	} else if sc.SignedOut {
		res.Gaps = append(res.Gaps, signedOutMachineGaps(cmd.Context())...)
	}
	if len(res.Gaps) == 0 {
		// Only once the shell's view is clean: the scheduler's own. A
		// signed-out schedule's keys are not sealed by a check, so its
		// probe reads them from the keychain, as add's seal will.
		probed := sc
		probed.SignedOut = false
		res.Gaps = append(res.Gaps, probeGaps(probed, "")...)
	}
	res.Ready = len(res.Gaps) == 0

	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), res)
	}
	o := cmd.OutOrStdout()
	if res.Ready {
		_, _ = fmt.Fprintf(o, "ready: %s@%s can be scheduled (%s)\n", res.Ref, res.Version, res.Describe)
	} else {
		_, _ = fmt.Fprintf(o, "not ready: a scheduled run of %s@%s would not have what it needs:\n  - %s\n", res.Ref, res.Version, strings.Join(res.Gaps, "\n  - "))
	}
	if !res.Installed {
		_, _ = fmt.Fprintf(o, "%s@%s is not installed yet: making the schedule installs it and checks its inputs then\n", res.Ref, res.Version)
	}
	if res.NeedsConsent {
		_, _ = fmt.Fprintf(o, "making it asks for consent to its capabilities: %s\n", res.ConsentReason)
	}
	return nil
}

// installedHere reports whether the pinned release is laid down in the
// runner home (a local directory always is), so its inputs can be read.
func installedHere(sc schedule.Schedule) bool {
	if sc.Local != "" {
		return true
	}
	home, err := runnerHome()
	if err != nil {
		return false
	}
	ns, name, ok := strings.Cut(sc.Ref, "/")
	if !ok {
		return false
	}
	_, err = os.Stat(installdir.ReleaseDir(home, ns, name, sc.Version, sc.Digest))
	return err == nil
}

// checkRunhostSchedule is --check with --on: whether runhost on takes
// schedules, and what a schedule of sc would keep in its store, read from
// the release's contract, this computer's policy and the flags, with no
// value read and nothing sent.
func checkRunhostSchedule(cmd *cobra.Command, on string, sc schedule.Schedule, preview *runPlanOutput, gap func(error)) (*scheduleCheckRunhost, error) {
	switch {
	case sc.Local != "":
		gap(errors.New("a runhost runs a published release, not a local folder"))
	case sc.SignedOut:
		gap(errors.New("--signed-out is for this computer: a runhost runs its schedules with nobody signed in"))
	case sc.Run.DeliverTo != "":
		gap(errors.New("--deliver-to copies to this computer, and a runhost's schedule runs while it may be away"))
	}
	rec, _, c, err := dialRunhostNamed(cmd.Context(), on)
	if rec == nil {
		return nil, err
	}
	out := &scheduleCheckRunhost{Name: rec.Name, Host: rec.Host, Fingerprint: rec.HostKeyFingerprint, Entries: []runhost.ScheduleEntry{}}
	if err != nil {
		gap(fmt.Errorf("runhost %s cannot be reached over its run key: %w", on, err))
	} else {
		defer func() { _ = c.Close() }()
		if _, verr := remote.SameVersion(cmd.Context(), c, rec.Name); verr != nil {
			gap(verr)
		}
		if out.TakesSchedules, err = remote.TakesSchedules(cmd.Context(), c); err != nil {
			gap(err)
		} else if !out.TakesSchedules {
			gap(fmt.Errorf("runhost %s takes no schedule: its systemd cannot seal its store's key to it (schedules need systemd %d or newer)", on, schedule.MinSystemd))
		}
	}
	add := func(kind, name string) {
		out.Entries = append(out.Entries, runhost.ScheduleEntry{Kind: kind, Name: name})
	}
	if pf, err := policy.Resolve(sc.Run.Policy); err == nil {
		if pol, _, err := policy.EnsureDefault(pf.Path); err == nil {
			if m := job.ModelOf(pol.Model); !m.SignIn() && !m.PaysWithCredits() && pol.Model.Provider != "" {
				add("model-key", pol.Model.KeyProvider())
			}
		}
	}
	var caps manifest.Capabilities
	if len(preview.Contract) > 0 {
		if err := json.Unmarshal(preview.Contract, &caps); err != nil {
			return nil, fmt.Errorf("read the release's contract: %w", err)
		}
	}
	secrets := slices.Clone(caps.Secrets)
	slices.Sort(secrets)
	for _, s := range secrets {
		add("secret", s)
	}
	for _, d := range sc.Run.Destinations {
		add("destination", d)
	}
	for _, h := range sc.Run.Hosts {
		add("target-host", h.ID)
	}
	m := &manifest.Manifest{Capabilities: caps}
	for _, s := range oauthServers(m) {
		add("connection", connectionServer(s))
	}
	return out, nil
}
