// Package remote is the client of a registry's knowledge routes (plan
// knowledge.md §4.3, §4.4; agentry-cli#319): wrap and unwrap a pack's data
// key, create a pack, upload and download its sealed tree, list and delete,
// for the account a session token or a personal access token with
// knowledge:read and knowledge:write speaks for. It implements
// store.Registry for the CLI and the desktop alike.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/blaktron/agentry/pkg/apiclient"

	"github.com/blaktron/agentry-cli/pkg/knowledge/store"
)

// Client is one registry, as one account.
type Client struct {
	url string
	api *apiclient.ClientWithResponses
}

var _ store.Registry = (*Client)(nil)

// New is the client of the registry at regURL (no trailing slash) with
// token as its bearer credential. hc is the HTTP client every request goes
// through (nil: http.DefaultClient, which has no timeout; callers pass
// their own).
func New(regURL, token string, hc *http.Client) (*Client, error) {
	regURL = strings.TrimRight(regURL, "/")
	if token == "" {
		return nil, fmt.Errorf("%s: %w", regURL, store.ErrSignedOut)
	}
	opts := []apiclient.ClientOption{apiclient.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	})}
	if hc != nil {
		opts = append(opts, apiclient.WithHTTPClient(hc))
	}
	api, err := apiclient.NewClientWithResponses(regURL+"/api/v1", opts...)
	if err != nil {
		return nil, err
	}
	return &Client{url: regURL, api: api}, nil
}

// URL implements store.Registry.
func (c *Client) URL() string { return c.url }

// Wrap implements store.Registry: POST /knowledge/keys/wrap.
func (c *Client) Wrap(ctx context.Context, dataKey []byte) (store.WrappedKey, error) {
	res, err := c.api.WrapKnowledgeKeyWithResponse(ctx, apiclient.KnowledgeDataKey{Key: dataKey})
	if err != nil {
		return store.WrappedKey{}, err
	}
	if res.JSON200 == nil {
		return store.WrappedKey{}, c.problem("wrap a knowledge key", res.HTTPResponse, res.ApplicationproblemJSONDefault, res.Body)
	}
	return fromAPIKey(*res.JSON200), nil
}

// Unwrap implements store.Registry: POST /knowledge/keys/unwrap.
func (c *Client) Unwrap(ctx context.Context, wk store.WrappedKey) ([]byte, error) {
	res, err := c.api.UnwrapKnowledgeKeyWithResponse(ctx, toAPIKey(wk))
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, c.problem("unwrap a knowledge key", res.HTTPResponse, res.ApplicationproblemJSONDefault, res.Body)
	}
	return res.JSON200.Key, nil
}

// Create implements store.Registry: POST /knowledge/packs.
func (c *Client) Create(ctx context.Context, name string, wk store.WrappedKey) (store.Remote, error) {
	res, err := c.api.CreateKnowledgePackWithResponse(ctx, apiclient.CreateKnowledgePackRequest{Name: name, WrappedKey: toAPIKey(wk)})
	if err != nil {
		return store.Remote{}, err
	}
	if res.JSON201 == nil {
		return store.Remote{}, c.problem("create knowledge pack "+name, res.HTTPResponse, res.ApplicationproblemJSONDefault, res.Body)
	}
	return fromAPIPack(*res.JSON201), nil
}

// PutTree implements store.Registry: PUT /knowledge/packs/{id}/tree.
func (c *Client) PutTree(ctx context.Context, id string, sealed []byte) (store.Remote, error) {
	pid, err := packID(id)
	if err != nil {
		return store.Remote{}, err
	}
	res, err := c.api.UploadKnowledgeTreeWithBodyWithResponse(ctx, pid, "application/octet-stream", bytes.NewReader(sealed))
	if err != nil {
		return store.Remote{}, err
	}
	if res.JSON202 == nil {
		return store.Remote{}, c.problem("upload the knowledge tree", res.HTTPResponse, res.ApplicationproblemJSONDefault, res.Body)
	}
	return fromAPIPack(*res.JSON202), nil
}

// List implements store.Registry: GET /knowledge/packs.
func (c *Client) List(ctx context.Context) ([]store.Remote, error) {
	res, err := c.api.ListKnowledgePacksWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, c.problem("list knowledge packs", res.HTTPResponse, res.ApplicationproblemJSONDefault, res.Body)
	}
	out := make([]store.Remote, 0, len(*res.JSON200))
	for _, p := range *res.JSON200 {
		out = append(out, fromAPIPack(p))
	}
	return out, nil
}

// GetTree implements store.Registry: GET /knowledge/packs/{id}/tree.
func (c *Client) GetTree(ctx context.Context, id string) ([]byte, string, error) {
	pid, err := packID(id)
	if err != nil {
		return nil, "", err
	}
	res, err := c.api.DownloadKnowledgeTreeWithResponse(ctx, pid)
	if err != nil {
		return nil, "", err
	}
	if res.StatusCode() != http.StatusOK {
		return nil, "", c.problem("download the knowledge tree", res.HTTPResponse, res.ApplicationproblemJSONDefault, res.Body)
	}
	return res.Body, res.HTTPResponse.Header.Get("X-Agentry-Knowledge-Digest"), nil
}

// Delete implements store.Registry: DELETE /knowledge/packs/{id}.
func (c *Client) Delete(ctx context.Context, id string) error {
	pid, err := packID(id)
	if err != nil {
		return err
	}
	res, err := c.api.DeleteKnowledgePackWithResponse(ctx, pid)
	if err != nil {
		return err
	}
	if res.StatusCode() != http.StatusNoContent {
		return c.problem("delete the knowledge pack in Agentry", res.HTTPResponse, res.ApplicationproblemJSONDefault, res.Body)
	}
	return nil
}

func packID(id string) (openapi_types.UUID, error) {
	var u openapi_types.UUID
	if err := u.UnmarshalText([]byte(id)); err != nil {
		return u, fmt.Errorf("registry pack id %q: %w", id, err)
	}
	return u, nil
}

func toAPIKey(wk store.WrappedKey) apiclient.KnowledgeWrappedKey {
	return apiclient.KnowledgeWrappedKey{Kind: wk.Kind, Recipient: wk.Recipient, Key: wk.Key}
}

func fromAPIKey(k apiclient.KnowledgeWrappedKey) store.WrappedKey {
	return store.WrappedKey{Kind: k.Kind, Recipient: k.Recipient, Key: k.Key}
}

func fromAPIPack(p apiclient.KnowledgePack) store.Remote {
	r := store.Remote{
		ID: p.Id.String(), Name: p.Name, State: string(p.State),
		Bytes: p.Bytes, Files: p.Files, Pages: p.Pages, Digest: p.Digest,
		WrappedKey: fromAPIKey(p.WrappedKey), Created: p.CreatedAt, Updated: p.UpdatedAt,
	}
	if p.Scan != nil {
		if raw, err := json.Marshal(p.Scan); err == nil {
			r.Scan = raw
		}
	}
	return r
}

// problem turns a refusal into an error: the store's sentinel where the
// store acts on it, wrapped with the registry's own sentence, bounded and
// stripped of control characters before a terminal prints it.
func (c *Client) problem(op string, res *http.Response, p *apiclient.Problem, body []byte) error {
	status := 0
	if res != nil {
		status = res.StatusCode
	}
	if p == nil && len(body) > 0 {
		var parsed apiclient.Problem
		if json.Unmarshal(body, &parsed) == nil && parsed.Title != "" {
			p = &parsed
		}
	}
	slug, sentence := "", fmt.Sprintf("HTTP %d", status)
	if p != nil {
		if i := strings.LastIndex(p.Type, "/"); i >= 0 {
			slug = p.Type[i+1:]
		}
		sentence = safe(p.Title, 120)
		if p.Detail != nil && *p.Detail != "" {
			sentence += ": " + safe(*p.Detail, 400)
		}
	}
	var sentinel error
	switch {
	case status == http.StatusUnauthorized:
		sentinel = store.ErrSignedOut
		sentence += "; sign in again with `agentry login`"
	case status == http.StatusForbidden:
		sentence += "; sign in again with `agentry login` for a token that reaches your knowledge (knowledge:read, knowledge:write)"
	case slug == "knowledge-key-not-found":
		sentinel = store.ErrKeyNotYours
	case slug == "knowledge-scanning":
		sentinel = store.ErrScanning
	case slug == "knowledge-name-taken":
		sentinel = store.ErrRemoteNameTaken
	case slug == "knowledge-unconfigured":
		sentinel = store.ErrUnconfigured
	case status == http.StatusNotFound:
		sentinel = store.ErrRemoteNotFound
	}
	msg := fmt.Sprintf("%s at %s: %s", op, c.url, sentence)
	if sentinel != nil {
		return fmt.Errorf("%s: %w", msg, sentinel)
	}
	return errors.New(msg)
}

// safe bounds a server-supplied string and drops control characters, so
// nothing a registry answers can rewrite the terminal it is printed to.
func safe(s string, limit int) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= limit {
			b.WriteString("…")
			break
		}
	}
	return b.String()
}
