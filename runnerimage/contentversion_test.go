package runnerimage

import (
	"regexp"
	"testing"
)

func TestContentVersionIsStableAndTagSafe(t *testing.T) {
	v := ContentVersion()
	if !regexp.MustCompile(`^c[0-9a-f]{12}$`).MatchString(v) {
		t.Fatalf("content version %q", v)
	}
	if ContentVersion() != v {
		t.Fatal("content version must be stable within a process")
	}
	if got := DefaultImageTag(); got != ImageTagPrefix+":"+v {
		t.Fatalf("default tag %q", got)
	}
	if !validTagVersion(v) {
		t.Fatal("content version must satisfy docker's tag grammar")
	}
}
