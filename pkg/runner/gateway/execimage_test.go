package gateway

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// The exec image is built once from a generated Dockerfile that runs the
// runner's minimize role in exec form (no shell needed after busybox is
// gone), reused when present, and skipped when nothing is declared.
func TestEnsureExecImage(t *testing.T) {
	fake := &dockerx.Fake{ExistingImages: map[string]bool{}}
	ctxDir := t.TempDir()
	tag, err := EnsureExecImage(t.Context(), fake, "agentry-runner:v1", []string{"jq", "git"}, ctxDir, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if want := spec.ExecImageTag("agentry-runner:v1", []string{"git", "jq"}); tag != want || !strings.HasPrefix(tag, "agentry-runner-exec:v1-") {
		t.Fatalf("tag = %q want %q", tag, want)
	}
	built := false
	for _, op := range fake.Ops() {
		if strings.HasPrefix(op, "Build:"+tag+"<-") {
			built = true
			dir := strings.TrimPrefix(op, "Build:"+tag+"<-")
			df, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(df), "FROM agentry-runner:v1\n") || !strings.Contains(string(df), `RUN ["/usr/local/bin/agentry-runner", "minimize", "git", "jq"]`) {
				t.Fatalf("dockerfile:\n%s", df)
			}
		}
	}
	if !built {
		t.Fatalf("no build recorded: %v", fake.Ops())
	}
	fake.ExistingImages[tag] = true
	before := len(fake.Ops())
	if again, err := EnsureExecImage(t.Context(), fake, "agentry-runner:v1", []string{"git", "jq"}, ctxDir, io.Discard); err != nil || again != tag {
		t.Fatalf("reuse: %q %v", again, err)
	}
	if n := len(fake.Ops()) - before; n != 1 { // ImageExists only
		t.Fatalf("reuse must not build: %v", fake.Ops()[before:])
	}
	if tag, err := EnsureExecImage(t.Context(), fake, "agentry-runner:v1", nil, ctxDir, io.Discard); err != nil || tag != "" {
		t.Fatalf("no exec: %q %v", tag, err)
	}
	if a, b := spec.ExecImageTag("r:v1", []string{"a"}), spec.ExecImageTag("r:v1", []string{"b"}); a == b {
		t.Fatal("different command sets must yield different tags")
	}
}
