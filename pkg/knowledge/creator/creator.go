// Package creator is the Knowledge Creator (plan knowledge.md §4.2, D6;
// agentry-cli#318): the QuickAgent package that compiles documents into a
// knowledge pack, embedded in the CLI and run by the runner as a built-in
// package — part of the application, never a release a person installs.
// Its contract declares no egress, no filesystem, no exec and the files
// data category: the runner decodes the documents before the run
// (pkg/runner/decode), the agent reads their text through the inputs
// channel and the pack as it stands through the knowledge tools, and
// writes pages through write_report; pkg/knowledge applies them.
package creator

import (
	"embed"
	"io/fs"
)

//go:embed agent.yaml INSTRUCTION.md README.md LICENSE
var files embed.FS

// FS is the package's tree: agent.yaml, INSTRUCTION.md, README.md and
// LICENSE at its root (a public release carries the last two, V7).
func FS() fs.FS { return files }

// The package's identity, as its manifest declares it.
const (
	Namespace = "agentry"
	Name      = "knowledge-creator"
	Version   = "1.1.1"
)

// LocalTimeoutFloorSeconds is the least a knowledge build on this computer
// is given (plan scan-and-run-tweaks.md §6.2, D13; agentry-cli#500, DR-54):
// 60 minutes, what a build in Agentry gets on the tiers that can build,
// where the runner policy's run timeout defaults to 15. A policy that
// allows more keeps its own, and a timeout named for the build wins either
// way (localrun.Builtin.LocalBudgets). It is a timeout alone: a build on
// this computer has no token limit, and its tool-call cap is the policy's.
const LocalTimeoutFloorSeconds = 3600
