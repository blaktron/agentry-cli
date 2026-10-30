// Package drafter is the Drafter: the QuickAgent package that turns a
// person's description into a draft for the Builder, embedded in the CLI
// beside the Knowledge Creator and run by the runner as the second built-in
// package. Its manifest declares no egress, no filesystem, no exec and no
// secret; its reach is the three web tools the runner grants to it alone
// (runnerimage/toolsrv/web.go), every result of which AI Review reads before
// its model does.
//
// Around the run this package holds the task the Drafter is given, the
// description's scan before the run starts (Prescan), the person's own keys
// for a draft on this computer (LocalWeb), the draft it returns (Draft,
// draft.json's shape) with AI Review's record (Finish), and the package a
// draft renders into, which the CLI's post-step validates. The CLI's `agentry
// draft` and the desktop's Describe it both drive it.
package drafter

import (
	"embed"
	"io/fs"
)

//go:embed agent.yaml AGENT.md output.schema.json README.md LICENSE knowledge
var files embed.FS

// FS is the package's tree: agent.yaml, AGENT.md, output.schema.json,
// README.md and LICENSE at its root (a public release carries the last two,
// V7), and under knowledge/ the pack it ships (GuideName).
func FS() fs.FS { return files }

// GuideName is the knowledge pack the Drafter ships in its own release, its
// drafting guide: how a run works, what a declared MCP server meets in the
// runner, and the guide to the rest of a draft. The runner gives it to
// every run of the package, the repair turn included (localrun's
// withShippedPacks), apart from the person's packs, so it counts against
// none of a draft's caps; a person's pack of this name is refused
// (CheckKnowledgeSize). Its manifest.json, index and runtime facts are
// generated (TestTheDraftingGuideIsCurrent).
const GuideName = "drafting-guide"

// The package's identity, as its manifest declares it.
const (
	Namespace = "agentry"
	Name      = "drafter"
	Version   = "1.18.5"
)

// LocalTimeoutFloorSeconds is the least a draft made on this computer is
// given: 30 minutes, where the runner policy's run timeout defaults to 15.
// A policy that allows more keeps its own, and a timeout the person names
// for the draft wins either way (localrun.Builtin.LocalBudgets). It is a
// timeout alone: a draft on this computer has no token limit, and its
// tool-call cap is the policy's. A draft made in Agentry runs under the
// caps its job carries.
const LocalTimeoutFloorSeconds = 1800

// The Drafter's own caps: tighter than the manifest's bounds, which stay as
// they are for a QuickAgent written by hand. AGENT.md states each by value
// (TestTheInstructionStatesItsBoundsByValue) and the fixtures' shape check
// holds a draft to them (CheckShape).
const (
	MaxIntake        = 3
	MaxPrepare       = 5
	MaxOpenQuestions = 2
	MaxNoteBullets   = 5
)

// MaxInstructionBytes is the most the instruction file the Drafter writes
// may hold: most QuickAgents run with no network, so a drafted instruction
// carries the domain knowledge the run cannot look up, 20–30 KB for a big
// task. It is held equal to the core's draft-check bound
// (judge.DraftMaxInstruction), so no instruction it returns is past what
// the check reads. The check also holds the description, the manifest and
// the instruction to 48 KiB together (judge.DraftMaxState) and reads a
// draft past that as not scanned, which the post-step records and never
// counts as a pass. A longer instruction goes to the repair turn to be
// shortened.
const MaxInstructionBytes = 32 << 10
