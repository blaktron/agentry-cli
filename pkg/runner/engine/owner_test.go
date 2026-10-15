package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// stampOwner adds the invoking uid/gid to the per-run tool spec and leaves
// every compiled field alone.
func TestStampOwner(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "toolspec.json")
	if err := os.WriteFile(p, []byte(`{"specVersion":"2","workspaceDir":"/workspace","outDir":"/out","fsRead":{"enabled":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := stampOwner(p); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		SpecVersion string         `json:"specVersion"`
		FSRead      map[string]any `json:"fsRead"`
		Owner       *struct {
			UID int `json:"uid"`
			GID int `json:"gid"`
		} `json:"owner"`
	}
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SpecVersion != "2" || doc.FSRead["enabled"] != true {
		t.Errorf("compiled fields changed: %s", b)
	}
	if os.Getuid() >= 0 && (doc.Owner == nil || doc.Owner.UID != os.Getuid() || doc.Owner.GID != os.Getgid()) {
		t.Errorf("owner = %+v, want %d:%d", doc.Owner, os.Getuid(), os.Getgid())
	}
	// A plan without a tools server has no spec: not an error.
	if err := stampOwner(filepath.Join(dir, "absent.json")); err != nil {
		t.Errorf("absent spec: %v", err)
	}
}
