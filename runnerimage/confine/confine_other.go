//go:build !linux

package confine

import "errors"

// errNotLinux is why there is no confinement off Linux: Landlock is
// Linux's, and the tools server runs a QuickAgent's commands only inside
// the (Linux) runner image.
var errNotLinux = errors.New("landlock: Linux only")

// ABI reports that there is no Landlock off Linux.
func ABI() (int, error) { return 0, errNotLinux }

// ExecWith refuses off Linux rather than run the program unconfined.
func ExecWith(_ Options, _ string, _, _ []string) error { return errNotLinux }

// NetworkDenied is false off Linux.
func NetworkDenied(int) bool { return false }
