package harnessbuild

import (
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/imagelabel"
)

// Both harness builds label the unpack stage as well as the image, so the
// stage the classic builder leaves untagged is found by label.
func TestHarnessBuildsLabelEveryStage(t *testing.T) {
	if err := imagelabel.Check(claudeDockerfile, imagelabel.ClaudeCode); err != nil {
		t.Errorf("claude code: %v", err)
	}
	if err := imagelabel.Check(copilotDockerfile, imagelabel.Copilot); err != nil {
		t.Errorf("copilot: %v", err)
	}
}
