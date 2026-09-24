//go:build !linux

package owner

// StartTime has no portable answer off Linux, so ok is always false: the
// engine's group kill then escalates on the process-group id alone, unable to
// tell a pid the kernel reused from the process it started. macOS is the
// platform this affects, and the window it leaves open is the grace period —
// the same one every job-control implementation that signals a group id lives
// with.
func StartTime(_ int) (uint64, bool) { return 0, false }

// Identity has no answer off Linux: without a start time a pid alone cannot
// say its process is the one that labelled a container, so no container is
// labelled with an owner and the startup sweep removes nothing here.
func Identity() (string, bool) { return "", false }
