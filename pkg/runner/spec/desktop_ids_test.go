package spec

import (
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/runnerimage/desktoplink"
)

// The plan's computer-use id and scopes are the runner image's and the
// manifest's.
func TestTheComputerUseNamesAgree(t *testing.T) {
	if DesktopComputerID != desktoplink.ComputerID {
		t.Errorf("the plan names computer use %q, the runner image %q", DesktopComputerID, desktoplink.ComputerID)
	}
	if ComputerUseApp != string(manifest.ComputerUseApp) || ComputerUseScreen != string(manifest.ComputerUseScreen) {
		t.Error("the plan's scopes are not the manifest's")
	}
}
