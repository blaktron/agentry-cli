//go:build linux

package beneath

import "testing"

// eachMode runs f with paths resolved by openat2, when this machine answers
// it, and again with the os.Root fallback.
func eachMode(t *testing.T, f func(t *testing.T, wantBeneath bool)) {
	t.Run("openat2", func(t *testing.T) {
		if !usable() {
			t.Skip("this machine does not answer openat2 with RESOLVE_BENEATH")
		}
		f(t, true)
	})
	t.Run("fallback", func(t *testing.T) {
		disabled = true
		defer func() { disabled = false }()
		f(t, false)
	})
}
