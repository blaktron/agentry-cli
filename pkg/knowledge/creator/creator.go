// Package creator is the Knowledge Creator: the QuickAgent package that
// compiles documents into a knowledge pack, embedded in the CLI and run by
// the runner as a built-in package — part of the application, never a release
// a person installs. Its contract declares no egress, no filesystem, no exec
// and the files data category: the runner decodes the documents before the
// run (pkg/runner/decode), the agent reads their text through the inputs
// channel and the pack as it stands through the knowledge tools, and writes
// pages through write_report; pkg/knowledge applies them. From 1.2.0 a build
// may be given a brief instead of documents, and the application may grant it
// web_search and web_read, which its manifest does not declare (they ride on
// the built-in, pkg/runner/localrun): it researches what the documents and
// the pack do not hold, every result read by AI Review first, and cites web
// pages as [src: web:<url>] and its model's own knowledge as [src: model].
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
	Version   = "1.3.0"
)

// LocalTimeoutFloorSeconds is the least a knowledge build on this computer
// is given: 60 minutes, what a build in Agentry gets on the tiers that can
// build, where the runner policy's run timeout defaults to 15. A policy that
// allows more keeps its own, and a timeout named for the build wins either
// way (localrun.Builtin.LocalBudgets). It is a timeout alone: a build on this
// computer has no token limit, and its tool-call cap is the policy's.
const LocalTimeoutFloorSeconds = 3600
