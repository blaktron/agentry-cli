package toolsrv

import (
	"archive/zip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// A result in OSV-Scanner's JSON shape: one vulnerable Go module with two
// grouped vulnerabilities, each fixed in a later version.
const depScanVulnerable = `{"results":[{"source":{"path":"/agent/data/app/go.mod","type":"lockfile"},"packages":[
 {"package":{"name":"golang.org/x/text","version":"0.3.0","ecosystem":"Go"},
  "vulnerabilities":[
   {"id":"GO-2020-0015","affected":[{"ranges":[{"events":[{"introduced":"0"},{"fixed":"0.3.3"}]}]}]},
   {"id":"GHSA-5rcv-m4m3-hfh7","affected":[{"ranges":[{"events":[{"introduced":"0"},{"fixed":"0.3.3"}]}]}]},
   {"id":"GO-2021-0113","affected":[{"ranges":[{"events":[{"introduced":"0"},{"fixed":"0.3.7"}]}]}]}],
  "groups":[{"ids":["GO-2020-0015","GHSA-5rcv-m4m3-hfh7"]},{"ids":["GO-2021-0113"]}]},
 {"package":{"name":"golang.org/x/net","version":"0.40.0","ecosystem":"Go"}}]}]}`

const depScanEmpty = `{"results":[],"experimental_config":{"licenses":{"summary":false,"allowlist":null}}}`

// TestDependencyScanReadsOSVScannersAnswer: the vulnerable packages with
// their grouped ids and fixed versions; a clean scan; and every sign of a
// database it could not read refusing an answer, even beside an empty
// result or a found vulnerability.
func TestDependencyScanReadsOSVScannersAnswer(t *testing.T) {
	got, isErr := depScanAnswer(1, []byte(depScanVulnerable), "", "/agent/data/app", "/agent", "")
	if isErr || !strings.Contains(got, "2 known vulnerabilities in 1 package beneath data/app") ||
		!strings.Contains(got, "- golang.org/x/text 0.3.0 (Go, in data/app/go.mod): GO-2020-0015, GO-2021-0113; fixed in 0.3.3, 0.3.7") || strings.Contains(got, "x/net") {
		t.Fatalf("vulnerable: %q (isError %v)", got, isErr)
	}
	if got, isErr := depScanAnswer(0, []byte(`{"results":[]}`), "", "/agent/data/app", "/agent", ""); isErr || !strings.HasPrefix(got, "No known vulnerabilities in the dependencies beneath data/app") {
		t.Fatalf("clean: %q (isError %v)", got, isErr)
	}
	missing := "Scanned /src/npm/package-lock.json file and found 1 package\ncould not load db for npm ecosystem: unable to fetch OSV database: no offline version of the OSV database is available\nError during extraction: (extracting as vulnmatch/osvlocal) unable to fetch OSV database: no offline version of the OSV database is available\n"
	for name, tc := range map[string]struct {
		code   int
		out    string
		stderr string
	}{
		"a missing database beside an empty result":   {127, depScanEmpty, missing},
		"a missing database beside a found one":       {1, depScanVulnerable, missing},
		"a database it cannot write the cache beside": {127, depScanEmpty, "Error during extraction: (extracting as vulnmatch/osvlocal) could not create : mkdir /db/osv-scalibr: read-only file system\n"},
		"exit 127 with no error line":                 {127, depScanEmpty, ""},
		"a corrupt archive":                           {127, depScanEmpty, "could not load db for Go ecosystem: unable to fetch OSV database: could not read OSV database archive: zip: not a valid zip file\n"},
		"vulnerabilities reported and none listed":    {1, depScanEmpty, ""},
		"an answer that is not JSON":                  {0, "Scanning dir", ""},
		"an exit code it does not know":               {2, depScanEmpty, "flag provided but not defined\n"},
	} {
		got, isErr := depScanAnswer(tc.code, []byte(tc.out), tc.stderr, "/agent/data/app", "/agent", "")
		if !isErr || strings.Contains(got, "No known vulnerabilities") || strings.Contains(got, "known vulnerabilities in") {
			t.Errorf("%s: %q (isError %v)", name, got, isErr)
		}
	}
	if got, _ := depScanAnswer(128, []byte(depScanEmpty), "", "/agent/data/app", "/agent", ""); !strings.Contains(got, "no lockfile or manifest") {
		t.Errorf("no sources: %q", got)
	}
}

// TestDependencyScanIsTheCodingAgentsWithADatabase: granted under Any with
// a database folder alone, and with no archive there it refuses before
// running anything.
func TestDependencyScanIsTheCodingAgentsWithADatabase(t *testing.T) {
	spec := anySpec(t)
	if slices.Contains(spec.GrantedTools(), ToolDependencyScan) {
		t.Fatal("granted with no database")
	}
	db := t.TempDir()
	spec.Exec.DependencyScan = &DependencyScan{DBDir: db, Ecosystems: []string{"Go", "npm"}}
	if !slices.Contains(spec.GrantedTools(), ToolDependencyScan) {
		t.Fatal("not granted with a database")
	}
	s := newTestServer(t, spec)
	if s.confine.refused != "" {
		t.Skipf("this machine cannot confine a program: %s", s.confine.refused)
	}
	got, isErr := s.callTool(s.tools[ToolDependencyScan], rawArgs(t, map[string]any{"dir": "data"}))
	if !isErr || !strings.Contains(got, "no OSV database is on this computer yet") {
		t.Fatalf("with no archive: %q", got)
	}
	if got, isErr := s.callTool(s.tools[ToolDependencyScan], rawArgs(t, map[string]any{"dir": "../etc"})); !isErr || !strings.Contains(got, "dir") {
		t.Errorf("a folder outside: %q", got)
	}
	if d := s.tools[ToolDependencyScan].description; !strings.Contains(d, "offline") || !strings.Contains(d, "Go, npm") || !strings.Contains(d, "never reported as no vulnerabilities") {
		t.Errorf("description %q", d)
	}
}

// TestDependencyScanRunsOSVScannerOffline runs the real OSV-Scanner where
// this machine has one, against a database with no archive for the
// project's ecosystem: the answer is an error, never a clean scan.
func TestDependencyScanRunsOSVScannerOffline(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("a program runs confined on Linux alone")
	}
	if _, err := exec.LookPath("osv-scanner"); err != nil {
		t.Skip("no osv-scanner here")
	}
	spec := anySpec(t)
	db := t.TempDir()
	// A database for another ecosystem only: a zip with one entry.
	if err := os.MkdirAll(filepath.Join(db, "osv-scalibr", "crates.io"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeTestZip(filepath.Join(db, "osv-scalibr", "crates.io", "all.zip")); err != nil {
		t.Fatal(err)
	}
	spec.Exec.DependencyScan = &DependencyScan{DBDir: db}
	s := newTestServer(t, spec)
	if s.confine.refused != "" {
		t.Skipf("this machine cannot confine a program: %s", s.confine.refused)
	}
	proj := filepath.Join(spec.WorkspaceDir, "data", "app")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module example.com/app\n\ngo 1.22\n\nrequire golang.org/x/text v0.3.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, isErr := s.callTool(s.tools[ToolDependencyScan], rawArgs(t, map[string]any{"dir": "data/app"}))
	if !isErr || strings.Contains(got, "No known vulnerabilities") {
		t.Fatalf("a scan with no Go database: %q (isError %v)", got, isErr)
	}
}

// writeTestZip writes a zip archive of one small entry.
func writeTestZip(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("RUSTSEC-0000-0000.json")
	if err == nil {
		_, err = w.Write([]byte(`{"id":"RUSTSEC-0000-0000"}`))
	}
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// TestDependencyScanNamesTheDatabasesAge: from the runner's record beside
// them, the oldest download's date, and stale past a week.
func TestDependencyScanNamesTheDatabasesAge(t *testing.T) {
	db := t.TempDir()
	if got := dbAgeOf(db); got != "" {
		t.Errorf("with no record: %q", got)
	}
	recent := time.Now().Add(-2 * time.Hour).UTC()
	write := func(t0 time.Time) {
		rec := `{"fetched":{"Go":"` + t0.Format(time.RFC3339) + `","npm":"` + time.Now().UTC().Format(time.RFC3339) + `"}}`
		if err := os.WriteFile(filepath.Join(db, "current.json"), []byte(rec), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(recent)
	if got := dbAgeOf(db); !strings.Contains(got, "downloaded "+recent.Format("2006-01-02")) || strings.Contains(got, "stale") {
		t.Errorf("recent: %q", got)
	}
	write(time.Now().Add(-9 * 24 * time.Hour))
	if got := dbAgeOf(db); !strings.Contains(got, "stale") {
		t.Errorf("a week old: %q", got)
	}
	if got, _ := depScanAnswer(0, []byte(`{"results":[]}`), "", "/agent/data", "/agent", dbAgeOf(db)); !strings.Contains(got, "stale") {
		t.Errorf("a clean answer with stale databases: %q", got)
	}
}
