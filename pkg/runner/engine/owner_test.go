package engine

import (
	"encoding/json"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"os"
	"path/filepath"
	"testing"
)

// stampOwner adds the person's uid/gid, as the run's engine maps them, to
// the per-run tool spec and leaves every compiled field alone: the invoking
// user's on a rootful engine, root's on a rootless one.
func TestStampOwner(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "toolspec.json")
	if err := os.WriteFile(p, []byte(`{"specVersion":"2","workspaceDir":"/workspace","outDir":"/out","fsRead":{"enabled":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rd := &runDirs{engine: dockerx.LocalDockerEngine()}
	if err := stampOwner(p, rd); err != nil {
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
	if err := stampOwner(filepath.Join(dir, "absent.json"), rd); err != nil {
		t.Errorf("absent spec: %v", err)
	}
	// On a rootless engine the person is the container's root.
	rootless := &runDirs{engine: dockerx.Engine{Rootless: true}}
	if err := stampOwner(p, rootless); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if os.Getuid() >= 0 && (doc.Owner == nil || doc.Owner.UID != 0 || doc.Owner.GID != 0) {
		t.Errorf("rootless owner = %+v, want 0:0", doc.Owner)
	}
}
