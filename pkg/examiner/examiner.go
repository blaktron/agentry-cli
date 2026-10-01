// Package examiner is the Examiner (plan desktop-review-key-and-run-cards.md
// §6, D2, D18, D20, D24, D25; agentry-cli#504): the built-in QuickAgent that
// reviews a QuickAgent a person has built and reports what the registry's
// pipeline would be expected to say about it at Submit, embedded in the CLI
// and run by the runner as the fourth built-in package, the one granted the
// review grant (C1, agentry-cli#503). Its manifest declares no egress, no
// filesystem, no exec, no secret and no server; its reach is ai_review and
// scanned reads of its inputs, with the person's own TypeSafe key.
//
// Around the run this package holds the host's pre-step (Prepare: the
// registry's own rules, its baseline policy and its record of each declared
// server, and the inputs staged read-only), the post-step (Finish: the
// report, its three kinds of finding, the expected outcome, what did not run
// here) and the gate's rule restated (Gate), which a test holds to the
// core's published table of gate cases. The CLI's `agentry review` is its
// first caller; the desktop's Review Your QuickAgents locally is the next.
package examiner

import (
	"embed"
	"io/fs"
)

//go:embed agent.yaml INSTRUCTION.md output.schema.json README.md LICENSE
var files embed.FS

// FS is the package's tree: agent.yaml, INSTRUCTION.md, output.schema.json,
// README.md and LICENSE at its root (a public release carries the last two,
// V7).
func FS() fs.FS { return files }

// The package's identity, as its manifest declares it.
const (
	Namespace = "agentry"
	Name      = "examiner"
	Version   = "1.0.0"
)

// LocalTimeoutFloorSeconds is the least a review on this computer is given:
// 30 minutes, past the runner policy's 15-minute run timeout, since one
// rubric review alone may take 15 (toolsrv's bound on an ai_review call). It
// is a timeout alone, as the other built-ins' floors are.
const LocalTimeoutFloorSeconds = 1800
