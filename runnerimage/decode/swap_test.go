package decode

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A decoder the archive exploits runs in the same container as the unpacker,
// beside it while an archive is read, and can swap a folder the unpacker has
// just made for a link out of the archive's folder. These tests swap a folder
// for such a link in each gap between the unpacker's check of a name and its
// use (decoder.afterCheck) and hold that no member is written beyond it.

const outsideSecret = "OUTSIDE-SECRET"

func TestAnArchiveMemberIsNotWrittenThroughAFolderSwappedForALinkAfterTheCheck(t *testing.T) {
	// The unpacker reaches the gap once for a folder the archive names, and
	// twice for each member it writes: before it makes the member's folders,
	// and before it opens the member.
	members := []zipEntry{
		{name: "sub/one.txt", body: "one"},
		{name: "sub/two.txt", body: "two"},
	}
	withFolder := append([]zipEntry{{name: "sub/", mode: fs.ModeDir | 0o755}}, members...)
	for _, c := range []struct {
		name    string
		nth     int
		entries []zipEntry
	}{
		{"before the first member's folders are made", 1, members},
		{"after the first member's folders are made", 2, members},
		{"before the second member's folders are made", 3, members},
		{"after the second member's folders are made", 4, members},
		{"before a folder the archive names is made", 1, withFolder},
		{"before the member below a folder the archive names is made", 2, withFolder},
	} {
		t.Run(c.name, func(t *testing.T) {
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "today.md"), []byte(outsideSecret+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			work := t.TempDir()
			src, dir := filepath.Join(work, "a.zip"), filepath.Join(work, "0")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(src, zipOf(t, c.entries...), 0o600); err != nil {
				t.Fatal(err)
			}
			d := &decoder{lim: Limits{MaxFiles: 10}, budget: 1 << 20}
			seen, swapped := 0, false
			d.afterCheck = func() {
				if seen++; seen != c.nth || swapped {
					return
				}
				swapped = true
				if err := os.RemoveAll(filepath.Join(dir, "sub")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(dir, "sub")); err != nil {
					t.Skipf("no symlinks here: %v", err)
				}
			}
			d.unpackInto(context.Background(), src, dir, "a.zip", contentKind{archive: formatZip})
			if !swapped {
				t.Fatalf("the gap was reached %d times, not %d", seen, c.nth)
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, e := range entries {
				got = append(got, e.Name())
			}
			if !slices.Equal(got, []string{"today.md"}) {
				t.Fatalf("the folder outside holds %v", got)
			}
			if b, _ := os.ReadFile(filepath.Join(outside, "today.md")); string(b) != outsideSecret+"\n" {
				t.Fatalf("a file outside was changed: %q", b)
			}
		})
	}
}
