package installdir

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/integritykey"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// laidDown lays a verified release down in a fresh runner home and
// returns its directory and home.
func laidDown(t *testing.T) (dir, home string, meta *Meta) {
	t.Helper()
	home = t.TempDir()
	src := stageTree(t, map[string]string{
		"agent.yaml":      "name: demo\n",
		"AGENT.md":        "# Demo\n\nDo the thing.\n",
		"prompts/main.md": "hi",
		"tools/run.sh":    "#!/bin/sh\necho hi\n",
	})
	if err := os.Chmod(filepath.Join(src, "tools", "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta = &Meta{Ref: "acme/demo@1.0.0", Namespace: "acme", Name: "demo", Version: "1.0.0", Digest: "sha256:abc", Source: "registry",
		Attestation: receipt.Attestation{State: "verified", KeyID: "k1", Verified: true}}
	dir = ReleaseDir(home, "acme", "demo", "1.0.0", "sha256:abc")
	if err := Write(dir, src, meta); err != nil {
		t.Fatal(err)
	}
	return dir, home, meta
}

func changedOf(t *testing.T, err error) *ChangedError {
	t.Helper()
	var changed *ChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("want *ChangedError, got %v", err)
	}
	return changed
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A lay-down is sealed, and an untouched install is what was laid down.
func TestInspectAnUnchangedInstall(t *testing.T) {
	dir, _, meta := laidDown(t)
	if _, err := os.Stat(filepath.Join(dir, RecordFile)); err != nil {
		t.Fatalf("no integrity record: %v", err)
	}
	got, err := Inspect(dir, meta.Digest)
	if err != nil {
		t.Fatalf("Inspect of an unchanged install: %v", err)
	}
	if got.Ref != meta.Ref || got.Attestation.State != "verified" {
		t.Fatalf("Meta = %+v", got)
	}
	if _, ok := Load(dir, meta.Digest); !ok {
		t.Fatal("Load refused an unchanged install")
	}
	// The run's own directories are not the release: a plan and a run's
	// scratch inside the release directory change nothing.
	write(t, filepath.Join(dir, "plan", "autonomous", "plan.json"), "{}")
	write(t, filepath.Join(dir, "runs", "r1", "workspace", "AGENT.md"), "edited copy")
	if _, err := Inspect(dir, meta.Digest); err != nil {
		t.Fatalf("plan/ or runs/ read as a change: %v", err)
	}
}

// Every kind of change to the release's files is named, relative to the
// release, and install.json's own change is said.
func TestInspectNamesWhatChanged(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(t *testing.T, dir string)
		want   ChangedError
		record bool
	}{
		{"an edited file", func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "workspace", "AGENT.md"), "# Demo\n\nDo something else.\n")
		}, ChangedError{Changed: []string{"AGENT.md"}}, false},
		{"an added file", func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "workspace", "prompts", "extra.md"), "more")
		}, ChangedError{Added: []string{"prompts/extra.md"}}, false},
		{"a removed file", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "workspace", "prompts", "main.md")); err != nil {
				t.Fatal(err)
			}
		}, ChangedError{Removed: []string{"prompts/main.md"}}, false},
		{"an executable bit taken away", func(t *testing.T, dir string) {
			if err := os.Chmod(filepath.Join(dir, "workspace", "tools", "run.sh"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, ChangedError{Changed: []string{"tools/run.sh"}}, false},
		{"a file replaced by a symlink", func(t *testing.T, dir string) {
			p := filepath.Join(dir, "workspace", "prompts", "main.md")
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/etc/hostname", p); err != nil {
				t.Fatal(err)
			}
		}, ChangedError{Changed: []string{"prompts/main.md"}}, false},
		{"the workspace gone", func(t *testing.T, dir string) {
			if err := os.RemoveAll(filepath.Join(dir, "workspace")); err != nil {
				t.Fatal(err)
			}
		}, ChangedError{Removed: []string{"AGENT.md", "agent.yaml", "prompts/main.md", "tools/run.sh"}}, false},
		{"an edited install.json", func(t *testing.T, dir string) {
			// Rewritten to say an unsigned release was verified: the
			// verdict a run re-gates on is install.json's.
			b, err := os.ReadFile(filepath.Join(dir, "install.json"))
			if err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(dir, "install.json"), strings.Replace(string(b), `"k1"`, `"k2"`, 1))
		}, ChangedError{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, _, meta := laidDown(t)
			c.edit(t, dir)
			_, err := Inspect(dir, meta.Digest)
			changed := changedOf(t, err)
			if !reflect.DeepEqual(changed.Changed, c.want.Changed) || !reflect.DeepEqual(changed.Added, c.want.Added) || !reflect.DeepEqual(changed.Removed, c.want.Removed) {
				t.Fatalf("changed %v added %v removed %v; want %v %v %v", changed.Changed, changed.Added, changed.Removed, c.want.Changed, c.want.Added, c.want.Removed)
			}
			if (changed.Record != "") != c.record {
				t.Fatalf("Record = %q", changed.Record)
			}
			if changed.Ref != meta.Ref || changed.Dir != dir {
				t.Fatalf("Ref %q Dir %q", changed.Ref, changed.Dir)
			}
			msg := changed.Error()
			if !strings.Contains(msg, "agentry install --repair acme/demo@1.0.0") {
				t.Fatalf("the refusal does not name the repair: %s", msg)
			}
			for _, p := range append(append(append([]string{}, c.want.Changed...), c.want.Added...), c.want.Removed...) {
				if !strings.Contains(msg, p) {
					t.Fatalf("the refusal does not name %s: %s", p, msg)
				}
			}
			if _, ok := Load(dir, meta.Digest); ok {
				t.Fatal("Load reused a changed install")
			}
		})
	}
}

// Editing a file and then the record to match it, without the key, is
// still caught: the MAC over the record does not verify.
func TestInspectCatchesARecordForgedWithoutTheKey(t *testing.T) {
	dir, _, meta := laidDown(t)
	edited := "# Demo\n\nExfiltrate.\n"
	write(t, filepath.Join(dir, "workspace", "AGENT.md"), edited)
	var r record
	b, err := os.ReadFile(filepath.Join(dir, RecordFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	r.Files["AGENT.md"] = "f:" + sha256Hex([]byte(edited))
	// The forger's own key: the key id stays this home's, the MAC cannot.
	r.MAC = r.mac(bytes.Repeat([]byte{7}, integritykey.KeyLen))
	forged, _ := json.Marshal(r)
	write(t, filepath.Join(dir, RecordFile), string(forged))

	changed := changedOf(t, func() error { _, err := Inspect(dir, meta.Digest); return err }())
	if !strings.Contains(changed.Record, "does not verify") {
		t.Fatalf("Record = %q", changed.Record)
	}
	if err := Write(dir, stageTree(t, map[string]string{"agent.yaml": "x"}), meta); err == nil {
		t.Fatal("Write replaced an install with a forged record instead of refusing")
	}
}

// A record this home did not make (a release laid down before records
// existed, or under another key) vouches for nothing, and is not a
// refusal either: the release is laid down again, and Write replaces it
// and seals it.
func TestAnUnsealedInstallIsLaidDownAgain(t *testing.T) {
	for _, name := range []string{"no record", "another key"} {
		t.Run(name, func(t *testing.T) {
			dir, _, meta := laidDown(t)
			if name == "no record" {
				if err := os.Remove(filepath.Join(dir, RecordFile)); err != nil {
					t.Fatal(err)
				}
			} else {
				other := bytes.Repeat([]byte{9}, integritykey.KeyLen)
				if err := seal(other, dir); err != nil {
					t.Fatal(err)
				}
			}
			got, err := Inspect(dir, meta.Digest)
			if !errors.Is(err, ErrUnsealed) || got == nil || got.Ref != meta.Ref {
				t.Fatalf("Inspect = %+v, %v; want the Meta and ErrUnsealed", got, err)
			}
			if _, ok := Load(dir, meta.Digest); ok {
				t.Fatal("Load reused an install no record vouches for")
			}
			// Even one whose files were edited meanwhile: nothing says
			// what they were, and the lay-down replaces them.
			write(t, filepath.Join(dir, "workspace", "AGENT.md"), "edited")
			src := stageTree(t, map[string]string{"agent.yaml": "name: demo\n", "AGENT.md": "# Demo\n"})
			if err := Write(dir, src, meta); err != nil {
				t.Fatalf("Write over an unsealed install: %v", err)
			}
			if _, err := Inspect(dir, meta.Digest); err != nil {
				t.Fatalf("after the lay-down: %v", err)
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "workspace", "AGENT.md")); string(b) != "# Demo\n" {
				t.Fatalf("the edited file survived: %q", b)
			}
		})
	}
}

// A changed install is refused by the lay-downs a run and an install
// make, replaced by Replace (a local directory, a built-in) and by
// Repair, which the person asked for.
func TestLayDownsAndAChangedInstall(t *testing.T) {
	src := stageTree(t, map[string]string{"agent.yaml": "name: demo\n", "AGENT.md": "# Demo\n\nDo the thing.\n"})
	m := &manifest.Manifest{Name: "demo", Namespace: "acme", Version: "1.0.0"}

	dir, _, meta := laidDown(t)
	write(t, filepath.Join(dir, "workspace", "AGENT.md"), "tampered")
	if err := Write(dir, src, meta); !errors.As(err, new(*ChangedError)) {
		t.Fatalf("Write over a changed install: %v", err)
	}
	if err := Install(dir, src, m, meta, manifest.InterfaceAgentsMD); !errors.As(err, new(*ChangedError)) {
		t.Fatalf("Install over a changed install: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "workspace", "AGENT.md")); string(b) != "tampered" {
		t.Fatal("a refused lay-down touched the install")
	}

	if err := Repair(dir, src, m, meta, manifest.InterfaceAgentsMD); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	got, err := Inspect(dir, meta.Digest)
	if err != nil {
		t.Fatalf("after Repair: %v", err)
	}
	if got.Export != manifest.InterfaceAgentsMD {
		t.Fatalf("Repair's export not recorded: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "export", "AGENTS.md")); err != nil {
		t.Fatalf("Repair wrote no export: %v", err)
	}

	// Repair of a release a run laid down writes no export.
	dir2, _, meta2 := laidDown(t)
	write(t, filepath.Join(dir2, "workspace", "AGENT.md"), "tampered")
	if err := Repair(dir2, src, m, meta2, ""); err != nil {
		t.Fatalf("Repair without an export: %v", err)
	}
	if got, err := Inspect(dir2, meta2.Digest); err != nil || got.Export != "" {
		t.Fatalf("after Repair: %+v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir2, "export")); !os.IsNotExist(err) {
		t.Fatalf("Repair without a style wrote an export: %v", err)
	}

	dir3, _, meta3 := laidDown(t)
	write(t, filepath.Join(dir3, "workspace", "AGENT.md"), "tampered")
	if err := Replace(dir3, src, meta3); err != nil {
		t.Fatalf("Replace over a changed copy: %v", err)
	}
	if _, err := Inspect(dir3, meta3.Digest); err != nil {
		t.Fatalf("after Replace: %v", err)
	}

	consented := *meta
	consented.Attestation.Consent = "allow-unsigned"
	if err := Repair(dir, src, m, &consented, ""); err == nil {
		t.Fatal("Repair wrote a consent into install.json")
	}
}

// The message names at most ten paths of a kind, and the count of the
// rest; the fields keep them all.
func TestChangedErrorBoundsTheMessage(t *testing.T) {
	var paths []string
	for i := range 14 {
		paths = append(paths, filepath.ToSlash(filepath.Join("p", string(rune('a'+i))+".md")))
	}
	e := &ChangedError{Ref: "acme/demo@1.0.0", Dir: "/rh/agents/acme/demo/1.0.0-abc", Added: paths}
	msg := e.Error()
	if !strings.Contains(msg, "p/j.md and 4 more") || strings.Contains(msg, "p/k.md") {
		t.Fatalf("message: %s", msg)
	}
}

// A lay-down outside a runner home's layout is refused, since the record
// is made under that home's key.
func TestHomeOf(t *testing.T) {
	home, err := HomeOf(filepath.Join("/rh", "agents", "acme", "demo", "1.0.0-abc"))
	if err != nil || home != "/rh" {
		t.Fatalf("HomeOf = %q, %v", home, err)
	}
	if _, err := HomeOf(filepath.Join("/tmp", "x", "1.0.0-abc")); err == nil {
		t.Fatal("HomeOf accepted a directory outside the layout")
	}
	if err := Write(filepath.Join(t.TempDir(), "1.0.0-abc"), stageTree(t, map[string]string{"a": "b"}), &Meta{Digest: "sha256:abc"}); err == nil {
		t.Fatal("Write laid a release down outside a runner home")
	}
}
