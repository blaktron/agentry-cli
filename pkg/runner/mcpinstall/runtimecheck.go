package mcpinstall

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// RuntimeMismatch says why a resolved server's own declared runtime range
// (Resolved.Runtime: npm's engines.node, PyPI's requires_python) excludes
// the runtime it would run on, node or python being that runtime's version
// (policy.DefaultNodeVersion, policy.DefaultPythonVersion), or "" when the
// range admits it or the release declares none (agentry-cli#609). A range
// that cannot be read admits nothing, as npm's engine-strict and pip both
// treat one. It reads the registry's metadata, not the artifact: the npm
// builder's check at install reads the tarball itself (agentry-cli#608).
func RuntimeMismatch(r Resolved, node, python string) string {
	if r.Runtime == "" {
		return ""
	}
	switch r.Source {
	case "npm":
		ok, valid := npmRangeAdmits(r.Runtime, node)
		switch {
		case !valid:
			return fmt.Sprintf("its engines.node %q is not a range npm can read, so no Node satisfies it", r.Runtime)
		case !ok:
			return fmt.Sprintf("it needs Node %s (its engines.node), and runs use Node %s", r.Runtime, node)
		}
	case "pypi":
		ok, valid := pep440Admits(r.Runtime, python)
		switch {
		case !valid:
			return fmt.Sprintf("its requires_python %q is not a specifier pip can read, so no Python satisfies it", r.Runtime)
		case !ok:
			return fmt.Sprintf("it needs Python %s (its requires_python), and runs use Python %s", r.Runtime, python)
		}
	}
	return ""
}

// npmRangeAdmits evaluates an npm semver range. Masterminds' grammar is
// npm's for the ranges packages declare (comparators joined by spaces or
// commas, "||", ^, ~, x-ranges, hyphen ranges).
func npmRangeAdmits(rng, version string) (ok, valid bool) {
	c, err := semver.NewConstraint(rng)
	if err != nil {
		return false, false
	}
	v, err := semver.NewVersion(version)
	if err != nil {
		return false, false
	}
	return c.Check(v), true
}

// pep440Admits evaluates a PEP 440 specifier set ("" admits everything)
// against a final release version, for the clauses requires_python uses:
// ==, !=, >=, <=, >, <, ~= and ===, each comma-separated, with a trailing
// .* on == and !=. Pre-, post- and dev-release clauses are not read.
func pep440Admits(spec, version string) (ok, valid bool) {
	v, good := releaseParts(version)
	if !good {
		return false, false
	}
	ok = true
	for _, clause := range strings.Split(spec, ",") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		op := ""
		for _, o := range []string{"===", "~=", "==", "!=", ">=", "<=", ">", "<"} {
			if strings.HasPrefix(clause, o) {
				op = o
				break
			}
		}
		if op == "" {
			return false, false
		}
		target := strings.TrimSpace(clause[len(op):])
		wild := strings.HasSuffix(target, ".*")
		if wild && op != "==" && op != "!=" {
			return false, false
		}
		target = strings.TrimSuffix(target, ".*")
		if op == "===" {
			ok = ok && target == version
			continue
		}
		t, good := releaseParts(target)
		if !good {
			return false, false
		}
		var hold bool
		switch op {
		case "==", "!=":
			if wild {
				hold = prefixEqual(v, t)
			} else {
				hold = compareRelease(v, t) == 0
			}
			if op == "!=" {
				hold = !hold
			}
		case ">=":
			hold = compareRelease(v, t) >= 0
		case "<=":
			hold = compareRelease(v, t) <= 0
		case ">":
			hold = compareRelease(v, t) > 0
		case "<":
			hold = compareRelease(v, t) < 0
		case "~=":
			if len(t) < 2 {
				return false, false
			}
			hold = compareRelease(v, t) >= 0 && prefixEqual(v, t[:len(t)-1])
		}
		ok = ok && hold
	}
	return ok, true
}

// releaseParts is a release segment's numbers ("3.12.14" → 3 12 14).
func releaseParts(s string) ([]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return nil, false
	}
	var out []int
	for _, p := range strings.Split(s, ".") {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// compareRelease compares two release segments, padding the shorter with
// zeros as PEP 440 does.
func compareRelease(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// prefixEqual says v starts with the release p (zero-padded).
func prefixEqual(v, p []int) bool {
	for i, n := range p {
		x := 0
		if i < len(v) {
			x = v[i]
		}
		if x != n {
			return false
		}
	}
	return true
}
