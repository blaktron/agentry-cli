package main

import (
	"slices"
	"testing"
)

// TestTheNewestVersionIsTriedFirst: the survey tries the registry's latest,
// then every other release newest first, and pre-releases only when the
// latest is one, so the version it records is the newest a run could pin.
func TestTheNewestVersionIsTriedFirst(t *testing.T) {
	npm := map[string]npmVersion{"1.2.0": {}, "1.10.0": {}, "1.9.0": {}, "2.0.0-beta.1": {}, "0.9.0": {}}
	if got, want := npmCandidates("1.10.0", npm, ""), []string{"1.10.0", "1.9.0", "1.2.0", "0.9.0"}; !slices.Equal(got, want) {
		t.Errorf("npm: %v, want %v", got, want)
	}
	if got := npmCandidates("2.0.0-beta.1", npm, ""); got[0] != "2.0.0-beta.1" || !slices.Contains(got, "1.10.0") {
		t.Errorf("npm with a pre-release latest: %v", got)
	}
	if got := npmCandidates("1.10.0", npm, "1.2.0"); !slices.Equal(got, []string{"1.2.0"}) {
		t.Errorf("npm pinned: %v", got)
	}
	pypi := map[string][]pypiFile{"2026.8.18": nil, "2026.10.1": nil, "2025.4.25": nil, "2026.11.0rc1": nil}
	if got, want := pypiCandidates("2026.10.1", pypi, ""), []string{"2026.10.1", "2026.8.18", "2025.4.25"}; !slices.Equal(got, want) {
		t.Errorf("pypi: %v, want %v", got, want)
	}
}
