package deliver

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// markOpen sets the quarantine attribute on the copy through its open
// descriptor, so Gatekeeper treats it as a download; the attribute stays with
// the file when it is given its final name.
func markOpen(f *os.File) error {
	return unix.Fsetxattr(int(f.Fd()), "com.apple.quarantine", []byte(quarantineValue(time.Now())), 0)
}

// markPublished does nothing on macOS: markOpen did it.
func markPublished(string, string) error { return nil }
