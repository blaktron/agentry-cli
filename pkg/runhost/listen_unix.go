//go:build unix

package runhost

import (
	"net"
	"syscall"
)

// listenOwnerOnly listens on a Unix socket created under a umask that gives
// no one else any bit, so there is no moment when another user could
// connect.
func listenOwnerOnly(path string) (net.Listener, error) {
	old := syscall.Umask(0o177)
	defer syscall.Umask(old)
	return net.Listen("unix", path)
}
