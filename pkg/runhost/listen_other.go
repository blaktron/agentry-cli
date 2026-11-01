//go:build !unix

package runhost

import (
	"errors"
	"net"
)

// listenOwnerOnly is unix-only: a runhost's service runs on a Linux server.
func listenOwnerOnly(string) (net.Listener, error) {
	return nil, errors.New("a runhost's service runs only on Linux")
}
