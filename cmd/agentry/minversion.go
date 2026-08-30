// Minimum-CLI-version enforcement (§11 distribution note: "CLI checks
// registry `meta` for min-supported-version").
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/blaktron/agentry-cli/internal/version"
)

// The policy, stated once so help text and behavior cannot drift:
//
//	same major, older than minCLIVersion  → WARN, continue
//	older major than minCLIVersion        → REFUSE
//	at or above minCLIVersion             → silent
//	meta unreachable, or no minCLIVersion → silent
//
// The split is about what "too old" costs. Within a major, an old CLI talks a
// compatible API and merely misses features, so refusing would break working
// setups over a cosmetic gap — a warning is the honest signal. Across a major,
// the registry is telling us the wire contract moved; continuing would produce
// confusing failures deep inside a command instead of one clear line at the
// top. A registry that says nothing constrains nothing, and an unreachable
// registry is the command's own problem to report, not a reason to refuse
// before trying.

// checkedMinVersion makes the check once per process: every command's FIRST
// API call triggers it, and later calls in the same invocation stay quiet.
var checkedMinVersion sync.Once

// envSkipVersionCheck disables the check entirely (air-gapped mirrors, and
// the CLI's own tests).
const envSkipVersionCheck = "AGENTRY_SKIP_VERSION_CHECK"

// minVersionGate fetches GET /meta once per invocation and applies the policy
// above. It returns a non-nil error only in the refuse case; warnings go to w.
func minVersionGate(ctx context.Context, w io.Writer, regURL, token string) error {
	if os.Getenv(envSkipVersionCheck) != "" {
		return nil
	}
	var refusal error
	checkedMinVersion.Do(func() {
		refusal = runMinVersionCheck(ctx, w, regURL, token)
	})
	return refusal
}

func runMinVersionCheck(ctx context.Context, w io.Writer, regURL, token string) error {
	client, err := newAPIClient(regURL, token)
	if err != nil {
		return nil // the command's own call will report this properly
	}
	resp, err := client.GetMetaWithResponse(ctx)
	if err != nil || resp.JSON200 == nil || resp.JSON200.MinCLIVersion == nil {
		return nil
	}
	minv := strings.TrimSpace(*resp.JSON200.MinCLIVersion)
	if minv == "" {
		return nil
	}

	have := version.Version
	cmp, ok := compareSemver(have, minv)
	if !ok || cmp >= 0 {
		return nil
	}
	if majorOf(have) < majorOf(minv) {
		return fmt.Errorf("this CLI is version %s; %s requires at least %s — a full major behind, so the API contract has moved. Upgrade agentry (see the registry's docs) or set %s=1 to proceed anyway",
			have, regURL, minv, envSkipVersionCheck)
	}
	_, _ = fmt.Fprintf(w, "warning: agentry %s is older than %s's minimum supported version %s — some commands may not work; upgrade when convenient\n",
		have, regURL, minv)
	return nil
}

// compareSemver compares two MAJOR.MINOR.PATCH strings, ignoring any
// prerelease/build suffix. ok is false when either side is not parseable —
// a development build ("0.0.0-dev") compares fine, but a garbage string
// must not be turned into a refusal.
func compareSemver(a, b string) (int, bool) {
	av, aok := parseSemver(a)
	bv, bok := parseSemver(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range av {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseSemver extracts [major, minor, patch] from a version string, tolerating
// a leading "v" and any "-prerelease"/"+build" suffix.
func parseSemver(s string) ([3]int, bool) {
	var out [3]int
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "v"))
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// majorOf returns the major component, or -1 when unparseable.
func majorOf(s string) int {
	v, ok := parseSemver(s)
	if !ok {
		return -1
	}
	return v[0]
}
