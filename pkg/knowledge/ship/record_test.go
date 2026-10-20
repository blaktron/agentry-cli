package ship

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
)

func TestTheSidecarRoundTripsAndGoesWithTheLastPack(t *testing.T) {
	dir := t.TempDir()
	r, err := ReadRecord(dir)
	if err != nil || len(r.Packs) != 0 {
		t.Fatalf("a package with no sidecar: %v %v", r, err)
	}
	o := &Origin{Kind: KindOrg, Org: "acme", PackID: "6f1c"}
	r.Set(Entry{Name: "b", SHA256: "sha256:b", Registry: "https://r.example/", Origin: o})
	r.Set(Entry{Name: "a", SHA256: "sha256:a"})
	r.Set(Entry{Name: "b", SHA256: "sha256:b2", Registry: "https://r.example/", Origin: o})
	if err := WriteRecord(dir, r); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRecord(dir)
	if err != nil || len(got.Packs) != 2 || got.Packs[0].Name != "a" || got.Packs[1].SHA256 != "sha256:b2" || *got.Packs[1].Origin != *o {
		t.Fatalf("read back %+v %v", got, err)
	}
	got.Remove("a")
	got.Remove("b")
	if err := WriteRecord(dir, got); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".agentry")); !os.IsNotExist(err) {
		t.Errorf("the sidecar's folder stayed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".agentry"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(RecordFile)), []byte(`{"schema":"other/v9","packs":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRecord(dir); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Errorf("a sidecar of another schema: %v", err)
	}
}

func TestAnOriginIsClaimedOnlyForThePackThatWasLaid(t *testing.T) {
	dir := packageDir(t)
	p := knowledgetest.Make(t, "runbooks", nil)
	laid := shipInto(t, dir, p)
	o := &Origin{Kind: KindOwn, PackID: "6f1c0f2e-0000-4000-8000-000000000001"}
	var r Record
	r.Set(Entry{Name: "runbooks", SHA256: laid.SHA256, Registry: "https://r.example", Origin: o})

	if got, why := OriginOf(dir, r, "runbooks", "https://r.example/"); got == nil || *got != *o || why != "" {
		t.Fatalf("the laid pack: %v %q", got, why)
	}
	if got, why := OriginOf(dir, r, "runbooks", "https://other.example"); got != nil || !strings.Contains(why, "not at this registry") {
		t.Errorf("another registry: %v %q", got, why)
	}
	if got, why := OriginOf(dir, r, "unknown", "https://r.example"); got != nil || why != "" {
		t.Errorf("a pack the sidecar does not know: %v %q", got, why)
	}
	// The pack's manifest.json is no longer the one that was laid: no claim.
	mf := filepath.Join(Dir(dir, "runbooks"), "manifest.json")
	raw, _ := os.ReadFile(mf)
	if err := os.WriteFile(mf, []byte(strings.Replace(string(raw), laid.SHA256, "sha256:0000", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, why := OriginOf(dir, r, "runbooks", "https://r.example"); got != nil || !strings.Contains(why, "not the ones laid down") {
		t.Errorf("a pack replaced by hand: %v %q", got, why)
	}
}
