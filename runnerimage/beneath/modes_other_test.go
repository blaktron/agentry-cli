//go:build !linux

package beneath

import "testing"

// eachMode runs f once: off Linux a Dir is an os.Root.
func eachMode(t *testing.T, f func(t *testing.T, wantBeneath bool)) {
	f(t, false)
}
