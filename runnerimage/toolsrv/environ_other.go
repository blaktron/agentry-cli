//go:build !linux

package toolsrv

// hideEnvironment is a no-op off Linux. The tools server only runs inside
// the (Linux) runner image; off Linux it runs as `agentry runner-tools`,
// a host-side debugging path with no /proc to protect in the same way.
func hideEnvironment() error { return nil }

// environmentHideable reports whether hideEnvironment protects anything on
// this platform.
const environmentHideable = false
