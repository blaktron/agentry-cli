// API client plumbing: bearer-authenticated apiclient construction, RFC 9457
// problem rendering, and the §9.4 download/verify helpers shared by
// install and verify.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/apiclient"
	"github.com/blaktron/agentry/pkg/manifest"
)

// Client-side safety caps for release archives. The registry enforces the
// real quotas (§19: 100 MB repo hard cap, 25 MB single object); these only
// bound what the CLI is willing to buffer and unpack.
const (
	maxArchiveBytes      int64 = 512 << 20
	extractMaxFileBytes  int64 = 25 << 20
	extractMaxTotalBytes int64 = 200 << 20
)

// httpTimeout bounds every request the CLI makes. Archive downloads are the
// long pole (a 100 MB release over a slow link), so the budget is generous —
// but it is finite: http.DefaultClient has no timeout at all, and a hung
// registry would otherwise wedge a CI job until the job's own timeout fires,
// with no error message explaining why.
const httpTimeout = 5 * time.Minute

// httpClient is the one client every CLI request goes through. Per-request
// deadlines still come from the command's context; this is the backstop.
var httpClient = &http.Client{Timeout: httpTimeout}

// newAPIClient builds a generated client rooted at {registry}/api/v1,
// attaching "Authorization: Bearer <token>" when a token is known.
func newAPIClient(regURL, token string) (*apiclient.ClientWithResponses, error) {
	return newAPIClientWithHTTP(regURL, token, httpClient)
}

func newAPIClientWithHTTP(regURL, token string, hc *http.Client) (*apiclient.ClientWithResponses, error) {
	opts := []apiclient.ClientOption{apiclient.WithHTTPClient(hc)}
	if token != "" {
		opts = append(opts, apiclient.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+token)
			return nil
		}))
	}
	c, err := apiclient.NewClientWithResponses(regURL+"/api/v1", opts...)
	if err != nil {
		return nil, fmt.Errorf("create API client for %s: %w", regURL, err)
	}
	return c, nil
}

// newAPIClientChecked is newAPIClient plus the §11 min-version gate: the
// first API-using command in an invocation asks the registry what CLI version
// it still supports, and reports (or refuses) before doing any work. Every
// command that talks to a registry goes through here, so no command can
// forget the check.
func newAPIClientChecked(cmd *cobra.Command, regURL, token string) (*apiclient.ClientWithResponses, error) {
	c, err := newAPIClient(regURL, token)
	if err != nil {
		return nil, err
	}
	if err := minVersionGate(cmd.Context(), cmd.ErrOrStderr(), regURL, token); err != nil {
		return nil, err
	}
	return c, nil
}

// apiProblem renders a failed API call as an error, preferring the RFC 9457
// problem document when the server sent one.
func apiProblem(op string, status int, p *apiclient.Problem, body []byte) error {
	if p == nil {
		// A status the generated client types on its own (the spec's
		// explicit 402, say) is not the `default` problem it parsed; the
		// body is a problem all the same.
		var parsed apiclient.Problem
		if json.Unmarshal(body, &parsed) == nil && parsed.Title != "" && parsed.Status == status {
			p = &parsed
		}
	}
	if p != nil {
		msg := p.Title
		if p.Detail != nil && *p.Detail != "" {
			msg += ": " + *p.Detail
		}
		if p.Errors != nil {
			for _, fe := range *p.Errors {
				msg += fmt.Sprintf("; %s: %s", fe.Field, fe.Message)
			}
		}
		if acct := refusedAccount(status, body); acct != "" {
			msg += "; the account is " + acct
		}
		return fmt.Errorf("%s: %s (HTTP %d)", op, msg, status)
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return fmt.Errorf("%s: HTTP %d: %s", op, status, msg)
}

// refusedAccount names the account a payment refusal is about. A member
// publishes to their own namespace but spends from their organization's
// pool (plan organization-subscriptions.md D22), so the account whose
// balance is short is often not the one that signed the request, and the
// refusal's prose alone leaves that to be guessed.
//
// Only a 402 is read for it: `account` is the billing extension's
// extension member, and 402 is the one status it refuses with, so no other
// problem body can put a word of its own choosing into this sentence.
// The value is still the server's, so it is bounded and stripped of
// control characters before it reaches a terminal — the same care the
// non-problem branch below takes with a body it did not write.
func refusedAccount(status int, body []byte) string {
	if status != http.StatusPaymentRequired {
		return ""
	}
	var members struct {
		Account string `json:"account"`
	}
	if json.Unmarshal(body, &members) != nil {
		return ""
	}
	return safeTerminalText(members.Account, 64)
}

// safeTerminalText bounds a server-supplied string and drops the control
// characters an escape sequence needs, so nothing a registry answers can
// rewrite the terminal it is printed to.
func safeTerminalText(s string, max int) string {
	clean := strings.Map(func(r rune) rune {
		if r == '\t' || (unicode.IsControl(r) && r != ' ') {
			return -1
		}
		return r
	}, s)
	if len([]rune(clean)) > max {
		clean = string([]rune(clean)[:max]) + "…"
	}
	return clean
}

// sha256Digest computes the "sha256:<hex>" digest of b (D-15 identity form).
func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// orNone renders an empty string as "none" for human output.
func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// sameHost reports whether two URLs share a host, so the bearer token is
// only ever attached to the registry itself, never to a CDN (§9.4).
func sameHost(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && ua.Host == ub.Host
}

// downloadArchive fetches the release archive named by a resolve result
// (§9.4 step 2). Relative archive URLs (self-host) resolve against the
// registry base URL.
func downloadArchive(ctx context.Context, regURL, token string, res *apiclient.ResolveResult) ([]byte, error) {
	archiveURL := res.ArchiveUrl
	if strings.HasPrefix(archiveURL, "/") {
		archiveURL = regURL + archiveURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", err)
	}
	if token != "" && sameHost(archiveURL, regURL) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", archiveURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, apiProblem("download archive", resp.StatusCode, nil, body)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxArchiveBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read archive body: %w", err)
	}
	if int64(len(data)) > maxArchiveBytes {
		return nil, fmt.Errorf("archive exceeds the %d-byte client cap", maxArchiveBytes)
	}
	return data, nil
}

// manifestFromResolve decodes the canonical manifest JSON carried by a
// resolve result into the typed manifest.
func manifestFromResolve(res *apiclient.ResolveResult) (*manifest.Manifest, error) {
	raw, err := json.Marshal(res.Manifest)
	if err != nil {
		return nil, fmt.Errorf("re-encode resolved manifest: %w", err)
	}
	var m manifest.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode resolved manifest: %w", err)
	}
	return &m, nil
}

// resolveRef calls GET /resolve for a ns/name[@range] reference.
// installIntent adds the X-Agentry-Install hint so the registry counts an
// install without a download round-trip through the API (§9.4 step 4).
func resolveRef(ctx context.Context, client *apiclient.ClientWithResponses, ref string, installIntent bool) (*apiclient.ResolveResult, error) {
	var editors []apiclient.RequestEditorFn
	if installIntent {
		editors = append(editors, func(_ context.Context, req *http.Request) error {
			req.Header.Set("X-Agentry-Install", "1")
			return nil
		})
	}
	resp, err := client.ResolveRefWithResponse(ctx, &apiclient.ResolveRefParams{Ref: ref}, editors...)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", ref, err)
	}
	if resp.JSON200 == nil {
		return nil, apiProblem("resolve "+ref, resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.Body)
	}
	return resp.JSON200, nil
}
