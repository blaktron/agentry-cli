package drafter

import (
	"fmt"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"
)

// subagentsHeading opens the notes' section on a persistent agent's
// subagents.
const subagentsHeading = "## Subagents the draft proposes"

// maxSubagentsNoted is the most subagents the section lists; the manifest
// bound is wider than the notes can carry.
const maxSubagentsNoted = 16

// noteSubagents adds to the notes each subagent a persistent draft declares,
// with its constraint and its reason, from the manifest rather than from what
// the model chose to say, so no subagent is in a draft without the person
// being told. The draft cannot say whether a subagent resolves: the registry
// resolves each one when the version is published, to the newest published
// version its constraint admits that runs on its own and is not persistent
// itself, so the section says so and what the person can do.
func (d *Draft) noteSubagents() {
	m, err := manifest.Parse(d.Manifest)
	if err != nil {
		return
	}
	subs := m.Subagents()
	if len(subs) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString(subagentsHeading + "\n\nThe agent may start these QuickAgents as runs of their own. " +
		"You agree to each one, with its reason, when you install the agent, and the agent starts only the ones listed here. " +
		"This draft does not check them: the registry resolves each one when the version is published, and holds the version for a person when one is not published, " +
		"does not run on its own, is itself persistent, or has no version its constraint admits. " +
		"To leave one out, remove its entry from `dependencies.agents`, and take its steps out of the instructions.\n\n")
	for i, s := range subs {
		if i == maxSubagentsNoted {
			fmt.Fprintf(&b, "- … and %d more\n", len(subs)-maxSubagentsNoted)
			break
		}
		fmt.Fprintf(&b, "- `%s` at `%s`: %s\n", s.Ref, s.Version, strings.Join(strings.Fields(s.Why), " "))
	}
	d.composeNotes(b.String())
}
