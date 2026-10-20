// Package imagelabel names the label every image the runner builds carries:
// org.agentry.image=<kind>. A build's tag is content-keyed, so a new CLI,
// runner content, exec catalogue or pin builds a new tag and leaves the old
// image behind, and the classic builder leaves a multi-stage build's earlier
// stages behind untagged. Neither can be found by name; both can by this label,
// which is what the desktop's Docker images list and removal select by.
//
// The label is written into each Dockerfile, in every stage that runs an
// instruction, rather than passed as `docker build --label`: the flag
// labels the final image only, and an earlier stage the classic builder
// leaves dangling would carry nothing.
//
// The key is deliberately none of the run's own (agentry.run,
// agentry.gateway.run, agentry.owner.*): a container inherits its image's
// labels, so an image key the run sweep or the socket filter read would make
// every container of that image look like one run's. No run id is put on an
// image either: every build is shared by later runs.
package imagelabel

import (
	"fmt"
	"strings"
)

// Key is the label every image the runner builds carries.
const Key = "org.agentry.image"

// The kinds, the label's values: one per image the runner builds.
const (
	// Runner is the runner image (runnerimage/Dockerfile) and its build stage.
	Runner = "runner"
	// Exec is a per-QuickAgent exec image built on the runner image.
	Exec = "exec"
	// UnsafeExec is an Unsafe Mode exec image built on the unsafe base.
	UnsafeExec = "unsafe-exec"
	// CoderExec is the Coding agent's exec image built on its toolchain image.
	CoderExec = "coder-exec"
	// Decode is the decode step's image built on the files image.
	Decode = "decode"
	// HostLink is the host link's image built on the hostlink image.
	HostLink = "hostlink"
	// ClaudeCode is the Claude Code harness image and its unpack stage.
	ClaudeCode = "claude-code"
	// Copilot is the Copilot CLI harness image and its unpack stage.
	Copilot = "copilot"
)

// Filter is the `docker image ls --filter` argument that selects every
// image the runner built, of any kind.
const Filter = "label=" + Key

// Line is the Dockerfile instruction that labels a stage with kind,
// newline-terminated.
func Line(kind string) string { return "LABEL " + Key + "=" + kind + "\n" }

// Check reports the first stage of dockerfile that runs an instruction and
// does not carry Line(kind). A stage that is only a FROM (and ARGs) makes
// no image of its own and needs no label. It reads the Dockerfiles the
// runner generates line by line and is no Dockerfile parser: a heredoc, or
// the label written any other way (lower case, with another label on the
// line), reads as unlabelled, so it fails closed. The builders' tests run it.
func Check(dockerfile, kind string) error {
	want := strings.TrimSpace(Line(kind))
	stage, from, work, labelled := 0, "", false, false
	flush := func() error {
		if stage > 0 && work && !labelled {
			return fmt.Errorf("stage %d (%s) runs instructions but has no %q", stage, from, want)
		}
		return nil
	}
	for _, raw := range strings.Split(dockerfile, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		word := strings.ToUpper(strings.Fields(line)[0])
		switch {
		case word == "FROM":
			if err := flush(); err != nil {
				return err
			}
			stage, from, work, labelled = stage+1, line, false, false
		case stage == 0, word == "ARG":
		case line == want:
			labelled = true
		default:
			work = true
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if stage == 0 {
		return fmt.Errorf("no FROM in the Dockerfile")
	}
	return nil
}
