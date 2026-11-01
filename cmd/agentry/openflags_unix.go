//go:build !windows

package main

import "syscall"

// openNoFollow refuses to open a path whose last element is a symbolic link.
const openNoFollow = syscall.O_NOFOLLOW
