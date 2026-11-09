package trust

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/keyhandover"
)

// FetchHandovers reads the chain of signing key handovers the registry
// serves beside its key set. A registry that serves none (an older one
// answers 404) is an empty chain, not an error: there is nothing to adopt.
func FetchHandovers(ctx context.Context, hc *http.Client, registryURL string) (keyhandover.Chain, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(registryURL, "/")+"/.well-known/agentry/signing-key-handovers.json", nil)
	if err != nil {
		return keyhandover.Chain{}, err
	}
	res, err := orDefault(hc).Do(req)
	if err != nil {
		return keyhandover.Chain{}, fmt.Errorf("fetch signing key handovers: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusNotFound {
		return keyhandover.Chain{}, nil
	}
	if res.StatusCode != http.StatusOK {
		return keyhandover.Chain{}, fmt.Errorf("signing key handovers: HTTP %d", res.StatusCode)
	}
	var chain keyhandover.Chain
	if err := json.NewDecoder(res.Body).Decode(&chain); err != nil {
		return keyhandover.Chain{}, fmt.Errorf("decode signing key handovers: %w", err)
	}
	return chain, nil
}

// adoptHandovers adds to set every key the chain hands over to from a key
// already in it, each link verified with the key before it, and records
// each outgoing key as retired from the handover's time. Nothing else
// changes: no pinned key is removed or replaced, and a key the chain names
// with no verified link to a pinned key is never added. It returns the
// handovers it adopted, in order.
func adoptHandovers(set *KeySet, registryURL string, chain keyhandover.Chain) ([]keyhandover.Handover, error) {
	trusted := make([]keyhandover.Key, 0, len(set.Keys))
	for _, k := range set.Keys {
		trusted = append(trusted, keyhandover.Key{KeyID: k.KeyID, Algorithm: k.Algorithm, PublicKeyPEM: k.PublicKey})
	}
	adopted, err := keyhandover.Follow(registryURL, trusted, chain, normalizeRegistry)
	if err != nil || len(adopted) == 0 {
		return nil, err
	}
	for _, h := range adopted {
		for i := range set.Keys {
			id, err := keyhandover.KeyID(set.Keys[i].PublicKey)
			if err == nil && id == h.From && !set.Keys[i].Retired() {
				set.Keys[i].Status = KeyStatusRetired
				set.Keys[i].RetiredAt = h.At.UTC().Format(time.RFC3339)
			}
		}
		set.Keys = append(set.Keys, SigningKey{
			KeyID: h.To.KeyID, Algorithm: h.To.Algorithm, PublicKey: h.To.PublicKeyPEM, Status: KeyStatusActive,
		})
	}
	return adopted, nil
}

// AdoptHandedOverKeys pins the keys the registry's chain of handovers
// hands over to from a key already pinned for it, so what a rotated key
// signs verifies without the person re-pinning. It writes the registry's
// own pin file and nothing else, and does nothing while
// AGENTRY_PIN_REGISTRY points the pins at another registry, or when
// nothing is pinned yet (the first pin is the person's, at sign-in). The
// note says what was adopted, once; it is empty when nothing was.
func AdoptHandedOverKeys(ctx context.Context, hc *http.Client, registryURL string) (string, error) {
	if os.Getenv(EnvPinRegistry) != "" && normalizeRegistry(os.Getenv(EnvPinRegistry)) != normalizeRegistry(registryURL) {
		return "", nil
	}
	own, err := pinFile(registryURL)
	if err != nil {
		return "", err
	}
	set, err := loadPinnedFile(own)
	if err != nil || len(set.Keys) == 0 {
		return "", err
	}
	chain, err := FetchHandovers(ctx, hc, registryURL)
	if err != nil || len(chain.Handovers) == 0 {
		return "", err
	}
	adopted, err := adoptHandovers(&set, registryURL, chain)
	if err != nil || len(adopted) == 0 {
		return "", err
	}
	if err := savePinnedFile(own, set); err != nil {
		return "", fmt.Errorf("pin the handed-over signing key: %w", err)
	}
	return handoverNote(adopted), nil
}

// handoverNote says what adopting the handovers did, in one line.
func handoverNote(adopted []keyhandover.Handover) string {
	first, last := adopted[0], adopted[len(adopted)-1]
	if len(adopted) == 1 {
		return fmt.Sprintf("the registry rotated its signing key: key %s, which you had pinned, handed over to key %s on %s, so that key is pinned now",
			first.From, last.To.KeyID, last.At.UTC().Format("2006-01-02"))
	}
	return fmt.Sprintf("the registry rotated its signing key %d times: key %s, which you had pinned, handed over to key %s through a chain of handovers ending on %s, so that key is pinned now",
		len(adopted), first.From, last.To.KeyID, last.At.UTC().Format("2006-01-02"))
}
