package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"
)

// The persistent mode is never picked beside another mode, and a release
// that declares only it resolves to it, for the compiler to refuse.
func TestResolveRunModeNeverPicksPersistentBesideAnother(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader("")) // not a terminal: nothing is asked
	for _, tc := range []struct {
		modes []manifest.Mode
		opts  runOpts
		want  manifest.Mode
	}{
		{[]manifest.Mode{manifest.ModePersistent}, runOpts{}, manifest.ModePersistent},
		{[]manifest.Mode{manifest.ModeAutonomous, manifest.ModePersistent}, runOpts{}, manifest.ModeAutonomous},
		{[]manifest.Mode{manifest.ModePersistent, manifest.ModeInteractive}, runOpts{}, manifest.ModeInteractive},
		{[]manifest.Mode{manifest.ModePersistent, manifest.ModeInteractive, manifest.ModeAutonomous}, runOpts{}, manifest.ModeAutonomous},
		{[]manifest.Mode{manifest.ModePersistent, manifest.ModeInteractive, manifest.ModeAutonomous}, runOpts{yes: true}, manifest.ModeAutonomous},
		{[]manifest.Mode{manifest.ModeAutonomous, manifest.ModePersistent}, runOpts{mode: "persistent"}, manifest.ModePersistent},
	} {
		m := &manifest.Manifest{SchemaVersion: "2", Modes: tc.modes}
		got, err := resolveRunMode(cmd, tc.opts, m)
		if err != nil || got != tc.want {
			t.Errorf("modes %v, opts %+v: %q, %v; want %q", tc.modes, tc.opts, got, err, tc.want)
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
