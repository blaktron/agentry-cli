//go:build !linux && !windows

package engine

// procStartTime has no portable answer off Linux, so ok is always false and
// the caller escalates on the process-group id alone: it cannot tell a pid the
// kernel reused from the process it started. macOS is the platform this
// affects, and the window it leaves open is the grace period — the same one
// every job-control implementation that signals a group id lives with.
func procStartTime(_ int) (uint64, bool) { return 0, false }
