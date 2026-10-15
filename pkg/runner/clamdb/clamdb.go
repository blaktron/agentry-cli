// Package clamdb keeps this machine's copy of the ClamAV signature bundle the
// decode step checks every input against. agentry-dockerimages' clamav-db
// workflow publishes the bundle daily to GHCR as an OCI artifact of three CVDs,
// signed keyless as that workflow. Ensure refreshes the copy under the runner
// home when it is over a day since it last asked, verifies the signature before
// a byte of it is used, refuses a bundle older than the one it holds, and
// answers the directory a decode mounts read-only. A refresh that fails never
// fails a run: the copy it has is used, and past 7 days old its receipt says
// the signatures are stale; with no copy at all, nothing is checked, and the
// receipt says that.
package clamdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// The plan's two ages, and how often a refresh that failed is tried
// again while a bundle is held.
const (
	// RefreshAfter: the registry is asked again once a day has passed.
	RefreshAfter = 24 * time.Hour
	// RetryAfter: a refresh that failed (the registry down, a bundle
	// refused) is not tried again for an hour, so every run does not pay
	// for the same failure.
	RetryAfter = time.Hour
	// StaleAfter: signatures older than this are named stale in the receipt.
	StaleAfter = 7 * 24 * time.Hour
)

// The three databases the bundle holds, each a CVD.
var databases = []string{"main.cvd", "daily.cvd", "bytecode.cvd"}

// DB is the bundle a decode mounts: where it is and what it is.
type DB struct {
	// Dir holds the three CVDs; empty when this machine has none.
	Dir string
	// Ref is the tag it was refreshed from, Digest the signed manifest's.
	Ref    string
	Digest string
	// Daily dates the signatures: daily.cvd changes every day, main.cvd a
	// few times a year.
	Daily, Main Version
	// Checked is when the registry last answered.
	Checked time.Time
	// Stale says the signatures were older than StaleAfter when Ensure
	// answered.
	Stale bool
	// Problem says why there is no bundle, or that the last refresh
	// failed; the error itself went to the log.
	Problem string
}

// Held says whether this machine has a bundle to mount.
func (db DB) Held() bool { return db.Dir != "" }

// Describe names the bundle for a person: the signatures' date, the two
// versions and where they came from.
func (db DB) Describe() string {
	digest := strings.TrimPrefix(db.Digest, "sha256:")
	if len(digest) > 12 {
		digest = digest[:12]
	}
	return fmt.Sprintf("the signatures of %s (daily %d, main %d; %s@sha256:%s)",
		db.Daily.Built.UTC().Format("2006-01-02 15:04 UTC"), db.Daily.Number, db.Main.Number, db.Ref, digest)
}

// The words Problem holds, the host's own, for the receipt.
const (
	ProblemNone    = "no ClamAV signatures could be fetched"
	ProblemRefresh = "the last refresh of the signatures failed"
)

// state is what the runner home records of the bundle it holds.
type state struct {
	Ref     string    `json:"ref"`
	Digest  string    `json:"digest"`
	Dir     string    `json:"dir"`
	Checked time.Time `json:"checked"`
	// Attempted is when a refresh last failed, for RetryAfter.
	Attempted time.Time `json:"attempted,omitempty"`
	Daily     Version   `json:"daily"`
	Main      Version   `json:"main"`
	// Layers is each database's digest, so a refresh downloads only what
	// changed.
	Layers map[string]string `json:"layers"`
}

const stateFile = "current.json"

// now and scheme are variables for the tests: the clock moved forward, and
// a registry served over plain HTTP by httptest.
var (
	now    = time.Now
	scheme = "https"
	// refreshTimeout bounds one refresh: a paid hosted run waits on it.
	refreshTimeout = 5 * time.Minute
)

// Ensure answers the bundle for a decode, refreshing it first when the
// registry was last asked over RefreshAfter ago (or never, or about
// another ref). It never fails: a refresh's error is logged and named in
// Problem, and the copy this machine has is answered.
func Ensure(ctx context.Context, home, ref string, logw io.Writer) DB {
	if ref == "" {
		ref = policy.DefaultClamAVSignatures
	}
	if home == "" {
		return DB{Ref: ref, Problem: ProblemNone}
	}
	dir := filepath.Join(home, "clamav")
	cur := loadState(dir)
	problem := ""
	due := cur == nil || cur.Ref != ref || (now().Sub(cur.Checked) >= RefreshAfter && now().Sub(cur.Attempted) >= RetryAfter)
	if due {
		rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
		next, err := refresh(rctx, dir, ref, cur, logw)
		cancel()
		if err == nil {
			cur = next
		} else {
			_, _ = fmt.Fprintf(logw, "clamav: could not refresh the signatures from %s: %v\n", ref, err)
			problem = ProblemRefresh
			if cur != nil {
				cur.Attempted = now().UTC()
				_ = saveState(dir, cur)
			}
		}
	} else if cur.Ref == ref && !cur.Attempted.IsZero() && cur.Attempted.After(cur.Checked) {
		problem = ProblemRefresh
	}
	if cur == nil {
		return DB{Ref: ref, Problem: ProblemNone}
	}
	db := DB{
		Dir: filepath.Join(dir, "db", cur.Dir), Ref: cur.Ref, Digest: cur.Digest,
		Daily: cur.Daily, Main: cur.Main, Checked: cur.Checked, Problem: problem,
	}
	db.Stale = now().Sub(db.Daily.Built) > StaleAfter
	return db
}

// loadState reads the recorded bundle, or nil when there is none whole:
// no record, one that does not parse, or a database missing.
func loadState(dir string) *state {
	b, err := os.ReadFile(filepath.Join(dir, stateFile)) //nolint:gosec // under the runner home
	if err != nil {
		return nil
	}
	var st state
	if json.Unmarshal(b, &st) != nil || !digestRE.MatchString(st.Digest) || st.Dir != bundleDirName(st.Digest) || whole(filepath.Join(dir, "db", st.Dir)) != nil {
		return nil
	}
	return &st
}

func bundleDirName(digest string) string {
	return strings.TrimPrefix(digest, "sha256:")[:16]
}

// refresh asks the registry for the bundle the ref names. The same digest
// as the one held only moves the clock; a new one is verified, downloaded,
// read, held to be no older, and made current.
func refresh(ctx context.Context, dir, ref string, cur *state, logw io.Writer) (*state, error) {
	reg, tag, err := newRegistry(ref, httpClient, scheme)
	if err != nil {
		return nil, err
	}
	body, digest, err := reg.manifest(ctx, tag, ociManifest)
	if err != nil {
		return nil, err
	}
	if cur != nil && cur.Ref == ref && cur.Digest == digest {
		cur.Checked = now().UTC()
		return cur, saveState(dir, cur)
	}
	layers, err := bundleLayers(body)
	if err != nil {
		return nil, fmt.Errorf("%s@%s: %w", ref, digest, err)
	}
	if err := verifyAttached(ctx, reg, dir, digest); err != nil {
		return nil, fmt.Errorf("%s@%s: %w", ref, digest, err)
	}
	dbDir := filepath.Join(dir, "db")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(dbDir, ".tmp-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := os.Chmod(tmp, 0o755); err != nil { //nolint:gosec // the decode container reads it as the invoking user, relabelled
		return nil, err
	}
	var size int64
	for _, name := range databases {
		if cur == nil || cur.Layers[name] != layers[name].Digest || linkOrCopy(filepath.Join(dbDir, cur.Dir, name), filepath.Join(tmp, name)) != nil {
			size += layers[name].Size
		}
	}
	if size > 0 {
		_, _ = fmt.Fprintf(logw, "clamav: fetching the signatures %s@%s (%d MB)\n", ref, digest, (size+(1<<20)-1)>>20)
	}
	for _, name := range databases {
		dst := filepath.Join(tmp, name)
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		if err := reg.blobTo(ctx, layers[name].Digest, layers[name].Size, dst); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	next := &state{Ref: ref, Digest: digest, Dir: bundleDirName(digest), Checked: now().UTC(), Layers: map[string]string{}}
	for _, name := range databases {
		next.Layers[name] = layers[name].Digest
		v, err := readCVDHeader(filepath.Join(tmp, name))
		if err != nil {
			return nil, fmt.Errorf("%s@%s: %w", ref, digest, err)
		}
		switch name {
		case "daily.cvd":
			next.Daily = v
		case "main.cvd":
			next.Main = v
		}
	}
	// A bundle older than the one held is refused: anything that can move
	// the tag could otherwise move it back to an old, still signed, bundle
	// and hold every machine to signatures that miss what came since.
	if cur != nil && (next.Daily.Number < cur.Daily.Number || next.Main.Number < cur.Main.Number) {
		return nil, fmt.Errorf("%s@%s holds daily %d and main %d, older than the daily %d and main %d this machine has", ref, digest, next.Daily.Number, next.Main.Number, cur.Daily.Number, cur.Main.Number)
	}
	final := filepath.Join(dbDir, next.Dir)
	if err := os.Rename(tmp, final); err != nil {
		// Another process made it current first; a bundle directory is
		// renamed into place only whole, so it is this same bundle.
		if whole(final) != nil {
			return nil, err
		}
	}
	if err := saveState(dir, next); err != nil {
		return nil, err
	}
	keep := map[string]bool{next.Dir: true}
	if cur != nil {
		keep[cur.Dir] = true
	}
	prune(dbDir, keep)
	return next, nil
}

// bundleLayers holds a manifest to the bundle's shape: the artifact type,
// and exactly the three databases, each a CVD layer of a sane size, named
// once by its title. It answers each database's descriptor.
func bundleLayers(body []byte) (map[string]descriptor, error) {
	var m manifestDoc
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("the manifest: %w", err)
	}
	if m.MediaType != ociManifest || m.ArtifactType != bundleType {
		return nil, fmt.Errorf("not a ClamAV signature bundle (media type %q, artifact type %q)", m.MediaType, m.ArtifactType)
	}
	if len(m.Layers) != len(databases) {
		return nil, fmt.Errorf("the bundle has %d layers, not the %d databases", len(m.Layers), len(databases))
	}
	out := map[string]descriptor{}
	for _, l := range m.Layers {
		name := l.Annotations[titleKey]
		if l.MediaType != cvdType || !digestRE.MatchString(l.Digest) || l.Size <= cvdHeaderSize || l.Size > maxLayerBytes {
			return nil, fmt.Errorf("the layer %q is not a CVD of a sane size", name)
		}
		known := false
		for _, d := range databases {
			known = known || d == name
		}
		if _, twice := out[name]; !known || twice {
			return nil, fmt.Errorf("the layer %q is not one of %s, once", name, strings.Join(databases, ", "))
		}
		out[name] = l
	}
	return out, nil
}

// verifyAttached fetches the signatures attached to the digest and answers
// nil once one verifies as the clamav-db workflow's over it. Cosign attaches
// a signature as a referrer; GHCR has no referrers API, so the referrers
// are read from the tag schema (sha256-<hex>) when the API does not answer.
func verifyAttached(ctx context.Context, reg *registry, dir, digest string) error {
	idx, _, err := reg.get(ctx, "/referrers/"+digest, ociIndex, maxMetaBytes)
	var doc manifestDoc
	if err != nil || json.Unmarshal(idx, &doc) != nil || len(doc.Manifests) == 0 {
		body, _, terr := reg.manifest(ctx, "sha256-"+strings.TrimPrefix(digest, "sha256:"), ociIndex)
		if terr != nil {
			if errors.Is(terr, errNotFound) {
				return errUnsigned
			}
			return fmt.Errorf("its signatures: %w", terr)
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return fmt.Errorf("its signatures: %w", err)
		}
	}
	tm, err := trustedRoot(ctx, filepath.Join(dir, "tuf"))
	if err != nil {
		return fmt.Errorf("the Sigstore trust root: %w", err)
	}
	last := errUnsigned
	tried := 0
	// The index's own artifact type is not to be relied on (GHCR's tag
	// schema lists cosign's bundle under the empty config's type), so each
	// referrer is fetched and held to carrying one sigstore bundle.
	for _, d := range doc.Manifests {
		if !digestRE.MatchString(d.Digest) {
			continue
		}
		if tried++; tried > maxReferrers {
			break
		}
		raw, err := signatureBundle(ctx, reg, d.Digest)
		if err == nil {
			err = verifySig(tm, raw, digest)
		}
		if err == nil {
			return nil
		}
		last = err
	}
	return last
}

// errUnsigned is a bundle with no signature attached; maxReferrers bounds
// how many attached referrers are tried, a re-signed bundle having several.
var errUnsigned = errors.New("it carries no signature")

const maxReferrers = 8

// signatureBundle fetches one signature manifest and its one layer, the
// sigstore bundle.
func signatureBundle(ctx context.Context, reg *registry, digest string) ([]byte, error) {
	body, _, err := reg.manifest(ctx, digest, ociManifest)
	if err != nil {
		return nil, err
	}
	var m manifestDoc
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if len(m.Layers) != 1 || m.Layers[0].MediaType != sigstoreType || m.Layers[0].Size > maxMetaBytes || !digestRE.MatchString(m.Layers[0].Digest) {
		return nil, errors.New("a signature manifest without one sigstore bundle")
	}
	return reg.blob(ctx, m.Layers[0].Digest, maxMetaBytes)
}

// whole checks a bundle directory holds the three databases.
func whole(dir string) error {
	for _, name := range databases {
		if fi, err := os.Lstat(filepath.Join(dir, name)); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not whole", dir)
		}
	}
	return nil
}

// saveState writes the record in one rename, so a reader never sees half.
func saveState(dir string, st *state) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+stateFile+"."+hex.EncodeToString(suffix))
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil { //nolint:gosec // no secret: a digest and dates
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, stateFile)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// linkOrCopy reuses a database the previous bundle holds unchanged.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src) //nolint:gosec // a database this package wrote
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // read by the decode container
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// prune removes every bundle directory but the ones kept (the current and
// the one before it, which a decode started before this refresh may still
// read), and temporary directories a refresh left over an hour ago.
func prune(dbDir string, keep map[string]bool) {
	entries, err := os.ReadDir(dbDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if keep[name] || !e.IsDir() {
			continue
		}
		if strings.HasPrefix(name, ".tmp-") {
			if fi, err := e.Info(); err != nil || now().Sub(fi.ModTime()) < time.Hour {
				continue
			}
		}
		_ = os.RemoveAll(filepath.Join(dbDir, name))
	}
}
