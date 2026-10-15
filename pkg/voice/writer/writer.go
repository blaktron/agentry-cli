// Package writer is the Voice Writer: the QuickAgent package that writes a
// person's Writing Voice from samples of their writing, embedded in the CLI
// and run by the runner as a built-in package, the third beside the Knowledge
// Creator and the Drafter. Its contract declares no egress, no filesystem, no
// exec and the files data category: the runner decodes the samples before the
// run (pkg/runner/decode), the agent reads their text and the current voice
// through the inputs channel, and writes VOICE.md through write_report;
// pkg/voice/build keeps it.
package writer

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
	Name      = "voice-writer"
	Version   = "1.1.0"
)

// LocalTimeoutFloorSeconds is the least a Voice Writer run on this
// computer is given: 30 minutes, past the runner policy's 15-minute run
// timeout, since reading a person's samples outruns an ordinary task. It
// is a timeout alone, as the Knowledge Creator's floor is.
const LocalTimeoutFloorSeconds = 1800
