//go:build unix

package beneath

import "syscall"

const (
	// nonBlock opens a pipe without waiting for its other end.
	nonBlock = syscall.O_NONBLOCK
	// noFollow refuses a link as the last element of a path.
	noFollow = syscall.O_NOFOLLOW
)
