package deliver

import (
	"os"
	"path/filepath"
)

// markOpen does nothing on Windows: the flag is a stream of the published
// file's name.
func markOpen(*os.File) error { return nil }

// markPublished writes the Mark-of-the-Web as an alternate data stream of the
// copy dir/name. A file system without streams (FAT, a network share) refuses
// it, and the delivery says so.
func markPublished(dir, name string) error {
	return os.WriteFile(filepath.Join(dir, name)+":Zone.Identifier", []byte(zoneIdentifier), 0o644) //nolint:gosec // a stream of the file just published
}
