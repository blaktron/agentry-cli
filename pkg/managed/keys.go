package managed

// Delivered keys. When an organization's document names its own key for AI
// Review or a web search provider (review.source or webSearch.source with
// organization-key and a key reference), this client exchanges the
// reference for the key once per revision, as the document is refreshed:
// it makes a key pair for the one request, the registry seals the key to
// it, and only this client opens it (managedsettings.SealDelivery). The key
// is kept in the keychain under managed:<org>:<provider>, apart from the
// member's own provider:<provider>, and never shown, planned or recorded.
// A record beside the cached document names each key's account and
// revision, never its value. A key the document no longer names is
// deleted, and so is every one when the document is.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// KeyAccount is the keychain account an organization's delivered key for
// provider is kept under.
func KeyAccount(org, provider string) string { return "managed:" + org + ":" + provider }

// deliveredKey is one delivered key as the record names it: never its
// value. Err is why the last exchange failed, kept for the sentence a run
// that needs the key is refused with.
type deliveredKey struct {
	Setting  string `json:"setting"`
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Key      string `json:"key"`
	Rev      int64  `json:"rev"`
	Held     bool   `json:"held"`
	Err      string `json:"err,omitempty"`
}

type keysFile struct {
	Keys []deliveredKey `json:"keys"`
}

func keysPath(reg string) (string, error) {
	doc, _, err := paths(reg)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(doc, ".json") + ".keys", nil
}

func readKeys(reg string) (keysFile, error) {
	var k keysFile
	p, err := keysPath(reg)
	if err != nil {
		return k, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return k, nil
	}
	if err != nil {
		return k, err
	}
	return k, json.Unmarshal(raw, &k)
}

func writeKeys(reg string, k keysFile) error {
	p, err := keysPath(reg)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(p, append(raw, '\n'))
}

// keyRef is a delivering row's reference to the organization's key.
type keyRef struct {
	Setting  string
	Provider string
	Ref      managedsettings.KeyRef
}

// keyRefs are the references doc's delivering rows make with
// organization-key.
func keyRefs(doc managedsettings.Document) []keyRef {
	var out []keyRef
	for _, id := range []string{managedsettings.ReviewSource, managedsettings.WebSearchSource} {
		s, ok := doc.Settings[id]
		if !ok {
			continue
		}
		var v managedsettings.Source
		if json.Unmarshal(s.Value, &v) != nil || v.Source != managedsettings.SourceOrganizationKey || v.Key == nil {
			continue
		}
		provider := v.Provider
		if id == managedsettings.ReviewSource {
			provider = policy.JudgeKeyProvider
		}
		out = append(out, keyRef{Setting: id, Provider: provider, Ref: *v.Key})
	}
	return out
}

// deliveryWait bounds how long an exchange waits for the registry to seal
// the key, and deliveryPoll is how often it asks; variables for tests.
var (
	deliveryWait = time.Minute
	deliveryPoll = 2 * time.Second
)

// syncKeys holds the keys doc names at their revisions: one held at the
// named revision is kept, another is fetched, and one no longer named is
// deleted. A failed exchange is recorded, not returned: the document still
// applies, and a run that needs the key is refused with the reason.
func syncKeys(ctx context.Context, hc *http.Client, reg, token, account string, doc managedsettings.Document) error {
	prev, err := readKeys(reg)
	if err != nil {
		prev = keysFile{}
	}
	var next keysFile
	for _, r := range keyRefs(doc) {
		acct := KeyAccount(doc.Org, r.Provider)
		i := slices.IndexFunc(prev.Keys, func(k deliveredKey) bool { return k.Account == acct })
		if i >= 0 && prev.Keys[i].Held && prev.Keys[i].Key == r.Ref.ID && prev.Keys[i].Rev == r.Ref.Rev {
			next.Keys = append(next.Keys, prev.Keys[i])
			continue
		}
		k := deliveredKey{Setting: r.Setting, Provider: r.Provider, Account: acct, Key: r.Ref.ID, Rev: r.Ref.Rev}
		value, err := deliver(ctx, hc, reg, token, account, doc, r.Ref)
		if err == nil {
			err = modelkey.SetAccount(acct, value)
		}
		if err != nil {
			k.Err = err.Error()
		} else {
			k.Held = true
		}
		next.Keys = append(next.Keys, k)
	}
	for _, k := range prev.Keys {
		if !slices.ContainsFunc(next.Keys, func(n deliveredKey) bool { return n.Account == k.Account }) {
			_ = modelkey.DeleteAccount(k.Account)
		}
	}
	return writeKeys(reg, next)
}

// removeKeys deletes every key delivered for reg, and the record.
func removeKeys(reg string) error {
	k, err := readKeys(reg)
	if err != nil {
		return err
	}
	for _, d := range k.Keys {
		if err := modelkey.DeleteAccount(d.Account); err != nil {
			return fmt.Errorf("delete a key %s delivered: %w", reg, err)
		}
	}
	p, err := keysPath(reg)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// deliver exchanges ref for the key, sealed to a key pair made for this
// one request.
func deliver(ctx context.Context, hc *http.Client, reg, token, account string, doc managedsettings.Document, ref managedsettings.KeyRef) (string, error) {
	kp, err := managedsettings.NewDeliveryKey()
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{"rev": ref.Rev, "suite": managedsettings.DeliverySuite,
		"publicKey": base64.StdEncoding.EncodeToString(kp.PublicKey())})
	res, err := send(ctx, hc, http.MethodPost, reg+"/api/v1/x/vault/me/org-keys/"+ref.ID+"/deliveries", token, body)
	if err != nil {
		return "", err
	}
	var asked struct {
		ID string `json:"id"`
	}
	if err := answer(res, http.StatusAccepted, &asked); err != nil {
		return "", fmt.Errorf("ask %s for the organization's key: %w", reg, err)
	}
	deadline := time.Now().Add(deliveryWait)
	for {
		res, err := send(ctx, hc, http.MethodGet, reg+"/api/v1/x/vault/me/org-key-deliveries/"+asked.ID, token, nil)
		if err != nil {
			return "", err
		}
		if res.StatusCode == http.StatusAccepted {
			_ = res.Body.Close()
			if time.Now().After(deadline) {
				return "", fmt.Errorf("%s did not seal the organization's key within %s", reg, deliveryWait)
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(deliveryPoll):
			}
			continue
		}
		var got struct {
			Sealed string `json:"sealed"`
		}
		if err := answer(res, http.StatusOK, &got); err != nil {
			return "", fmt.Errorf("fetch the organization's key from %s: %w", reg, err)
		}
		sealed, err := base64.StdEncoding.DecodeString(got.Sealed)
		if err != nil {
			return "", fmt.Errorf("fetch the organization's key from %s: the answer is not base64", reg)
		}
		value, err := kp.Open(managedsettings.Delivery{Registry: doc.Registry, Org: doc.Org, Account: account, Key: ref.ID, Rev: ref.Rev}, sealed)
		if err != nil {
			return "", err
		}
		return string(value), nil
	}
}

func send(ctx context.Context, hc *http.Client, method, url, token string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return hc.Do(req)
}

// answer reads a JSON answer of status want into v, or says what the
// registry answered instead: its problem's detail, never a body.
func answer(res *http.Response, want int, v any) error {
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxEnvelopeBytes))
	if err != nil {
		return err
	}
	if res.StatusCode != want {
		var p struct {
			Detail string `json:"detail"`
		}
		if json.Unmarshal(raw, &p) == nil && p.Detail != "" {
			return fmt.Errorf("HTTP %d: %s", res.StatusCode, p.Detail)
		}
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return json.Unmarshal(raw, v)
}

// DeliveredKey is the organization's key for provider delivered under
// setting by the documents this computer holds, read from the keychain:
// found is false when no document names one. An error says why a named key
// is not here.
func (e Effective) DeliveredKey(setting, provider string) (value string, found bool, err error) {
	for _, d := range e.Documents {
		for _, r := range keyRefs(d.Document) {
			if r.Setting != setting || r.Provider != provider {
				continue
			}
			k, _ := readKeys(d.Registry)
			i := slices.IndexFunc(k.Keys, func(x deliveredKey) bool { return x.Account == KeyAccount(d.Document.Org, provider) })
			switch {
			case i < 0 || k.Keys[i].Rev != r.Ref.Rev:
				return "", true, fmt.Errorf("%s's key for %s has not been delivered to this computer yet; run agentry managed refresh", d.Document.Org, provider)
			case !k.Keys[i].Held:
				return "", true, fmt.Errorf("%s's key for %s could not be delivered: %s; run agentry managed refresh", d.Document.Org, provider, k.Keys[i].Err)
			}
			v, err := modelkey.LookupAccount(k.Keys[i].Account)
			if err != nil {
				return "", true, fmt.Errorf("%s's key for %s: %w", d.Document.Org, provider, err)
			}
			return v, true, nil
		}
	}
	return "", false, nil
}

// KeySource is what the documents say pays for a service: a delivering
// row's source, its strength, the provider an organization's key is for,
// the organization, and the registry its document came from. Decided is
// false when no document sets the row.
type KeySource struct {
	Decided  bool
	Locked   bool
	Source   string
	Provider string
	Org      string
	Registry string
}

// KeySourceOf reads id (review.source or webSearch.source) from the first
// document that sets it.
func (e Effective) KeySourceOf(id string) KeySource {
	for _, d := range e.Documents {
		s, ok := d.Document.Settings[id]
		if !ok {
			continue
		}
		var v managedsettings.Source
		_ = json.Unmarshal(s.Value, &v)
		return KeySource{Decided: true, Locked: s.Locked, Source: v.Source, Provider: v.Provider, Org: d.Document.Org, Registry: d.Registry}
	}
	return KeySource{}
}

// what names a delivering row's service in a sentence.
var serviceWhat = map[string]string{
	managedsettings.ReviewSource:    "AI Review",
	managedsettings.WebSearchSource: "web search",
}

// ServiceKey is the key a service runs with on this computer under the
// documents, given the member's own (own, "" for none): ok false means the
// member's own key stands. A lock to off is a refusal naming the
// organization and the setting. When the organization's credits pay (a
// lock to agentry-credits, or that default and no key of the member's), the
// error is a *PaidWithCredits: the caller opens an operation on the
// registry (OpenOperation) and works through it, or refuses with the
// error's sentence.
func (e Effective) ServiceKey(id, provider, own string) (key string, ok bool, err error) {
	ks := e.KeySourceOf(id)
	if !ks.Decided || ks.Source == managedsettings.SourceMember {
		return "", false, nil
	}
	if !ks.Locked && own != "" {
		// An organization default gives way to the member's own key.
		return "", false, nil
	}
	switch ks.Source {
	case managedsettings.SourceOrganizationKey:
		v, found, err := e.DeliveredKey(id, provider)
		if !found {
			return "", false, nil
		}
		return v, true, err
	case managedsettings.SourceOff:
		if ks.Locked {
			return "", true, fmt.Errorf("%s is off by %s's managed settings (%s)", sentenceCase(serviceWhat[id]), ks.Org, id)
		}
	case managedsettings.SourceAgentryCredits:
		if ks.Locked || own == "" {
			return "", true, &PaidWithCredits{Setting: id, Org: ks.Org, Registry: ks.Registry, Locked: ks.Locked}
		}
	}
	return "", false, nil
}
