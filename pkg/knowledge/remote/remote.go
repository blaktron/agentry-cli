// Package remote is the client of a registry's knowledge routes (plan
// knowledge.md §4.3, §4.4; agentry-cli#319): wrap and unwrap a pack's data
// key, create a pack, upload and download its sealed tree, list and delete,
// for the account a session token or a personal access token with
// knowledge:read and knowledge:write speaks for. It implements
// store.Registry for the CLI and the desktop alike, and, through Shared,
// store.Shared for an organization's shared packs (Knowledge M8,
// agentry-cli#380; the core's /orgs/{org}/knowledge/).
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/blaktron/agentry/pkg/apiclient"
	"github.com/blaktron/agentry/pkg/packseal"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
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

// GetTree implements store.Registry: GET /knowledge/packs/{id}/tree. The
// body is read no further than a sealed pack at the size cap can be, so a
// registry cannot make the client hold more than that.
func (c *Client) GetTree(ctx context.Context, id string) ([]byte, string, error) {
	pid, err := packID(id)
	if err != nil {
		return nil, "", err
	}
	res, err := c.api.DownloadKnowledgeTree(ctx, pid)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = res.Body.Close() }()
	limit := packseal.MaxSealedSize(knowledge.MaxBytes)
	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if res.StatusCode != http.StatusOK {
		return nil, "", c.problem("download the knowledge tree", res, nil, body)
	}
	if int64(len(body)) > limit {
		return nil, "", fmt.Errorf("download the knowledge tree at %s: the registry sent more than a sealed %d MB pack can be", c.url, knowledge.MaxBytes>>20)
	}
	return body, res.Header.Get("X-Agentry-Knowledge-Digest"), nil
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
		Shared: p.Shared,
	}
	if p.Org != nil {
		r.Org = *p.Org
	}
	if p.Exportable != nil {
		r.Exportable = *p.Exportable
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
	case slug == "knowledge-not-exportable":
		sentinel = store.ErrNotExportable
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

// maxExport bounds an export's body: the canonical tar of a pack at the
// cap, its files and a tar header (at most 1 KiB) for each of at most
// knowledge.MaxFiles of them, with room to spare.
const maxExport = knowledge.MaxBytes + 4<<20

// OrgClient is one organization's shared packs at a registry, as the
// account a Client speaks for (Knowledge M8): the registry answers it only
// while that account is an active member, and 404 to anyone else.
type OrgClient struct {
	c   *Client
	org string
}

var _ store.Shared = (*OrgClient)(nil)

// Shared is the client of org's shared packs, by the organization's handle.
func (c *Client) Shared(org string) *OrgClient { return &OrgClient{c: c, org: org} }

// URL implements store.Shared.
func (o *OrgClient) URL() string { return o.c.url }

// Org implements store.Shared.
func (o *OrgClient) Org() string { return o.org }

// Unwrap implements store.Shared: POST /orgs/{org}/knowledge/keys/unwrap,
// which the registry answers to an active member alone.
func (o *OrgClient) Unwrap(ctx context.Context, wk store.WrappedKey) ([]byte, error) {
	res, err := o.c.api.UnwrapOrgKnowledgeKeyWithResponse(ctx, o.org, toAPIKey(wk))
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, o.c.problem("unwrap "+o.org+"'s knowledge key", res.HTTPResponse, res.ApplicationproblemJSONDefault, res.Body)
	}
	return res.JSON200.Key, nil
}

// List implements store.Shared: GET /orgs/{org}/knowledge/packs.
func (o *OrgClient) List(ctx context.Context) ([]store.Remote, error) {
	res, err := o.c.api.ListOrgKnowledgePacksWithResponse(ctx, o.org)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, o.c.problem("list "+o.org+"'s knowledge packs", res.HTTPResponse, res.ApplicationproblemJSONDefault, res.Body)
	}
	out := make([]store.Remote, 0, len(*res.JSON200))
	for _, p := range *res.JSON200 {
		out = append(out, fromAPIPack(p))
	}
	return out, nil
}

// GetTree implements store.Shared: GET /orgs/{org}/knowledge/packs/{id}/tree,
// read no further than a sealed pack at the size cap can be.
func (o *OrgClient) GetTree(ctx context.Context, id string) ([]byte, string, error) {
	pid, err := packID(id)
	if err != nil {
		return nil, "", err
	}
	res, err := o.c.api.DownloadOrgKnowledgeTree(ctx, o.org, pid)
	if err != nil {
		return nil, "", err
	}
	return o.c.bounded("download "+o.org+"'s knowledge tree", res, packseal.MaxSealedSize(knowledge.MaxBytes))
}

// Export implements store.Shared: GET /orgs/{org}/knowledge/packs/{id}/export,
// the tree the registry decrypts: an admin's, or a member's of a pack
// marked exportable (store.ErrNotExportable otherwise).
func (o *OrgClient) Export(ctx context.Context, id string) ([]byte, string, error) {
	pid, err := packID(id)
	if err != nil {
		return nil, "", err
	}
	res, err := o.c.api.ExportOrgKnowledgePack(ctx, o.org, pid)
	if err != nil {
		return nil, "", err
	}
	return o.c.bounded("export "+o.org+"'s knowledge pack", res, maxExport)
}

// bounded reads a 200's body no further than limit, with its digest
// header, or turns a refusal into the store's error.
func (c *Client) bounded(op string, res *http.Response, limit int64) ([]byte, string, error) {
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if res.StatusCode != http.StatusOK {
		return nil, "", c.problem(op, res, nil, body)
	}
	if int64(len(body)) > limit {
		return nil, "", fmt.Errorf("%s at %s: the registry sent more than a %d MB pack can be", op, c.url, knowledge.MaxBytes>>20)
	}
	return body, res.Header.Get("X-Agentry-Knowledge-Digest"), nil
}
