package deliver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

var when = time.Date(2026, 10, 17, 15, 4, 0, 0, time.Local)

func write(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func listed(rel, title, content string, marked bool) receipt.Deliverable {
	sum := sha256.Sum256([]byte(content))
	return receipt.Deliverable{Path: rel, Title: title, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(content)), Type: strings.TrimPrefix(strings.ToLower(filepath.Ext(rel)), "."), Marked: marked}
}

// runFolder makes a run's output folder holding the given reports and an
// envelope listing them.
func runFolder(t *testing.T, reports map[string]string, status string) (string, []receipt.Deliverable) {
	t.Helper()
	dir := t.TempDir()
	var files []receipt.Deliverable
	for _, rel := range sortedKeys(reports) {
		write(t, dir, rel, reports[rel])
		files = append(files, listed(rel, "", reports[rel], true))
	}
	env := receipt.Envelope{Agentry: receipt.EnvelopeVersion, Status: status, RunID: "ab12cd34", Ref: "acme/weekly@1.2.0", OutDir: dir}
	if status == "ok" {
		env.Deliverables = &receipt.Deliverables{Files: files}
	}
	b, err := env.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "result.json", string(b))
	write(t, dir, "receipts/events.ndjson", "{}\n")
	return dir, files
}

func sortedKeys(m map[string]string) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// snapshot is every file under dir by path, with its mode and content, and
// every directory and link too.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			out[rel] = "link->" + target
		case fi.IsDir():
			out[rel] = "dir"
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = fi.Mode().String() + " " + string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func equalSnapshots(t *testing.T, before, after map[string]string, except ...string) {
	t.Helper()
	for _, e := range except {
		delete(before, e)
		delete(after, e)
	}
	if len(before) != len(after) {
		t.Errorf("the run folder holds %d entries, it held %d", len(after), len(before))
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed: %.60q -> %.60q", k, v, after[k])
		}
	}
}

func opts(root string) FolderOptions {
	return FolderOptions{Root: root, Agent: "weekly", Label: "ab12cd34", When: when}
}

func TestACopyIsMadeAndTheRunFolderIsLeftAlone(t *testing.T) {
	dir, files := runFolder(t, map[string]string{
		"reports/summary.md":    "# Summary\n",
		"reports/data/rows.csv": "a,b\n1,2\n",
	}, "ok")
	root := filepath.Join(t.TempDir(), "Documents", "Agentry")
	before := snapshot(t, dir)

	got := ToFolder(dir, files, opts(root))

	folder := filepath.Join(root, "weekly", "2026-10-17 1504 ab12cd34")
	if len(got) != 2 {
		t.Fatalf("deliveries = %+v", got)
	}
	for i, name := range []string{"rows.csv", "summary.md"} {
		d := got[i]
		if d.Status != receipt.DeliveryDelivered || d.Error != "" || d.Kind != "folder" || d.Destination != root || d.Object != filepath.Join(folder, name) || d.SHA256 != files[i].SHA256 {
			t.Errorf("delivery %d = %+v", i, d)
		}
		b, err := os.ReadFile(filepath.Join(folder, name))
		if err != nil || string(b) != map[string]string{"rows.csv": "a,b\n1,2\n", "summary.md": "# Summary\n"}[name] {
			t.Errorf("%s = %q, %v", name, b, err)
		}
	}
	equalSnapshots(t, before, snapshot(t, dir))
}

func TestAChangedFileIsRefusedAndLeavesNoCopy(t *testing.T) {
	dir, files := runFolder(t, map[string]string{"reports/a.md": "original", "reports/b.md": "fine"}, "ok")
	write(t, dir, "reports/a.md", "tampered") // the same size, other bytes
	write(t, dir, "reports/b.md", "fine")
	root := t.TempDir()
	before := snapshot(t, dir)

	got := ToFolder(dir, files, opts(root))

	if got[0].Status != receipt.DeliveryRefused || got[0].Error != receipt.DeliveryErrChanged || got[0].Object != "" {
		t.Errorf("the changed file = %+v", got[0])
	}
	if got[1].Status != receipt.DeliveryDelivered {
		t.Errorf("the other file = %+v", got[1])
	}
	folder := filepath.Join(root, "weekly", "2026-10-17 1504 ab12cd34")
	entries, _ := os.ReadDir(folder)
	if len(entries) != 1 || entries[0].Name() != "b.md" {
		t.Errorf("the folder holds %v, want b.md alone", entries)
	}
	equalSnapshots(t, before, snapshot(t, dir))

	// A file that grew or shrank is refused too.
	write(t, dir, "reports/b.md", "fine and longer")
	if got := ToFolder(dir, files[1:], opts(root)); got[0].Error != receipt.DeliveryErrChanged {
		t.Errorf("a file that grew = %+v", got[0])
	}
	write(t, dir, "reports/b.md", "fin")
	if got := ToFolder(dir, files[1:], opts(root)); got[0].Error != receipt.DeliveryErrChanged {
		t.Errorf("a file that shrank = %+v", got[0])
	}
}

func TestAListedPathOutsideReportsIsRefused(t *testing.T) {
	dir, _ := runFolder(t, map[string]string{"reports/a.md": "x"}, "ok")
	write(t, dir, "receipts/secret.txt", "internal")
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	var files []receipt.Deliverable
	for _, p := range []string{"result.json", "receipts/secret.txt", "reports/../receipts/secret.txt", "../outside.md", "/etc/hostname", "reports", "reports/", outside} {
		d := listed(p, "", "internal", true)
		files = append(files, d)
	}
	root := t.TempDir()
	for _, d := range ToFolder(dir, files, opts(root)) {
		if d.Status != receipt.DeliveryRefused || d.Error != receipt.DeliveryErrOutside || d.Object != "" {
			t.Errorf("%+v, want refused as outside reports/", d)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "weekly")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(root, "weekly", "2026-10-17 1504 ab12cd34"))
		if len(entries) != 0 {
			t.Errorf("a refused path left %v", entries)
		}
	}
}

func TestALinkIsNeverFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need privileges on Windows")
	}
	dir, _ := runFolder(t, map[string]string{"reports/real.md": "real"}, "ok")
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	write(t, other, "inner.md", "in a linked folder")
	for link, target := range map[string]string{
		"reports/file-link.md": secret,
		"reports/inside.md":    filepath.Join(dir, "reports", "real.md"),
		"reports/dir-link":     other,
	} {
		if err := os.Symlink(target, filepath.Join(dir, filepath.FromSlash(link))); err != nil {
			t.Fatal(err)
		}
	}
	files := []receipt.Deliverable{
		listed("reports/file-link.md", "", "not yours", true),
		listed("reports/inside.md", "", "real", true),
		listed("reports/dir-link/inner.md", "", "in a linked folder", true),
		listed("reports/real.md", "", "real", true),
	}
	root := t.TempDir()
	got := ToFolder(dir, files, opts(root))
	for i := range 3 {
		if got[i].Status != receipt.DeliveryRefused || got[i].Error != receipt.DeliveryErrLink {
			t.Errorf("delivery %d = %+v, want refused as a link", i, got[i])
		}
	}
	if got[3].Status != receipt.DeliveryDelivered {
		t.Errorf("the regular file = %+v", got[3])
	}
	entries, _ := os.ReadDir(filepath.Join(root, "weekly", "2026-10-17 1504 ab12cd34"))
	if len(entries) != 1 {
		t.Errorf("the folder holds %v, want real.md alone", entries)
	}
}

func TestAMissingFileAndADirectoryAreRefused(t *testing.T) {
	dir, _ := runFolder(t, map[string]string{"reports/dir/x.md": "x"}, "ok")
	got := ToFolder(dir, []receipt.Deliverable{listed("reports/gone.md", "", "x", true), listed("reports/dir", "", "x", true)}, opts(t.TempDir()))
	if got[0].Error != receipt.DeliveryErrMissing || got[1].Error != receipt.DeliveryErrLink {
		t.Errorf("%+v", got)
	}
	for _, d := range got {
		if d.Status != receipt.DeliveryRefused {
			t.Errorf("%+v", d)
		}
	}
}

func TestANameFromTheAgentNeverEscapesTheFolder(t *testing.T) {
	names := map[string]string{
		"reports/..hidden.md": "hidden.md",
		"reports/a:b.md":      "a_b.md",
		"reports/CON.txt":     "_CON.txt",
		"reports/q3" + string(rune(0x202e)) + "fdp.md": "q3_fdp.md",
	}
	reports := map[string]string{}
	for k := range names {
		reports[k] = "x"
	}
	dir, files := runFolder(t, reports, "ok")
	root := t.TempDir()
	folder := filepath.Join(root, "weekly", "2026-10-17 1504 ab12cd34")
	for _, d := range ToFolder(dir, files, opts(root)) {
		if d.Status != receipt.DeliveryDelivered {
			t.Fatalf("%+v", d)
		}
		if filepath.Dir(d.Object) != folder {
			t.Errorf("%q is not directly in %q", d.Object, folder)
		}
	}
	entries, _ := os.ReadDir(folder)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	slices.Sort(got)
	want := []string{"_CON.txt", "a_b.md", "hidden.md", "q3_fdp.md"}
	if !slices.Equal(got, want) {
		t.Errorf("names = %q, want %q", got, want)
	}
}

func TestACollisionTakesANumberAndNeverReplaces(t *testing.T) {
	dir, files := runFolder(t, map[string]string{"reports/a/notes.md": "from a", "reports/b/notes.md": "from b", "reports/c/notes.md": "from c"}, "ok")
	root := t.TempDir()
	folder := filepath.Join(root, "weekly", "2026-10-17 1504 ab12cd34")
	write(t, folder, "notes (2).md", "the person's own")

	got := ToFolder(dir, files, opts(root))

	want := []string{"notes.md", "notes (3).md", "notes (4).md"}
	for i, d := range got {
		if d.Status != receipt.DeliveryDelivered || filepath.Base(d.Object) != want[i] {
			t.Errorf("delivery %d = %+v, want %s", i, d, want[i])
		}
	}
	if b, _ := os.ReadFile(filepath.Join(folder, "notes (2).md")); string(b) != "the person's own" {
		t.Errorf("a file already there was replaced: %q", b)
	}
	// Delivered again in the same minute, every file takes the next numbers.
	again := ToFolder(dir, files[:1], opts(root))
	if filepath.Base(again[0].Object) != "notes (5).md" {
		t.Errorf("a second delivery = %+v", again[0])
	}
}

func TestADestinationThatCannotBeUsedFailsEveryFileAndNothingElse(t *testing.T) {
	dir, files := runFolder(t, map[string]string{"reports/a.md": "x"}, "ok")
	before := snapshot(t, dir)
	asFile := write(t, t.TempDir(), "afile", "x")
	for name, root := range map[string]string{"a relative folder": "Documents/Agentry", "a path under a file": filepath.Join(asFile, "sub")} {
		got := ToFolder(dir, files, opts(root))
		if len(got) != 1 || got[0].Status != receipt.DeliveryFailed || got[0].Error != receipt.DeliveryErrDestination {
			t.Errorf("%s = %+v", name, got)
		}
	}
	equalSnapshots(t, before, snapshot(t, dir))
}

func TestALinkInsideTheDestinationThatLeavesItIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need privileges on Windows")
	}
	dir, files := runFolder(t, map[string]string{"reports/a.md": "x"}, "ok")
	root, elsewhere := t.TempDir(), t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(root, "weekly")); err != nil {
		t.Fatal(err)
	}
	got := ToFolder(dir, files, opts(root))
	if got[0].Status != receipt.DeliveryFailed || got[0].Error != receipt.DeliveryErrDestination {
		t.Errorf("%+v", got[0])
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("a link out of the destination was followed: %v", entries)
	}
}

func TestTheFoldersNameComesFromTheRun(t *testing.T) {
	one := []receipt.Deliverable{{Title: "Weekly summary"}}
	if got := Label(one, "ab12cd34"); got != "Weekly summary" {
		t.Errorf("Label(one titled) = %q", got)
	}
	if got := Label(append(one, receipt.Deliverable{Title: "Data"}), "ab12cd34"); got != "ab12cd34" {
		t.Errorf("Label(two) = %q", got)
	}
	if got := Label([]receipt.Deliverable{{}}, "ab12cd34"); got != "ab12cd34" {
		t.Errorf("Label(untitled) = %q", got)
	}
	for ref, want := range map[string]string{"acme/weekly@1.2.0": "weekly", "acme/weekly": "weekly", "weekly": "weekly", "agentry/drafter@1.15.3": "drafter"} {
		if got := AgentName(ref); got != want {
			t.Errorf("AgentName(%q) = %q, want %q", ref, got, want)
		}
	}
	// The title is a path segment too.
	dir, files := runFolder(t, map[string]string{"reports/a.md": "x"}, "ok")
	root := t.TempDir()
	o := opts(root)
	o.Label = "../../etc/passwd: the plan"
	got := ToFolder(dir, files, o)
	if got[0].Status != receipt.DeliveryDelivered || !strings.HasPrefix(got[0].Object, filepath.Join(root, "weekly")+string(filepath.Separator)) {
		t.Errorf("%+v", got[0])
	}
	if filepath.Dir(filepath.Dir(got[0].Object)) != filepath.Join(root, "weekly") {
		t.Errorf("the title made a deeper path: %s", got[0].Object)
	}
}

func TestRedeliverAddsOnlyItsRecord(t *testing.T) {
	dir, _ := runFolder(t, map[string]string{"reports/summary.md": "# Summary\n"}, "ok")
	before := snapshot(t, dir)
	root := t.TempDir()

	got, err := Redeliver(dir, FolderOptions{Root: root, When: when})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Status != receipt.DeliveryDelivered {
		t.Fatalf("deliveries = %+v", got)
	}
	// The folder is named for the QuickAgent and, the run having one
	// deliverable, its title (here none, so the run id).
	if want := filepath.Join(root, "weekly", "2026-10-17 1504 ab12cd34", "summary.md"); got[0].Object != want {
		t.Errorf("object = %s, want %s", got[0].Object, want)
	}
	after := snapshot(t, dir)
	if line, ok := after["receipts/"+LedgerFile]; !ok || !strings.Contains(line, `"status":"delivered"`) {
		t.Fatalf("the record is missing: %q", line)
	}
	equalSnapshots(t, before, after, "receipts/"+LedgerFile)

	// A second delivery adds its lines, and still nothing else changes.
	if _, err := Redeliver(dir, FolderOptions{Root: root, When: when.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "receipts", LedgerFile))
	if n := strings.Count(string(b), "\n"); n != 2 {
		t.Errorf("the ledger holds %d lines, want 2:\n%s", n, b)
	}
	equalSnapshots(t, before, snapshot(t, dir), "receipts/"+LedgerFile)
}

func TestRedeliverNeedsASuccessfulRunThatListsDeliverables(t *testing.T) {
	failed, _ := runFolder(t, map[string]string{"reports/a.md": "x"}, "error")
	if _, err := Redeliver(failed, FolderOptions{Root: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "did not succeed") {
		t.Errorf("a failed run = %v", err)
	}
	empty := t.TempDir()
	env := receipt.Envelope{Agentry: receipt.EnvelopeVersion, Status: "ok"}
	b, _ := env.Marshal()
	write(t, empty, "result.json", string(b))
	if _, err := Redeliver(empty, FolderOptions{Root: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "no deliverables") {
		t.Errorf("a run with none = %v", err)
	}
	if _, err := Redeliver(t.TempDir(), FolderOptions{Root: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "not a run's output folder") {
		t.Errorf("an empty folder = %v", err)
	}
	notEnvelope := t.TempDir()
	write(t, notEnvelope, "result.json", `{"agentry":"other/v9"}`)
	if _, err := Redeliver(notEnvelope, FolderOptions{Root: t.TempDir()}); err == nil {
		t.Error("a result.json of another schema was accepted")
	}
}

func TestARunWritesItsOwnLedgerAnew(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "receipts/"+LedgerFile, "an earlier run's line\n")
	if err := WriteLedger(dir, []receipt.Delivery{{Destination: "/d", Kind: "folder", Status: "delivered"}}, false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "receipts", LedgerFile))
	var d receipt.Delivery
	if strings.Count(string(b), "\n") != 1 || json.Unmarshal(b, &d) != nil || d.Destination != "/d" {
		t.Errorf("the ledger = %q", b)
	}
}

func TestTheDownloadFlagsAreTheSystemsOwn(t *testing.T) {
	if zoneIdentifier != "[ZoneTransfer]\r\nZoneId=3\r\n" {
		t.Errorf("zone identifier = %q", zoneIdentifier)
	}
	if got := quarantineValue(time.Unix(0x68f1a2b3, 0)); got != "0081;68f1a2b3;Agentry;" {
		t.Errorf("quarantine value = %q", got)
	}
}

func TestATypeTheSystemActsOnWhenAFolderIsViewedIsNeverCopied(t *testing.T) {
	reports := map[string]string{"reports/ok.md": "fine"}
	for _, name := range []string{"Q3.lnk", "Q3.LNK", "icons.scf", "go.url", "notes.library-ms", "app.desktop", "site.webloc", "place.fileloc", "x.searchConnector-ms"} {
		reports["reports/"+name] = "payload"
	}
	dir, files := runFolder(t, reports, "ok")
	root := t.TempDir()
	got := ToFolder(dir, files, opts(root))
	delivered := 0
	for i, d := range got {
		if files[i].Path == "reports/ok.md" {
			if d.Status != receipt.DeliveryDelivered {
				t.Errorf("ok.md = %+v", d)
			}
			delivered++
			continue
		}
		if d.Status != receipt.DeliveryRefused || d.Error != receipt.DeliveryErrType || d.Object != "" {
			t.Errorf("%s = %+v, want refused as a type", files[i].Path, d)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(root, "weekly", "2026-10-17 1504 ab12cd34"))
	if len(entries) != delivered {
		t.Errorf("the folder holds %v, want ok.md alone", entries)
	}
}

func TestADeliveryNeverWritesIntoTheRunFolder(t *testing.T) {
	dir, files := runFolder(t, map[string]string{"reports/a.md": "a"}, "ok")
	before := snapshot(t, dir)
	for name, root := range map[string]string{"the run folder": dir, "its reports": filepath.Join(dir, "reports"), "a new folder under it": filepath.Join(dir, "reports", "copies")} {
		got := ToFolder(dir, files, opts(root))
		if got[0].Status != receipt.DeliveryFailed || got[0].Error != receipt.DeliveryErrDestination {
			t.Errorf("%s = %+v", name, got[0])
		}
	}
	equalSnapshots(t, before, snapshot(t, dir))
	if runtime.GOOS != "windows" {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(dir, link); err != nil {
			t.Fatal(err)
		}
		if got := ToFolder(dir, files, opts(link)); got[0].Status != receipt.DeliveryFailed {
			t.Errorf("a link to the run folder = %+v", got[0])
		}
		equalSnapshots(t, before, snapshot(t, dir))
	}
	// The folder around the run is fine.
	if got := ToFolder(dir, files, opts(filepath.Dir(dir))); got[0].Status != receipt.DeliveryDelivered {
		t.Errorf("the folder around the run = %+v", got[0])
	}
}

func TestNoUnverifiedFileIsSeenUnderADeliverablesName(t *testing.T) {
	dir, files := runFolder(t, map[string]string{"reports/a.md": "original"}, "ok")
	write(t, dir, "reports/a.md", "tampered") // the same size
	root := t.TempDir()
	ToFolder(dir, files, opts(root))
	entries, err := os.ReadDir(filepath.Join(root, "weekly", "2026-10-17 1504 ab12cd34"))
	if err != nil || len(entries) != 0 {
		t.Errorf("the folder holds %v (%v) after a refused copy, including hidden files", entries, err)
	}
	// A delivered file leaves no partial beside it.
	write(t, dir, "reports/a.md", "original")
	ToFolder(dir, files, opts(root))
	entries, _ = os.ReadDir(filepath.Join(root, "weekly", "2026-10-17 1504 ab12cd34"))
	if len(entries) != 1 || entries[0].Name() != "a.md" {
		t.Errorf("the folder holds %v, want a.md alone", entries)
	}
}

func TestALinkIsNotTheDeliveriesLedger(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need privileges on Windows")
	}
	dir, _ := runFolder(t, map[string]string{"reports/a.md": "a"}, "ok")
	before, _ := os.ReadFile(filepath.Join(dir, "result.json"))
	if err := os.Symlink(filepath.Join(dir, "result.json"), filepath.Join(dir, "receipts", LedgerFile)); err != nil {
		t.Fatal(err)
	}
	if err := WriteLedger(dir, []receipt.Delivery{{Destination: "/d", Kind: "folder", Status: "delivered"}}, true); err == nil {
		t.Error("a link was taken for the ledger")
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "result.json")); string(after) != string(before) {
		t.Error("the ledger's link let a delivery write result.json")
	}
}

// TestStageCopiesOnlyWhatTheEnvelopeNames: a staging folder gets a verified
// copy of each file that fits, under names of its own, and never a changed file,
// a link, a path outside reports/ or a file past the size given.
func TestStageCopiesOnlyWhatTheEnvelopeNames(t *testing.T) {
	run := t.TempDir()
	write := func(rel, body string) receipt.Deliverable {
		t.Helper()
		full := filepath.Join(run, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		return receipt.Deliverable{Path: rel, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(body))}
	}
	good := write("reports/a.md", "# a\n")
	changed := write("reports/b.md", "before")
	if err := os.WriteFile(filepath.Join(run, "reports/b.md"), []byte("after!"), 0o644); err != nil {
		t.Fatal(err)
	}
	big := write("reports/big.bin", strings.Repeat("x", 100))
	outside := write("notes.md", "n")
	linked := write("reports/real.md", "real")
	linked.Path = "reports/link.md"
	if err := os.Symlink(filepath.Join(run, "reports/real.md"), filepath.Join(run, "reports/link.md")); err != nil {
		t.Skip("no symlinks here")
	}
	missing := receipt.Deliverable{Path: "reports/gone.md", SHA256: good.SHA256, Bytes: 4}
	dir := t.TempDir()
	got := Stage(run, []receipt.Deliverable{good, changed, big, outside, linked, missing}, dir, 50)
	want := []Staged{{Name: "f0000"}, {Reason: receipt.DeliveryErrChanged}, {Reason: receipt.DeliveryErrTooLarge}, {Reason: receipt.DeliveryErrOutside}, {Reason: receipt.DeliveryErrLink}, {Reason: receipt.DeliveryErrMissing}}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("file %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "f0000" {
		t.Errorf("the staging folder holds %v, want f0000 alone", entries)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "f0000")); string(b) != "# a\n" {
		t.Errorf("staged bytes: %q", b)
	}
}

// TestStageStopsAtTheBudget: files are staged in order while their base64 fits
// the budget of the body they will ride in, and the first one that does not fit
// is not staged (nor is anything after it that would not fit what is left).
func TestStageStopsAtTheBudget(t *testing.T) {
	run := t.TempDir()
	if err := os.MkdirAll(filepath.Join(run, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	var files []receipt.Deliverable
	for i := 0; i < 8; i++ {
		body := strings.Repeat(string(rune('a'+i)), 9000)
		rel := "reports/f" + string(rune('a'+i)) + ".txt"
		if err := os.WriteFile(filepath.Join(run, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		files = append(files, receipt.Deliverable{Path: rel, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(body))})
	}
	// 9000 bytes are 12000 in base64: a budget of 40000 takes three.
	got := Stage(run, files, t.TempDir(), 40000)
	staged := 0
	for i, s := range got {
		if s.Name != "" {
			staged++
		} else if s.Reason != receipt.DeliveryErrTooLarge {
			t.Errorf("file %d: %+v", i, s)
		}
	}
	if staged != 3 || got[3].Name != "" {
		t.Errorf("%d staged: %+v", staged, got)
	}
}
