//go:build !linux

package confine

import "errors"

// errNotLinux is why there is no confinement off Linux: Landlock is
// Linux's, and the tools server runs a QuickAgent's commands only inside
// the (Linux) runner image.
var errNotLinux = errors.New("landlock: Linux only")

// ABI reports that there is no Landlock off Linux.
func ABI() (int, error) { return 0, errNotLinux }

// Exec refuses off Linux rather than run the program unconfined.
func Exec(_ []string, _ string, _, _ []string) error { return errNotLinux }
