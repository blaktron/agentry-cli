package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
)

// TestBuiltinsListsEveryBuiltinWithItsReleaseDigest (agentry-cli#470): the
// list is localrun.Builtins(), each with the archive digest the registry's
// release of it holds, in text and in --json.
func TestBuiltinsListsEveryBuiltinWithItsReleaseDigest(t *testing.T) {
	isolateEnv(t)
	var want []builtinEntry
	for _, b := range localrun.Builtins() {
		d, err := b.ArchiveDigest()
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, builtinEntry{Ref: b.Ref(), Digest: d})
	}

	stdout, _, err := runCLI(t, nil, "builtins")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != len(want) {
		t.Fatalf("builtins printed %q", stdout)
	}
	for i, e := range want {
		if lines[i] != e.Ref+"\t"+e.Digest {
			t.Errorf("line %d = %q, want %s %s", i, lines[i], e.Ref, e.Digest)
		}
	}

	stdout, _, err = runCLI(t, nil, "--json", "builtins")
	if err != nil {
		t.Fatal(err)
	}
	var got []builtinEntry
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("--json: %v in %s", err, stdout)
	}
	if len(got) != len(want) {
		t.Fatalf("--json = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("--json[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
