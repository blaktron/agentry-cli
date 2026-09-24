//go:build !linux

package owner

// StartTime has no portable answer off Linux, so ok is always false: a pid
// alone cannot be told from a pid the kernel reused.
func StartTime(_ int) (uint64, bool) { return 0, false }

func ownerDead(_ int, _ uint64) bool { return false }

// Identity has no answer off Linux: without a start time a pid alone cannot
// say its process is the one that labelled a container, so no container is
// labelled with an owner and the startup sweep removes nothing here.
func Identity() (string, bool) { return "", false }
