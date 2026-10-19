// Package writer is the Voice Writer: the QuickAgent package that writes a
// person's Writing Voice from samples of their writing, embedded in the CLI
// and run by the runner as a built-in package, the third beside the Knowledge
// Creator and the Drafter. Its contract declares no egress, no filesystem, no
// exec and the files data category: the runner decodes the samples before the
// run (pkg/runner/decode), the agent reads their text and the current voice
// through the inputs channel, and writes VOICE.md through write_report;
// pkg/voice/build keeps it. A build may give it web addresses of the
// person's writing: a page whose text was read before the run arrives as a
// sample, and one drawn by scripts is read with web_read, which the
// application grants it held to those addresses alone (pkg/runner/localrun).
package writer

import (
	"embed"
	"io/fs"
)

//go:embed agent.yaml INSTRUCTION.md README.md LICENSE knowledge
var files embed.FS

// FS is the package's tree: agent.yaml, INSTRUCTION.md, README.md and
// LICENSE at its root (a public release carries the last two), and under
// knowledge/ the pack it ships (GuideName).
func FS() fs.FS { return files }

// GuideName is the knowledge pack the Voice Writer ships in its own
// release, its guide to writing a voice: the dimensions of a voice, reading
// a web page, a worked example, and what AI Review refuses in a voice. The
// runner gives it to every run of the package (localrun's
// withShippedPacks). Its manifest.json, index and review page are
// generated (TestTheStyleGuideIsCurrent).
const GuideName = "style-guide"

// The package's identity, as its manifest declares it.
const (
	Namespace = "agentry"
	Name      = "voice-writer"
	Version   = "1.3.0"
)

// LocalTimeoutFloorSeconds is the least a Voice Writer run on this
// computer is given: 30 minutes, past the runner policy's 15-minute run
// timeout, since reading a person's samples outruns an ordinary task. It
// is a timeout alone, as the Knowledge Creator's floor is.
const LocalTimeoutFloorSeconds = 1800
