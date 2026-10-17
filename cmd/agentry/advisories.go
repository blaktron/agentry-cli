// The advisories on a QuickAgent this machine holds: asked of the registry
// with the archive digest of the release laid down here, which proves the
// holding, so a QuickAgent that has since gone private, been retired or been
// deleted still answers its advisories, signed in or not.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/blaktron/agentry/pkg/apiclient"

	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
)

// heldDigest is the archive digest of the release of ns/name at version laid
// down under the runner home, read from its install.json, or "" when none is.
func heldDigest(home, ns, name, version string) string {
	matches, _ := filepath.Glob(filepath.Join(installdir.AgentDir(home, ns, name), version+"-*", "install.json"))
	for _, p := range matches {
		b, err := os.ReadFile(p) //nolint:gosec // the runner home's own install record
		if err != nil {
			continue
		}
		var meta installdir.Meta
		if json.Unmarshal(b, &meta) != nil || meta.Version != version || !strings.HasPrefix(meta.Digest, "sha256:") {
			continue
		}
		if filepath.Base(filepath.Dir(p)) == installdir.ReleaseName(meta.Version, meta.Digest) {
			return meta.Digest
		}
	}
	return ""
}

// advisoryView is one advisory as list and doctor report it.
type advisoryView struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
	Affected string `json:"affectedVersions"`
}

// heldAdvisories is what the registry said about a held release's
// advisories: the list, that it does not know the QuickAgent, or why it
// could not be asked. A lookup that failed is never read as "none".
type heldAdvisories struct {
	Advisories []advisoryView
	Unknown    bool
	Err        string
}

// lookupHeldAdvisories asks client for the advisories affecting ns/name,
// presenting digest as the release held.
func lookupHeldAdvisories(ctx context.Context, client *apiclient.ClientWithResponses, ns, name, digest string) heldAdvisories {
	affecting := ns + "/" + name
	params := &apiclient.ListAdvisoriesParams{Affecting: &affecting}
	if digest != "" {
		params.Digest = &digest
	}
	res, err := client.ListAdvisoriesWithResponse(ctx, params)
	if err != nil {
		return heldAdvisories{Err: err.Error()}
	}
	switch {
	case res.StatusCode() == http.StatusNotFound:
		return heldAdvisories{Unknown: true}
	case res.JSON200 == nil:
		msg := res.Status()
		if p := res.ApplicationproblemJSONDefault; p != nil && p.Title != "" {
			msg += ": " + p.Title
		}
		return heldAdvisories{Err: msg}
	}
	out := heldAdvisories{Advisories: []advisoryView{}}
	for _, a := range *res.JSON200 {
		out.Advisories = append(out.Advisories, advisoryView{ID: a.Id.String(), Severity: a.Severity, Summary: a.Summary, Affected: a.AffectedVersions})
	}
	return out
}

// advisoriesLine says a lookup's outcome in one line.
func advisoriesLine(h heldAdvisories) string {
	switch {
	case h.Err != "":
		return "advisories could not be checked: " + h.Err
	case h.Unknown:
		return "not known to the registry"
	case len(h.Advisories) == 0:
		return "no advisories"
	}
	parts := make([]string, 0, len(h.Advisories))
	for _, a := range h.Advisories {
		parts = append(parts, a.Severity+": "+a.Summary+" (affects "+a.Affected+")")
	}
	return "advisories: " + strings.Join(parts, "; ")
}
