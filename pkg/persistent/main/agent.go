// Package mainagent is Main: the built-in persistent agent that helps the person
// with Agentry itself, embedded in the CLI and run by Agentry Desktop as a
// persistent agent (pkg/runner/persistent). Its manifest declares the
// persistent run mode alone, no egress, no filesystem, no exec and no
// secret; the application gives it its app link, through which it reaches
// Agentry and nothing else.
package mainagent

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
	Name      = "main"
	Version   = "1.0.0"
)
