// Package assistant is the Assistant: the built-in persistent agent that is
// the person's own assistant, embedded in the CLI and run by the runner as a
// persistent agent (pkg/runner/persistent). Its manifest declares the
// persistent run mode alone, no egress, no filesystem, no exec and no secret;
// the runner keeps its conversation on a volume of its own between segments,
// tells it the time at each turn, fires the tasks it registers, and gives it
// its folders and what its roster has on.
package assistant

import (
	"embed"
	"io/fs"
)

//go:embed agent.yaml INSTRUCTION.md README.md LICENSE
var files embed.FS

// FS is the package's tree: agent.yaml, INSTRUCTION.md, README.md and
// LICENSE at its root.
func FS() fs.FS { return files }

// The package's identity, as its manifest declares it.
const (
	Namespace = "agentry"
	Name      = "assistant"
	Version   = "1.3.0"
)
