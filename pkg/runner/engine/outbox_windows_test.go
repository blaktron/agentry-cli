//go:build windows

package engine

import "testing"

// makePipe makes nothing on Windows, which has no named pipe in a folder.
func makePipe(*testing.T, string) bool { return false }
