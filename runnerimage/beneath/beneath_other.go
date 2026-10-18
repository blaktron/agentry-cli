//go:build !linux

package beneath

// openFD is never used: there is no openat2 off Linux.
func openFD(string) (impl, bool, error) { return nil, false, nil }
