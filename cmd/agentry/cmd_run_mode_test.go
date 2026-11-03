package main

import (
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"
)

// The persistent mode is never picked beside another mode, and a release
// that declares only it resolves to it, for the compiler to refuse. The mode
// asked of a person on a terminal is the autonomous one unless they say
// another, and a flag, --yes, --dry-run or --plan never asks.
func TestResolveRunModeNeverPicksPersistentBesideAnother(t *testing.T) {
	prevTTY := stdinIsTTY
	t.Cleanup(func() { stdinIsTTY = prevTTY })
	both := []manifest.Mode{manifest.ModeAutonomous, manifest.ModeInteractive}
	for _, tc := range []struct {
		modes []manifest.Mode
		opts  runOpts
		// tty makes stdin a terminal, typing what.
		tty     bool
		typed   string
		want    manifest.Mode
		wantErr string
	}{
		{modes: []manifest.Mode{manifest.ModePersistent}, want: manifest.ModePersistent},
		{modes: []manifest.Mode{manifest.ModeAutonomous, manifest.ModePersistent}, want: manifest.ModeAutonomous},
		{modes: []manifest.Mode{manifest.ModePersistent, manifest.ModeInteractive}, want: manifest.ModeInteractive},
		{modes: []manifest.Mode{manifest.ModePersistent, manifest.ModeInteractive, manifest.ModeAutonomous}, want: manifest.ModeAutonomous},
		{modes: []manifest.Mode{manifest.ModePersistent, manifest.ModeInteractive, manifest.ModeAutonomous}, opts: runOpts{yes: true}, want: manifest.ModeAutonomous},
		{modes: []manifest.Mode{manifest.ModeAutonomous, manifest.ModePersistent}, opts: runOpts{mode: "persistent"}, want: manifest.ModePersistent},
		// The flag names the mode, and an unknown one is refused.
		{modes: both, opts: runOpts{mode: "interactive"}, want: manifest.ModeInteractive},
		{modes: both, opts: runOpts{mode: "chatty"}, wantErr: `unknown --mode "chatty" (want autonomous or interactive)`},
		// A release that declares no mode is left for the compiler to refuse.
		{modes: nil, want: manifest.ModeAutonomous},
		// A client of the Agent Client Protocol is a chat surface when the release has a chat.
		{modes: both, opts: runOpts{acp: &acpRun{}}, want: manifest.ModeInteractive},
		{modes: []manifest.Mode{manifest.ModeAutonomous, manifest.ModePersistent}, opts: runOpts{acp: &acpRun{}}, want: manifest.ModeAutonomous},
		// With a terminal the person is asked, and autonomous is the default answer.
		{modes: both, tty: true, typed: "interactive\n", want: manifest.ModeInteractive},
		{modes: both, tty: true, typed: "\n", want: manifest.ModeAutonomous},
		{modes: both, tty: true, typed: "bogus\n", wantErr: `unknown mode "bogus" (want autonomous or interactive)`},
		{modes: both, tty: true, typed: "", wantErr: "read answer"},
		// None of these asks, though the terminal would answer interactive.
		{modes: both, opts: runOpts{yes: true}, tty: true, typed: "interactive\n", want: manifest.ModeAutonomous},
		{modes: both, opts: runOpts{dryRun: true}, tty: true, typed: "interactive\n", want: manifest.ModeAutonomous},
		{modes: both, opts: runOpts{plan: true}, tty: true, typed: "interactive\n", want: manifest.ModeAutonomous},
		{modes: both, opts: runOpts{mode: "autonomous"}, tty: true, typed: "interactive\n", want: manifest.ModeAutonomous},
		{modes: both, opts: runOpts{acp: &acpRun{}}, tty: true, typed: "autonomous\n", want: manifest.ModeInteractive},
	} {
		stdinIsTTY = func(io.Reader) bool { return tc.tty }
		cmd := &cobra.Command{}
		cmd.SetIn(strings.NewReader(tc.typed)) // not a terminal unless tty: nothing is asked
		cmd.SetErr(io.Discard)
		m := &manifest.Manifest{SchemaVersion: "2", Modes: tc.modes}
		got, err := resolveRunMode(cmd, tc.opts, m)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("modes %v, opts %+v, typed %q: %q, %v; want the error %q", tc.modes, tc.opts, tc.typed, got, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("modes %v, opts %+v, typed %q: %q, %v; want %q", tc.modes, tc.opts, tc.typed, got, err, tc.want)
		}
		if strings.Join(modeNames(m.Modes), ",") != strings.Join(modeNames(tc.modes), ",") {
			t.Errorf("resolveRunMode changed the manifest's modes to %v", m.Modes)
		}
	}
}

func modeNames(modes []manifest.Mode) []string {
	out := make([]string, len(modes))
	for i, m := range modes {
		out[i] = string(m)
	}
	return out
}
