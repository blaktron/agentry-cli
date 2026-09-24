// The Dockerfile's artefact assertion (agentry-cli#235): the build stage must
// refuse a result that is not an executable ELF binary, because building the
// embedded Dockerfile from the repository root instead of the context
// Materialize writes turns ./runnerimage into a library package and `go build
// -o` into a 0644 `!<arch>` archive that exits 0. The line is lifted out of
// the Dockerfile verbatim and run under sh against each kind of artefact, so
// the assertion that ships is the one this proves.

package runnerimage

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const artefactPath = "/agentry-runner"

// assertionLine returns the Dockerfile's `RUN test -x /agentry-runner …`
// command, without the RUN keyword.
func assertionLine(t *testing.T) string {
	t.Helper()
	df, err := contextFS.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	var found string
	for _, line := range strings.Split(string(df), "\n") {
		if strings.HasPrefix(line, "RUN test -x "+artefactPath) {
			if found != "" {
				t.Fatal("Dockerfile carries two artefact assertions")
			}
			found = strings.TrimPrefix(line, "RUN ")
		}
	}
	if found == "" {
		t.Fatalf("Dockerfile has no `RUN test -x %s` assertion after the build (agentry-cli#235)", artefactPath)
	}
	// It must run after the go build and before the runtime stage copies.
	s := string(df)
	build := strings.Index(s, "-o "+artefactPath+" ./runnerimage")
	assert := strings.Index(s, "RUN test -x "+artefactPath)
	runtimeStage := strings.Index(s, "COPY --from=build "+artefactPath)
	if build < 0 || runtimeStage < 0 || assert <= build || runtimeStage <= assert {
		t.Fatalf("assertion must sit between the go build (%d) and the runtime COPY (%d); it is at %d", build, runtimeStage, assert)
	}
	return found
}

func TestDockerfileAssertsExecutable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the assertion checks for an ELF binary; the image is always built on Linux")
	}
	line := assertionLine(t)
	dir := t.TempDir()

	self, err := os.Executable() // this test binary: a real executable ELF
	if err != nil {
		t.Fatal(err)
	}
	elf := filepath.Join(dir, "elf")
	copyFile(t, self, elf, 0o755)

	archive := []byte("!<arch>\n__.PKGDEF       0           0     0     644     42        `\n")
	write := func(name string, data []byte, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil { // umask-proof
			t.Fatal(err)
		}
		return p
	}

	tests := []struct {
		name string
		path string
		ok   bool
	}{
		{"executable ELF binary", elf, true},
		{"library archive as go build -o writes it (0644)", write("archive", archive, 0o644), false},
		{"library archive made executable", write("archive-x", archive, 0o755), false},
		{"non-executable ELF", func() string { p := filepath.Join(dir, "elf-nox"); copyFile(t, self, p, 0o644); return p }(), false},
		{"truncated ELF", write("short", []byte("\x7fEL"), 0o755), false},
		{"empty file", write("empty", nil, 0o755), false},
		{"missing file", filepath.Join(dir, "absent"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", strings.ReplaceAll(line, artefactPath, tc.path))
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()
			if tc.ok && err != nil {
				t.Fatalf("assertion refused a real binary: %v (%s)", err, stderr.String())
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("assertion passed an artefact that is not an executable ELF binary")
				}
				if !strings.Contains(stderr.String(), "Materialize") {
					t.Errorf("failure message does not point at the fix: %q", stderr.String())
				}
			}
		})
	}
}

func copyFile(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dst, mode); err != nil {
		t.Fatal(err)
	}
}

// The Dockerfile names no external frontend (agentry-cli#281). A `# syntax=`
// parser directive makes BuildKit pull that frontend image before it reads a
// line of the file — docker.io/docker/dockerfile:1, by a tag that can move, on
// every build of every client's first run — and that image's code parses the
// build. The file uses nothing the daemon's built-in frontend lacks, so it
// carries no directive and the bases (DR-32) are the only images the build
// fetches. Directives are only honoured in the leading comment block, but any
// `syntax=` directive line anywhere is refused so one cannot creep back in.
func TestDockerfileNamesNoExternalFrontend(t *testing.T) {
	df, err := contextFS.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(df), "\n") {
		l := strings.ToLower(strings.Join(strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "#")), ""))
		if strings.HasPrefix(strings.TrimSpace(line), "#") && strings.HasPrefix(l, "syntax=") {
			t.Errorf("Dockerfile line %d is a syntax directive (%q): it makes BuildKit pull an external frontend on every build (agentry-cli#281)", i+1, line)
		}
	}
}
