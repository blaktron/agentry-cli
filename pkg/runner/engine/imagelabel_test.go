package engine

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/imagelabel"
	"github.com/blaktron/agentry-cli/pkg/runner/owner"
)

// A container inherits its image's labels, so the image label must be none
// of the keys the teardown, the abandoned-run sweep and the relay sweep
// select by: a key they share would put every container of a built image
// in one run's sweep.
func TestTheSweepsNeverSelectByTheImageLabel(t *testing.T) {
	for _, label := range []string{runLabel, gatewayOwnerLabel, RelayLabel, owner.LabelHost, owner.LabelPID, owner.LabelStart} {
		key, _, _ := strings.Cut(label, "=")
		if key == imagelabel.Key {
			t.Errorf("the sweeps select by %q, the image label's key", label)
		}
	}
}

// TestTheFiltersImageLabelIsTheRunners: the docker filter's own test spells
// the image label as a literal, since the runner image's build context it
// lives in is stdlib-only; this holds it to imagelabel.Key.
func TestTheFiltersImageLabelIsTheRunners(t *testing.T) {
	if imagelabel.Key != "org.agentry.image" {
		t.Fatalf("imagelabel.Key is %q; runnerimage/sockfilter's imageLabelKey must follow it", imagelabel.Key)
	}
}
