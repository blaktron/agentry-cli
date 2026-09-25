// Package knowledgetest makes knowledge packs for other packages' tests,
// through the format's own apply step, the way every pack a person attaches
// is made.
package knowledgetest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
)

// DefaultFiles is a pack of one page citing one source.
var DefaultFiles = map[string]string{
	"wiki/topics/buckets.md": "---\ntitle: Buckets\ntype: topic\nsummary: Naming\nsources: [doc:guide.txt]\nupdated: 2026-09-25\n---\n\nA bucket name is globally unique [src: doc:guide.txt].\n",
	"sources/guide.txt":      "the guide\n",
}

// Make applies files (DefaultFiles when nil) as one ingest into a new pack
// named name under a temporary directory, and returns it loaded.
func Make(t testing.TB, name string, files map[string]string) *knowledge.Pack {
	t.Helper()
	if files == nil {
		files = DefaultFiles
	}
	out := t.TempDir()
	write := make([]string, 0, len(files))
	for rel, body := range files {
		p := filepath.Join(out, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		write = append(write, rel)
	}
	sort.Strings(write)
	ch, err := json.Marshal(knowledge.Changes{Schema: knowledge.ChangesSchema, Write: write})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, knowledge.ChangesFile), ch, 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), name)
	res, err := knowledge.Apply(dir, out, knowledge.ApplyOptions{Name: name, Now: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Dropped) > 0 {
		t.Fatalf("knowledgetest: the apply step dropped %v", res.Dropped)
	}
	p, err := knowledge.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
