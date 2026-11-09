package managed

// The minimum version a document may lock: which version of agentry this
// process is, and whether it is older than the organization's floor.

import (
	"fmt"
	"math"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/internal/version"
)

// clientVersion is the version of agentry this process runs: the CLI's
// stamped version, or, in an application that links the runner, the
// version of the agentry module it was built with.
var clientVersion = func() string {
	if version.Version != "" && !strings.HasPrefix(version.Version, "0.0.0") {
		return version.Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Path == "github.com/blaktron/agentry-cli" {
			return bi.Main.Version
		}
		for _, dep := range bi.Deps {
			if dep.Path == "github.com/blaktron/agentry-cli" {
				if dep.Replace != nil && dep.Replace.Version != "" {
					return dep.Replace.Version
				}
				return dep.Version
			}
		}
	}
	return version.Version
}

// release reads "vX.Y.Z" or "X.Y.Z", with any pre-release or build suffix,
// as its three numbers and whether it carried a pre-release.
func release(v string) ([3]int, bool, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	pre := false
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v, pre = v[:i], true
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [3]int{}, false, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > math.MaxInt32 {
			return [3]int{}, false, false
		}
		out[i] = n
	}
	return out, pre, true
}

// older says v is older than minimum: by its numbers, or the same numbers
// as a pre-release of it. A version that does not read as one is older.
func older(v, minimum string) bool {
	want, _, ok := release(minimum)
	if !ok {
		return false
	}
	have, pre, ok := release(v)
	if !ok {
		return true
	}
	for i := range have {
		if have[i] != want[i] {
			return have[i] < want[i]
		}
	}
	return pre
}

// VersionRefusal is the refusal of every run by a client older than a
// document's minimum version, naming the update, or "".
func (e Effective) VersionRefusal() string {
	v := clientVersion()
	for _, d := range e.Documents {
		s, ok := d.Document.Settings[managedsettings.UpdatesMinimumVersion]
		if !ok || !s.Locked {
			continue
		}
		if minimum := s.String(); older(v, minimum) {
			return fmt.Sprintf("%s's managed settings need agentry %s or later (%s), and this is %s; update Agentry to run: %s/download", d.Document.Org, minimum, managedsettings.UpdatesMinimumVersion, v, strings.TrimSuffix(d.Document.Registry, "/"))
		}
	}
	return ""
}
