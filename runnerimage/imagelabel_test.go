package runnerimage

import (
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/imagelabel"
)

// Every stage of the runner image that runs an instruction carries the
// runner kind, so the build stage the classic builder leaves untagged is
// found by label as well as the image.
func TestDockerfileLabelsEveryStage(t *testing.T) {
	b, err := contextFS.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if err := imagelabel.Check(string(b), imagelabel.Runner); err != nil {
		t.Error(err)
	}
}
