// Package coder is the Coding agent: the built-in persistent agent for
// software work, embedded in the CLI. Its manifest declares the persistent
// run mode alone, no egress, no filesystem, no exec and no secret; what the
// application gives it is listed in its README.
package coder

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
	Name      = "coder"
	Version   = "1.0.0"
)
