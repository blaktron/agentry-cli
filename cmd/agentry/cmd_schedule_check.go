// `agentry schedule add --check`: everything add decides before it keeps a
// schedule, with nothing kept, asked or changed (agentry-cli#777). Agentry
// Desktop's When step keeps Schedule disabled until it says ready (plan
// quickagent-scheduling.md D8), then makes the schedule with this binary,
// whose path the system's alarm pins.
package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
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
	// Refusals is every refusal a run would meet, as `run --plan` names
	// them; one the flags given do not lift is also a gap.
	Refusals []runPlanRefusal `json:"refusals"`
	// SignedOut is whether this computer can run any schedule while its
	// person is signed out, whatever --signed-out says.
	SignedOut signedOutAvailability `json:"signedOut"`
	Describe  string                `json:"describe,omitempty"`
	Next      *time.Time            `json:"next,omitempty"`
}

// scheduleCheck is add up to the point it would ask, install or keep
// anything. It fails only when it could not check: a release that cannot
// be resolved, or no run plan.
func scheduleCheck(cmd *cobra.Command, ref, name, out string, spec runSpecFlags, w whenFlags, now time.Time) error {
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
	preview, err := schedulePreview(cmd, sc, ref)
	if err != nil {
		return err
	}
	res.Modes = append(res.Modes, preview.Modes...)
	res.Refusals = append(res.Refusals, preview.Refusals...)
	res.NeedsConsent, res.ConsentReason = preview.NeedsConfirmation, preview.Reason
	if !slices.Contains(preview.Modes, string(manifest.ModeAutonomous)) {
		gap(fmt.Errorf("%s runs interactive only: a schedule runs with nobody there to answer, so it can only run an autonomous release", preview.Ref))
	}
	for _, r := range preview.Refusals {
		if r.Lift == "" || !liftGiven(r.Lift, sc.Run) {
			gap(errors.New("a run would refuse: " + r.Message))
		}
	}
	if sc, err = pinPreview(sc, preview); err != nil {
		return err
	}
	res.Ref, res.Version, res.Digest = sc.Ref, sc.Version, sc.Digest

	if installedHere(sc) {
		res.Gaps = append(res.Gaps, fireGaps(sc)...)
		if sc.SignedOut {
			res.Gaps = append(res.Gaps, signedOutGaps(cmd.Context(), sc)...)
		}
	} else {
		res.Gaps = append(res.Gaps, fmt.Sprintf("%s@%s is not installed on this computer: `agentry install %s@%s` installs it (so does making the schedule), and its inputs are checked once it is", sc.Ref, sc.Version, sc.Ref, sc.Version))
		if sc.SignedOut {
			res.Gaps = append(res.Gaps, signedOutMachineGaps(cmd.Context())...)
		}
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
