// Package osvdb keeps this machine's copy of OSV's vulnerability databases,
// which the Coding agent's dependency scan reads offline. OSV publishes one
// archive per ecosystem, refreshed daily, at its public bucket. Ensure
// downloads each archive the runner wants into the runner home, in the
// layout OSV-Scanner reads (osv-scalibr/<ecosystem>/all.zip), when it was
// last fetched over a day ago, replacing a file only once a new one is
// whole. The download is the CLI's own: no container and no project file
// is involved, and the scan that reads the copy runs with no access to OSV
// at all. A refresh that fails keeps the copy it has; with none, the scan
// says there is no database rather than finding nothing.
package osvdb

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Host is where OSV's archives are downloaded from.
const Host = "osv-vulnerabilities.storage.googleapis.com"

// Ecosystems are the archives fetched: the ecosystems of the lockfiles the
// Coding agent's toolchains write.
var Ecosystems = []string{"Go", "npm", "PyPI", "crates.io", "Maven", "RubyGems", "Packagist", "NuGet"}

// The ages of a copy: asked again after a day, a failed refresh not tried
// again for an hour, and older than a week named stale.
const (
	RefreshAfter = 24 * time.Hour
	RetryAfter   = time.Hour
	StaleAfter   = 7 * 24 * time.Hour
)

// MaxArchiveBytes bounds one archive: the largest is well under it.
const MaxArchiveBytes = 2 << 30

// DB is the copy a scan mounts.
type DB struct {
	// Dir holds osv-scalibr/<ecosystem>/all.zip; empty when this machine
	// has no archive at all.
	Dir string
	// Fetched is when each archive held was last downloaded whole.
	Fetched map[string]time.Time
	// Problem says why there is no copy, or that the last refresh failed.
	Problem string
}

// Held says whether this machine has an archive to scan against.
func (db DB) Held() bool { return db.Dir != "" }

// Stale names the archives older than StaleAfter.
func (db DB) Stale() []string {
	var out []string
	for _, e := range Ecosystems {
		if t, ok := db.Fetched[e]; ok && now().Sub(t) > StaleAfter {
			out = append(out, e)
		}
	}
	return out
}

// The words Problem holds.
const (
	ProblemNone    = "no OSV database could be downloaded"
	ProblemRefresh = "the last refresh of the OSV databases failed"
)

// state is what the runner home records of the archives it holds.
type state struct {
	Fetched   map[string]time.Time `json:"fetched"`
	Attempted time.Time            `json:"attempted,omitempty"`
}

const stateFile = "current.json"

// now, baseURL and httpClient are variables for the tests.
var (
	now     = time.Now
	baseURL = "https://" + Host
	// httpClient follows no redirect: the archives come from Host alone.
	httpClient = &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	refreshTimeout = 15 * time.Minute
)

// Dir is the runner home's folder of the databases.
func Dir(home string) string { return filepath.Join(home, "osv") }

// Ensure answers the copy for a scan, refreshing it first when it is due.
// It never fails: a refresh's error is logged and named in Problem, and the
// archives this machine has are answered.
func Ensure(ctx context.Context, home string, logw io.Writer) DB {
	if home == "" {
		return DB{Problem: ProblemNone}
	}
	dir := Dir(home)
	st := loadState(dir)
	problem := ""
	due := len(st.Fetched) < len(Ecosystems) || oldest(st.Fetched) >= RefreshAfter
	if due && now().Sub(st.Attempted) >= RetryAfter {
		rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
		err := refresh(rctx, dir, &st, logw)
		cancel()
		if err != nil {
			_, _ = fmt.Fprintf(logw, "osv: could not refresh the databases from %s: %v\n", Host, err)
			problem = ProblemRefresh
			st.Attempted = now().UTC()
		} else {
			st.Attempted = time.Time{}
		}
		_ = saveState(dir, st)
	} else if !st.Attempted.IsZero() {
		problem = ProblemRefresh
	}
	held := map[string]time.Time{}
	for e, t := range st.Fetched {
		if whole(archive(dir, e)) == nil {
			held[e] = t
		}
	}
	if len(held) == 0 {
		return DB{Problem: ProblemNone}
	}
	return DB{Dir: dir, Fetched: held, Problem: problem}
}

// oldest is how long ago the oldest archive was fetched.
func oldest(fetched map[string]time.Time) time.Duration {
	var d time.Duration
	for _, t := range fetched {
		if age := now().Sub(t); age > d {
			d = age
		}
	}
	return d
}

func archive(dir, ecosystem string) string {
	return filepath.Join(dir, "osv-scalibr", ecosystem, "all.zip")
}

// refresh downloads every archive that is due, each into a temporary file
// beside it, checked to be a whole zip before it replaces the old one. An
// archive that fails is left as it was and the rest go on; the error names
// the first.
func refresh(ctx context.Context, dir string, st *state, logw io.Writer) error {
	if st.Fetched == nil {
		st.Fetched = map[string]time.Time{}
	}
	var first error
	for _, e := range Ecosystems {
		if t, ok := st.Fetched[e]; ok && now().Sub(t) < RefreshAfter && whole(archive(dir, e)) == nil {
			continue
		}
		if err := fetch(ctx, dir, e); err != nil {
			if first == nil {
				first = fmt.Errorf("%s: %w", e, err)
			}
			continue
		}
		st.Fetched[e] = now().UTC()
		_, _ = fmt.Fprintf(logw, "osv: downloaded the %s database\n", e)
	}
	return first
}

// fetch downloads one ecosystem's archive.
func fetch(ctx context.Context, dir, ecosystem string) error {
	final := archive(dir, ecosystem)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil { //nolint:gosec // read by the scan, which runs as the person
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/"+ecosystem+"/all.zip", nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", Host, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(final), ".all-*.zip")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, MaxArchiveBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return err
	case n > MaxArchiveBytes:
		return fmt.Errorf("the archive is larger than %d MiB", MaxArchiveBytes>>20)
	}
	if err := whole(tmp.Name()); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { //nolint:gosec // read by the scan, which runs as the person
		return err
	}
	return os.Rename(tmp.Name(), final)
}

// whole refuses a file that is not a readable zip archive with at least
// one entry.
func whole(path string) error {
	r, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	if len(r.File) == 0 {
		return errors.New("the archive is empty")
	}
	return nil
}

func loadState(dir string) state {
	var st state
	b, err := os.ReadFile(filepath.Join(dir, stateFile)) //nolint:gosec // under the runner home
	if err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func saveState(dir string, st state) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // read by the scan, which runs as the person
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+stateFile)
	if err := os.WriteFile(tmp, b, 0o644); err != nil { //nolint:gosec // dates, no secret
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, stateFile))
}
