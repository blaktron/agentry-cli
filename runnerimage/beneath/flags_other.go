//go:build !unix

package beneath

// Neither flag exists here; the runner's tools run on Linux, and these
// systems only build the package.
const (
	nonBlock = 0
	noFollow = 0
)
