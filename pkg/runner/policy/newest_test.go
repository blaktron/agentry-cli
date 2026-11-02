package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The record beside the policy file gives an adapter the policy pins
// nothing for its newest image; an entry that would not pass as a pin, one
// for an adapter whose newest release is never tried, and an unreadable
// record are ignored, and the policy's own pin always wins.
func TestTheRecordGivesOnlyUnpinnedAdaptersTheirNewestImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	if _, _, err := EnsureDefault(path); err != nil {
		t.Fatal(err)
	}
	good := Newest{Ref: "agentry-claude-code:9.9.9", Artifact: "sha512-" + strings.Repeat("A", 86) + "==", Version: "9.9.9"}
	for name, n := range map[string]Newest{
		"claude-image":  good,
		"codex-image":   {Ref: "ghcr.io/evil/codex:1", Artifact: "sha256:" + strings.Repeat("a", 64)},
		"copilot-image": {Ref: "agentry-copilot:9.9.9", Artifact: "sha256:" + strings.Repeat("a", 64)},
	} {
		if err := WriteNewest(NewestPathFor(path), name, n); err != nil {
			t.Fatal(err)
		}
	}
	pol, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := pol.Images.HarnessImageFor("claude-image"); h.Ref != good.Ref || h.Artifact != good.Artifact {
		t.Errorf("claude-image runs %+v", h)
	}
	for _, name := range []string{"codex-image", "copilot-image"} {
		if h, _ := pol.Images.HarnessImageFor(name); h != DefaultHarnessImages[name] {
			t.Errorf("%s runs %+v, not its default", name, h)
		}
	}
	pinned := DefaultHarnessImages["claude-image"]
	pol.Images.HarnessImages = map[string]HarnessImage{"claude-image": pinned}
	if h, _ := pol.Images.HarnessImageFor("claude-image"); h != pinned || !pol.Images.Pins("claude-image") {
		t.Errorf("the policy's own pin lost to the record: %+v", h)
	}
	if err := os.WriteFile(NewestPathFor(path), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	again, _, err := Load(path)
	if err != nil {
		t.Fatalf("an unreadable record failed the load: %v", err)
	}
	if h, _ := again.Images.HarnessImageFor("claude-image"); h != pinned && h != DefaultHarnessImages["claude-image"] {
		t.Errorf("an unreadable record gave %+v", h)
	}
}
