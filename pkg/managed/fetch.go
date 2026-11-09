package managed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/internal/origin"
	"github.com/blaktron/agentry-cli/pkg/trust"
)

// maxEnvelopeBytes bounds what a fetch reads: a document is a few
// kilobytes, and an account's own record not much more.
const maxEnvelopeBytes = 1 << 20

// Refresh asks the registry for the signed-in account's managed settings
// with the account's token, and keeps what it answers: a document that
// verifies, is bound to this registry, is the settings of the organization
// the account is an active member of, and is not older than the newest one
// accepted from that organization replaces the cache; 204 (none apply) or
// 404 (no longer a member) deletes it. It returns nil when none apply. When
// the token is another account's than the last one's, the last one's
// settings are deleted first (SignedIn). On any other answer, or a document
// that is refused, the cache is left as it was and the error says why.
func Refresh(ctx context.Context, hc *http.Client, registryURL, token string) (*State, error) {
	if token == "" {
		return nil, fmt.Errorf("not signed in to %s", registryURL)
	}
	reg := normalize(registryURL)
	hc = origin.Guard(hc)
	me, err := whoAmI(ctx, hc, reg, token)
	if err != nil {
		return nil, err
	}
	if err := SignedIn(reg, me.id); err != nil {
		return nil, err
	}
	res, err := get(ctx, hc, reg+Route, token)
	if err != nil {
		return nil, fmt.Errorf("fetch managed settings from %s: %w", reg, err)
	}
	defer func() { _ = res.Body.Close() }()
	switch res.StatusCode {
	case http.StatusNoContent, http.StatusNotFound:
		return nil, cleared(reg)
	case http.StatusOK:
	default:
		return nil, fmt.Errorf("fetch managed settings from %s: HTTP %d", reg, res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxEnvelopeBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetch managed settings from %s: %w", reg, err)
	}
	if len(body) > maxEnvelopeBytes {
		return nil, fmt.Errorf("fetch managed settings from %s: the answer is larger than %d bytes", reg, maxEnvelopeBytes)
	}
	var env managedsettings.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("managed settings from %s: decode the envelope: %w", reg, err)
	}
	doc, err := open(reg, env)
	if err != nil {
		// A document signed by a key the registry rotated to after this
		// computer pinned its keys opens once the pinned key's handover to
		// it is adopted.
		if note, aerr := adoptHandovers(ctx, hc, reg); aerr == nil && note != "" {
			doc, err = open(reg, env)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("managed settings from %s: %w", reg, err)
	}
	switch {
	case me.org == "":
		return nil, fmt.Errorf("managed settings from %s: the document is %s's, and the signed-in account is an active member of no organization", reg, doc.Org)
	case doc.Org != me.org:
		return nil, fmt.Errorf("managed settings from %s: the document is %s's, not those of %s, the organization the signed-in account is a member of", reg, doc.Org, me.org)
	}
	seen, err := readSeen(reg)
	if err != nil {
		return nil, fmt.Errorf("managed settings from %s: %w", reg, err)
	}
	if err := seen.notOlder(doc); err != nil {
		return nil, fmt.Errorf("managed settings from %s: %w", reg, err)
	}
	now := time.Now().UTC()
	raw, err := json.MarshalIndent(cacheFile{Format: cacheFormat, Registry: reg, FetchedAt: now, Envelope: env}, "", "  ")
	if err != nil {
		return nil, err
	}
	docPath, _, err := paths(reg)
	if err != nil {
		return nil, err
	}
	if err := writeFile(docPath, append(raw, '\n')); err != nil {
		return nil, fmt.Errorf("cache the managed settings from %s: %w", reg, err)
	}
	if seen.Versions == nil {
		seen.Versions = map[string]int64{}
	}
	seen.Versions[doc.Org] = max(seen.Versions[doc.Org], doc.Version)
	seen.Holds, seen.AnsweredAt = holdsDocument, now
	if err := writeSeen(reg, seen); err != nil {
		return nil, err
	}
	return &State{Registry: reg, Document: doc, FetchedAt: now}, nil
}

// cleared deletes the cached document after the registry answered that
// none apply, and records the answer.
func cleared(reg string) error {
	if err := removeDocument(reg); err != nil {
		return err
	}
	seen, err := readSeen(reg)
	if err != nil {
		seen = seenFile{}
	}
	seen.Holds, seen.AnsweredAt = holdsNone, time.Now().UTC()
	return writeSeen(reg, seen)
}

// Sync is the refresh a run makes: when the registry answered within
// maxAge and what it answered still verifies, the cached copy (or none)
// stands; otherwise it asks the registry. When the registry cannot be
// asked, the last verified copy applies and is returned beside the error.
func Sync(ctx context.Context, hc *http.Client, registryURL, token string, maxAge time.Duration) (*State, error) {
	reg := normalize(registryURL)
	cached, loadErr := Load(reg)
	if loadErr == nil && fresh(reg, maxAge) {
		return cached, nil
	}
	st, err := Refresh(ctx, hc, reg, token)
	if err != nil {
		if loadErr != nil {
			return nil, errors.Join(err, loadErr)
		}
		return cached, err
	}
	return st, nil
}

func get(ctx context.Context, hc *http.Client, url, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	return hc.Do(req)
}

// account is who a token belongs to: the account's id, and the handle of
// the organization it is an active member of, "" when none. A person
// belongs to one organization at most, and a membership an admin has yet
// to approve, or one whose state the registry does not say, is not active.
type account struct {
	id, org string
}

func whoAmI(ctx context.Context, hc *http.Client, reg, token string) (account, error) {
	res, err := get(ctx, hc, reg+"/api/v1/user", token)
	if err != nil {
		return account{}, fmt.Errorf("read the signed-in account from %s: %w", reg, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return account{}, fmt.Errorf("read the signed-in account from %s: HTTP %d", reg, res.StatusCode)
	}
	var u struct {
		ID   string `json:"id"`
		Orgs []struct {
			Handle string `json:"handle"`
			State  string `json:"state"`
		} `json:"orgs"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxEnvelopeBytes)).Decode(&u); err != nil {
		return account{}, fmt.Errorf("read the signed-in account from %s: %w", reg, err)
	}
	if u.ID == "" {
		return account{}, fmt.Errorf("read the signed-in account from %s: the answer names no account", reg)
	}
	a := account{id: u.ID}
	for _, o := range u.Orgs {
		if o.State == "active" {
			a.org = o.Handle
			break
		}
	}
	return a, nil
}

// adoptHandovers pins the signing keys the registry's pinned key handed
// over to; a package variable purely as a test seam.
var adoptHandovers = trust.AdoptHandedOverKeys
