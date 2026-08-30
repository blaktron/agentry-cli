// `agentry validate --rules-update` (§11): fetch the registry's current S3/S4
// rule bundle and cache it, so local validation can track rules that move
// faster than CLI releases.
//
// Trust model, stated plainly because it is narrower than §11's one-word
// "(signed)": the bundle is pinned by DIGEST, not by a signature. The
// registry advertises the digest recorded on its ruleset_releases row, the
// CLI recomputes that digest over what it received, and a mismatch is a hard
// refusal. That defeats a corrupted or truncated transfer and a proxy that
// rewrites bytes; it does NOT defeat a registry that lies about both the
// bundle and its digest, which is what a signature would buy. There is no
// bundle-level signature to verify yet — see DEVIATIONS DEV-024, which
// records the deferral rather than inventing a scheme the server cannot
// honour.
//
// Cache layout: ~/.config/agentry/rules/<semver>/{bundle.yaml,*.yaml},
// one directory per version, alongside the config (see installed.go for why
// everything the CLI keeps lives under one root).
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/blaktron/agentry/pkg/rules"
)

// maxBundleBytes bounds what the CLI will read from the bundle endpoint.
const maxBundleBytes int64 = 8 << 20

// rulesCacheDir is ~/.config/agentry/rules.
func rulesCacheDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "rules"), nil
}

// fetchRulesBundle downloads, verifies, and caches the registry's bundle.
// It returns the semver it stored under.
func fetchRulesBundle(ctx context.Context, w io.Writer, regURL string) (string, error) {
	url := strings.TrimRight(regURL, "/") + "/api/v1/ruleset-releases/latest/bundle"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch rule bundle: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return "", apiProblem("fetch rule bundle", res.StatusCode, nil, body)
	}

	data, err := io.ReadAll(io.LimitReader(res.Body, maxBundleBytes+1))
	if err != nil {
		return "", fmt.Errorf("read rule bundle: %w", err)
	}
	if int64(len(data)) > maxBundleBytes {
		return "", fmt.Errorf("rule bundle exceeds the %d-byte client cap", maxBundleBytes)
	}

	semver := strings.TrimSpace(res.Header.Get("X-Agentry-Bundle-Semver"))
	advertised := normalizeBundleDigest(res.Header.Get("X-Agentry-Bundle-Digest"))
	if semver == "" || advertised == "" {
		return "", fmt.Errorf("rule bundle from %s carries no semver/digest headers — refusing to trust it", regURL)
	}
	if !validCacheSemver(semver) {
		return "", fmt.Errorf("rule bundle semver %q is not MAJOR.MINOR.PATCH", semver)
	}

	// Unpack into a staging directory, then LOAD it: the digest is computed
	// by the same code that computes it on the server, over the same files,
	// so a match means the bundle this CLI will actually scan with is the one
	// the registry advertised — not merely that some bytes hashed correctly.
	staging, err := os.MkdirTemp("", "agentry-rules-*")
	if err != nil {
		return "", fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if _, err := rules.UntarBundle(data, func(name string, content []byte) error {
		return os.WriteFile(filepath.Join(staging, name), content, 0o600)
	}); err != nil {
		return "", err
	}
	bundle, err := rules.LoadBundle(os.DirFS(staging), ".")
	if err != nil {
		return "", fmt.Errorf("the fetched rule bundle does not load: %w", err)
	}
	got := normalizeBundleDigest(bundle.Digest())
	if got != advertised {
		return "", fmt.Errorf("rule bundle digest mismatch: %s advertises %s, the bytes are %s — refusing to cache it",
			regURL, advertised, got)
	}
	if bundle.Version() != semver {
		return "", fmt.Errorf("rule bundle version mismatch: header says %s, bundle.yaml says %s",
			semver, bundle.Version())
	}

	// Install atomically-ish: write to a sibling and rename over, so an
	// interrupted update never leaves a half-written version directory that
	// the loader would then pick as "newest".
	root, err := rulesCacheDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", root, err)
	}
	final := filepath.Join(root, semver)
	tmp := final + ".incoming"
	_ = os.RemoveAll(tmp)
	if err := copyTree(staging, tmp); err != nil {
		return "", err
	}
	if err := os.RemoveAll(final); err != nil {
		return "", fmt.Errorf("clear %s: %w", final, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", fmt.Errorf("install rule bundle at %s: %w", final, err)
	}

	_, _ = fmt.Fprintf(w, "rules: cached bundle %s (%s) from %s at %s\n", semver, got, regURL, final)
	_, _ = fmt.Fprintln(w, "rules: pinned by digest; bundle-level signatures are not available yet (DEV-024)")
	return semver, nil
}

// newestCachedBundle returns the highest-semver cached bundle, or "" when the
// cache is empty. A directory that no longer loads is skipped rather than
// fatal: the embedded bundle is always a valid fallback, and one corrupt
// cache entry should not make `validate` unusable.
func newestCachedBundle() (*rules.Bundle, string) {
	root, err := rulesCacheDir()
	if err != nil {
		return nil, ""
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, ""
	}
	best := ""
	for _, e := range entries {
		if !e.IsDir() || !validCacheSemver(e.Name()) {
			continue
		}
		if best == "" {
			best = e.Name()
			continue
		}
		if c, ok := compareSemver(e.Name(), best); ok && c > 0 {
			best = e.Name()
		}
	}
	if best == "" {
		return nil, ""
	}
	b, err := rules.LoadBundle(os.DirFS(filepath.Join(root, best)), ".")
	if err != nil {
		return nil, ""
	}
	return b, best
}

// validCacheSemver gates directory names used as cache keys: only
// MAJOR.MINOR.PATCH, so a hostile semver header cannot name a path.
func validCacheSemver(s string) bool {
	if strings.ContainsAny(s, `/\`) || strings.Contains(s, "..") {
		return false
	}
	_, ok := parseSemver(s)
	return ok && !strings.HasPrefix(s, "v")
}

// normalizeBundleDigest renders a digest as "sha256:<hex>", accepting either
// form on input (pkg/rules computes bare hex; the API documents the prefix).
func normalizeBundleDigest(d string) string {
	d = strings.TrimSpace(strings.Trim(strings.TrimSpace(d), `"`))
	if d == "" {
		return ""
	}
	return "sha256:" + strings.TrimPrefix(d, "sha256:")
}
