//go:build windows

package engine

import (
	"io/fs"
	"os"
)

// outboxOpenFlags opens a file of the share outbox for reading.
const outboxOpenFlags = os.O_RDONLY

// outboxLinks is one on Windows, where a container's saved file reaches the
// host through the engine's file sharing, which makes no hard links.
func outboxLinks(fs.FileInfo) uint64 { return 1 }

// outboxNeedsHandOver is false on Windows, whose files have no POSIX owner:
// the engine's file sharing gives the person what a container saves.
func outboxNeedsHandOver(string) bool { return false }
