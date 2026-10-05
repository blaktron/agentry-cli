package clamdb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// The media types the bundle is held to (agentry-dockerimages clamav-db.yaml).
const (
	ociManifest   = "application/vnd.oci.image.manifest.v1+json"
	ociIndex      = "application/vnd.oci.image.index.v1+json"
	bundleType    = "application/vnd.blaktron.agentry.clamav-db.v1"
	cvdType       = "application/vnd.clamav.cvd"
	sigstoreType  = "application/vnd.dev.sigstore.bundle.v0.3+json"
	titleKey      = "org.opencontainers.image.title"
	maxMetaBytes  = 256 << 10
	maxLayerBytes = 512 << 20
)

// registry is an anonymous OCI distribution client for one repository: it
// pulls a public package with the bearer token the registry hands anyone
// who asks, as `docker pull` does.
type registry struct {
	client *http.Client
	// base is https://<host>/v2/<repository>; scheme is http only in tests.
	base  string
	host  string
	repo  string
	token string
}

func newRegistry(ref string, client *http.Client, scheme string) (*registry, string, error) {
	host, repo, tag, ok := policy.SplitTagRef(ref)
	if !ok {
		return nil, "", fmt.Errorf("the ClamAV signature bundle %q is not <registry>/<repository>:<tag>", ref)
	}
	return &registry{client: client, base: scheme + "://" + host + "/v2/" + repo, host: host, repo: repo}, tag, nil
}

// manifest fetches a manifest by tag or digest and answers its bytes and
// digest. A manifest fetched by digest must hash to it; one fetched by tag
// is named by the hash of what was served, which is what gets verified.
func (r *registry) manifest(ctx context.Context, ref, accept string) ([]byte, string, error) {
	body, hdr, err := r.get(ctx, "/manifests/"+ref, accept, maxMetaBytes)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if strings.HasPrefix(ref, "sha256:") && ref != digest {
		return nil, "", fmt.Errorf("the manifest %s hashes to %s", ref, digest)
	}
	if got := hdr.Get("Docker-Content-Digest"); got != "" && got != digest {
		return nil, "", fmt.Errorf("the registry says the manifest %s is %s, but it hashes to %s", ref, got, digest)
	}
	return body, digest, nil
}

// blob fetches a small blob by digest, checked against it.
func (r *registry) blob(ctx context.Context, digest string, limit int64) ([]byte, error) {
	body, _, err := r.get(ctx, "/blobs/"+digest, "", limit)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	if "sha256:"+hex.EncodeToString(sum[:]) != digest {
		return nil, fmt.Errorf("the blob %s does not hash to its digest", digest)
	}
	return body, nil
}

// blobTo streams a blob by digest into a new file at dst, holding it to
// its size and digest as it streams; a blob that fails either is removed.
func (r *registry) blobTo(ctx context.Context, digest string, size int64, dst string) (err error) {
	resp, err := r.do(ctx, "/blobs/"+digest, "")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // read by the decode container
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, size+1))
	if err != nil {
		return fmt.Errorf("download %s: %w", digest, err)
	}
	if n != size {
		return fmt.Errorf("the blob %s is %d bytes, not the %d its manifest says", digest, n, size)
	}
	if "sha256:"+hex.EncodeToString(h.Sum(nil)) != digest {
		return fmt.Errorf("the blob %s does not hash to its digest", digest)
	}
	return nil
}

func (r *registry) get(ctx context.Context, path, accept string, limit int64) ([]byte, http.Header, error) {
	resp, err := r.do(ctx, path, accept)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(body)) > limit {
		return nil, nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return body, resp.Header, nil
}

// errNotFound is a 404: a tag or digest the repository does not hold.
var errNotFound = errors.New("not found")

// do makes one GET, answering a bearer challenge once with an anonymous
// token for pull.
func (r *registry) do(ctx context.Context, path, accept string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+path, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if r.token != "" {
			req.Header.Set("Authorization", "Bearer "+r.token)
		}
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, err
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return resp, nil
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			challenge := resp.Header.Get("WWW-Authenticate")
			_ = resp.Body.Close()
			if err := r.authorize(ctx, challenge); err != nil {
				return nil, err
			}
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%s%s: %w", r.host+"/"+r.repo, path, errNotFound)
		}
		return nil, fmt.Errorf("%s%s: the registry answered %s", r.host+"/"+r.repo, path, resp.Status)
	}
}

// authorize fetches an anonymous pull token from the realm a bearer
// challenge names.
func (r *registry) authorize(ctx context.Context, challenge string) error {
	params := parseChallenge(challenge)
	realm := params["realm"]
	u, err := url.Parse(realm)
	if err != nil || realm == "" || (u.Scheme != "https" && !strings.HasPrefix(r.base, "http://")) {
		return fmt.Errorf("the registry asked for a token from %q, which is not an https URL", realm)
	}
	q := u.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", "repository:"+r.repo+":pull")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the registry's token service answered %s", resp.Status)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&tok); err != nil {
		return fmt.Errorf("the registry's token: %w", err)
	}
	r.token = tok.Token
	if r.token == "" {
		r.token = tok.AccessToken
	}
	if r.token == "" {
		return errors.New("the registry's token service gave no token")
	}
	return nil
}

// parseChallenge reads `Bearer realm="…",service="…",scope="…"`.
func parseChallenge(h string) map[string]string {
	out := map[string]string{}
	rest, ok := strings.CutPrefix(strings.TrimSpace(h), "Bearer ")
	if !ok {
		return out
	}
	for _, part := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok {
			out[strings.ToLower(k)] = strings.Trim(v, `"`)
		}
	}
	return out
}

// descriptor is an OCI descriptor, as much of one as the bundle needs.
type descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// manifestDoc is an OCI image manifest or index.
type manifestDoc struct {
	MediaType    string       `json:"mediaType"`
	ArtifactType string       `json:"artifactType,omitempty"`
	Config       descriptor   `json:"config"`
	Layers       []descriptor `json:"layers"`
	Manifests    []descriptor `json:"manifests"`
}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// httpClient is the client a refresh uses, bounded like the refresh, so a
// stalled registry fails the refresh rather than holding the run.
var httpClient = &http.Client{Timeout: refreshTimeout}
