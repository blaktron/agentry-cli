package deliver

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

func within(t *testing.T, dir, abs string) {
	t.Helper()
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", abs, err)
	}
	realDir, _ := filepath.EvalSymlinks(dir)
	if !strings.HasPrefix(real, filepath.Join(realDir, "reports")+string(filepath.Separator)) {
		t.Fatalf("%s resolves to %s, outside %s/reports", abs, real, realDir)
	}
}

func TestAttackPathSpellings(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "attack-secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })
	good := listed("reports/a.md", "", "hello", true)
	write(t, dir, "reports/a.md", "hello")
	write(t, dir, "reports/sub/b.md", "bee")
	write(t, dir, "receipts/events.ndjson", "{}")
	secret := listed(outside, "", "secret", true)
	secretDigest := secret.SHA256

	mk := func(p string) receipt.Deliverable { g := good; g.Path = p; return g }
	refused := []string{
		"", ".", "..", "/", "reports", "reports/", "reports/.", "reports/..", "reports/../receipts/events.ndjson",
		"../" + filepath.Base(dir) + "/reports/a.md", "reports/../../" + filepath.Base(dir) + "/reports/a.md/../../../x",
		"/etc/passwd", outside, "//etc/passwd", `reports\a.md`, `\reports\a.md`, `reports/..\a.md`, `C:\x`, `C:/x`, "REPORTS/a.md", "Reports/a.md",
		"reports/a.md\x00", "reports/a.md\x00/../../x", "reports/\x00", " reports/a.md", "reports /a.md", "reports/a.md/x", "reports/a.md/..",
		"reports%2fa.md", "reports/..%2f..%2fetc", "file:///etc/passwd", "reports/" + strings.Repeat("a", 5000), strings.Repeat("reports/", 600) + "a.md",
		"reports/nonexistent", "reports/sub", "reports/sub/", "reports/./sub",
	}
	for _, p := range refused {
		f := mk(p)
		if p == outside {
			f = secret
		}
		if strings.HasPrefix(p, "reports/") && strings.Contains(p, "/a.md/") {
			f = good
			f.Path = p
		}
		if abs, err := Locate(dir, f); err == nil {
			t.Errorf("Locate(%q) = %s, want refusal", p, abs)
		}
		if abs, _, err := Read(dir, f, 1<<20); err == nil {
			t.Errorf("Read(%q) = %s, want refusal", p, abs)
		}
	}
	_ = secretDigest
	// spellings that clean to a file under reports are served as that file,
	// and the returned path is the cleaned one, inside reports.
	for _, p := range []string{"./reports/a.md", "reports/./a.md", "reports//a.md", "reports/sub/../a.md", "reports/a.md/"} {
		abs, err := Locate(dir, mk(p))
		if err != nil {
			t.Logf("spelling %q refused: %v", p, err)
			continue
		}
		within(t, dir, abs)
		if strings.Contains(abs, "..") {
			t.Errorf("Locate(%q) = %q carries a ..", p, abs)
		}
	}
}

func TestAttackLinksAndSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	write(t, outDir, "secret.md", "secret")
	write(t, dir, "reports/real.md", "hello")
	write(t, dir, "receipts/events.ndjson", "{}")
	secret := listed("x", "", "secret", true)
	real := listed("x", "", "hello", true)
	var failures []string
	try := func(name string, f receipt.Deliverable, wantOK bool) {
		abs, err := Locate(dir, f)
		if (err == nil) != wantOK {
			failures = append(failures, name+": Locate err="+errString(err))
		} else if err == nil {
			within(t, dir, abs)
		}
		_, b, err := Read(dir, f, 1<<20)
		if (err == nil) != wantOK {
			failures = append(failures, name+": Read err="+errString(err))
		}
		if err != nil && len(b) != 0 {
			failures = append(failures, name+": Read returned bytes with an error")
		}
	}
	mkf := func(base receipt.Deliverable, p string) receipt.Deliverable { base.Path = p; return base }
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink(filepath.Join(outDir, "secret.md"), filepath.Join(dir, "reports", "link-out.md")))
	must(os.Symlink("real.md", filepath.Join(dir, "reports", "link-in.md")))
	must(os.Symlink(outDir, filepath.Join(dir, "reports", "linkdir")))
	must(os.Symlink("..", filepath.Join(dir, "reports", "up")))
	must(os.Symlink("/nonexistent", filepath.Join(dir, "reports", "dangling.md")))
	must(os.Symlink("link-in.md", filepath.Join(dir, "reports", "chain.md")))
	must(os.Mkdir(filepath.Join(dir, "reports", "dir.md"), 0o755))
	must(syscall.Mkfifo(filepath.Join(dir, "reports", "fifo.md"), 0o644))
	try("control", mkf(real, "reports/real.md"), true)
	try("link out", mkf(secret, "reports/link-out.md"), false)
	try("link in", mkf(real, "reports/link-in.md"), false)
	try("chain", mkf(real, "reports/chain.md"), false)
	try("dir link", mkf(secret, "reports/linkdir/secret.md"), false)
	try("up link", mkf(real, "reports/up/reports/real.md"), false)
	try("dangling", mkf(real, "reports/dangling.md"), false)
	try("directory", mkf(real, "reports/dir.md"), false)
	empty := listed("x", "", "", true)
	try("fifo", mkf(empty, "reports/fifo.md"), false)
	// reports itself a link
	dir2 := t.TempDir()
	write(t, outDir, "r/real.md", "hello")
	must(os.Symlink(filepath.Join(outDir, "r"), filepath.Join(dir2, "reports")))
	if _, err := Locate(dir2, mkf(real, "reports/real.md")); err == nil {
		failures = append(failures, "reports as a symlink to an outside dir was served")
	}
	// the run folder itself reached through a link is the person's own path
	holder := t.TempDir()
	must(os.Symlink(dir, filepath.Join(holder, "run")))
	if _, err := Locate(filepath.Join(holder, "run"), mkf(real, "reports/real.md")); err != nil {
		t.Logf("a run folder reached by a link is refused: %v", err)
	}
	for _, f := range failures {
		t.Error(f)
	}
}

func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

func TestAttackSizeAndDigestLies(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "reports/a.md", "hello")
	good := listed("reports/a.md", "", "hello", true)
	mut := func(f func(*receipt.Deliverable)) receipt.Deliverable { d := good; f(&d); return d }
	bad := map[string]receipt.Deliverable{
		"bytes+1":       mut(func(d *receipt.Deliverable) { d.Bytes++ }),
		"bytes-1":       mut(func(d *receipt.Deliverable) { d.Bytes-- }),
		"bytes 0":       mut(func(d *receipt.Deliverable) { d.Bytes = 0 }),
		"bytes neg":     mut(func(d *receipt.Deliverable) { d.Bytes = -1 }),
		"bytes max":     mut(func(d *receipt.Deliverable) { d.Bytes = 1<<63 - 1 }),
		"bytes min":     mut(func(d *receipt.Deliverable) { d.Bytes = -1 << 63 }),
		"sha upper":     mut(func(d *receipt.Deliverable) { d.SHA256 = strings.ToUpper(d.SHA256) }),
		"sha empty":     mut(func(d *receipt.Deliverable) { d.SHA256 = "" }),
		"sha prefixed":  mut(func(d *receipt.Deliverable) { d.SHA256 = "sha256:" + d.SHA256 }),
		"sha truncated": mut(func(d *receipt.Deliverable) { d.SHA256 = d.SHA256[:63] }),
	}
	for name, f := range bad {
		if abs, err := Locate(dir, f); err == nil {
			t.Errorf("%s: Locate accepted (%s)", name, abs)
		}
		if _, b, err := Read(dir, f, 1<<20); err == nil || len(b) != 0 {
			t.Errorf("%s: Read accepted", name)
		}
	}
	// the max argument
	for _, m := range []int64{0, -1, 4, 5} {
		_, b, err := Read(dir, good, m)
		if (m >= 5) != (err == nil) || (err != nil && len(b) != 0) {
			t.Errorf("Read max=%d: %v %q", m, err, b)
		}
	}
	// a file that grows or shrinks after the envelope
	write(t, dir, "reports/a.md", "hello world")
	if _, err := Locate(dir, good); err == nil {
		t.Error("a grown file was accepted")
	}
	write(t, dir, "reports/a.md", "hell")
	if _, err := Locate(dir, good); err == nil {
		t.Error("a shrunk file was accepted")
	}
}

func TestAttackDangerousTypes(t *testing.T) {
	dir := t.TempDir()
	var missed []string
	for _, name := range []string{"x.lnk", "x.LNK", "x.Lnk", "x.url", "x.desktop", "x.webloc", "x.scf", "x.library-ms", "x.searchconnector-ms", "x.appref-ms", "x.fileloc", "x.glk",
		"x.lnk.", "x.lnk ", "x.lnk.md.lnk", "x.lnk::$DATA", "x.lnk:stream"} {
		write(t, dir, "reports/"+name, "z")
		f := listed("reports/"+name, "", "z", true)
		if _, err := Locate(dir, f); err == nil {
			missed = append(missed, name)
		}
	}
	t.Logf("accepted (Linux, so a trailing dot or space or a stream name is just a name): %v", missed)
	for _, m := range missed {
		switch m {
		case "x.lnk.", "x.lnk ", "x.lnk::$DATA", "x.lnk:stream":
		default:
			t.Errorf("a dangerous type was handed back: %s", m)
		}
	}
}

// The path Locate returns names a file whose bytes were the listed ones at
// that moment. Nothing keeps the name bound to those bytes afterwards: a link
// put at the name later is followed by whoever opens the URI. This documents
// that, so the decision (it matters only to a writer of the run folder, i.e. a
// process of the same user once the sandbox is gone) is on record.
func TestAttackSwapAfterLocateIsNotDetectedByTheURI(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	write(t, outDir, "secret.md", "secret")
	write(t, dir, "reports/a.md", "hello")
	f := listed("reports/a.md", "", "hello", true)
	abs, err := Locate(dir, f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(abs); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outDir, "secret.md"), abs); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(abs)
	if string(b) != "secret" {
		t.Fatalf("expected the swapped link to be followed, got %q", b)
	}
	// ... but a second check refuses it, which is why a client must hold the
	// digest the agent put in _meta and re-verify (or the agent re-Locate).
	if _, err := Locate(dir, f); err == nil {
		t.Fatal("the swapped link passed a second Locate")
	}
}

// Read serves the bytes it hashed: replacing the file while Read runs cannot
// make it return other bytes.
func TestAttackReadReturnsOnlyHashedBytes(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("A", 200<<10)
	write(t, dir, "reports/a.md", big)
	f := listed("reports/a.md", "", big, true)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			content := big
			if i%2 == 1 {
				content = strings.Repeat("B", 200<<10)
			}
			tmp := filepath.Join(dir, "reports", ".tmp")
			_ = os.WriteFile(tmp, []byte(content), 0o644)
			_ = os.Rename(tmp, filepath.Join(dir, "reports", "a.md"))
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	n := 0
	for time.Now().Before(deadline) {
		_, b, err := Read(dir, f, 1<<20)
		if err == nil && string(b) != big {
			t.Fatal("Read returned bytes that are not the listed ones")
		}
		n++
	}
	close(stop)
	<-done
	t.Logf("%d reads under a racing writer", n)
}
