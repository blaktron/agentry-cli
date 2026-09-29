// Package drafter is the Drafter (plan builder-rework.md §7.2, D6;
// agentry-cli#345): the QuickAgent package that turns a person's
// description into a draft for the Builder, embedded in the CLI beside the
// Knowledge Creator and run by the runner as the second built-in package.
// Its manifest declares no egress, no filesystem, no exec and no secret; its
// reach is the three web tools the runner grants to it alone
// (runnerimage/toolsrv/web.go), every result of which AI Review reads before
// its model does.
//
// Around the run this package holds the task the Drafter is given, the
// description's scan before the run starts (Prescan), the person's own keys
// for a draft on this computer (LocalWeb), the draft it returns (Draft,
// draft.json's shape, plan §7.5) with AI Review's record (Finish), and the
// package a draft renders into, which the CLI's post-step validates. The
// CLI's `agentry draft` and the desktop's Describe it both drive it.
package drafter

import (
	"embed"
	"io/fs"
)

//go:embed agent.yaml AGENT.md output.schema.json README.md
var files embed.FS

// FS is the package's tree: agent.yaml, AGENT.md, output.schema.json and
// README.md at its root.
func FS() fs.FS { return files }

// The package's identity, as its manifest declares it.
const (
	Namespace = "agentry"
	Name      = "drafter"
	Version   = "1.1.0"
)

// The Drafter's own caps (plan builder-simplify.md §5.1, D8): tighter than
// the manifest's bounds, which stay as they are for a QuickAgent written by
// hand. AGENT.md states each by value (TestTheInstructionStatesItsBoundsByValue)
// and the fixtures' shape check holds a draft to them (CheckShape).
const (
	MaxIntake        = 3
	MaxPrepare       = 5
	MaxOpenQuestions = 2
	MaxNoteBullets   = 5
)
