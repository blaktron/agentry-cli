// Package coder is the Coding agent: the built-in persistent agent for
// software work, embedded in the CLI. Its manifest declares the persistent
// run mode alone, no egress, no filesystem, no exec and no secret; what the
// application gives it (its programs, open egress, a browser, a dev server)
// is listed in its README.
package coder

import (
	"embed"
	"io/fs"
)

//go:embed agent.yaml INSTRUCTION.md README.md LICENSE skills
var files embed.FS

// FS is the package's tree: agent.yaml, INSTRUCTION.md, README.md and
// LICENSE at its root, and its skills, one folder each beneath skills/.
func FS() fs.FS { return files }

// The package's identity, as its manifest declares it.
const (
	Namespace = "agentry"
	Name      = "coder"
	Version   = "1.3.1"
)
