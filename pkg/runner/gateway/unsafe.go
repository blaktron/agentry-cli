package gateway

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/runner/unsafeexec"
)

// ExecFailedFile is where an Unsafe Mode exec image build records what did
// not install ("what<TAB>why" a line). An image whose record is not empty
// is rebuilt on its next use rather than reused, so a passing mirror or
// PyPI fault does not leave a tool missing for good under the same tag
// (plan unsafe-mode.md §4.3).
const ExecFailedFile = "/etc/agentry/exec-failed"

//go:embed unsafe/install.sh
var unsafeInstallScript []byte

//go:embed unsafe/pip-record.py
var unsafePipRecord []byte

// ensureUnsafeExecImage builds an Unsafe Mode exec image when absent, or
// again when its failure record is not empty, and returns what it
// installed and, marked spec.ExecNotInstalledPrefix, what it did not.
func ensureUnsafeExecImage(ctx context.Context, d dockerx.Client, runnerImage string, img spec.ExecImageSpec, ctxDir string, logw io.Writer) ([]string, error) {
	ok, err := d.ImageExists(ctx, img.Tag)
	if err != nil {
		return nil, err
	}
	attempt := ""
	if ok {
		failed, err := d.ImageFile(ctx, img.Tag, ExecFailedFile)
		if err != nil {
			return nil, fmt.Errorf("read the Unsafe Mode exec image's failure record: %w", err)
		}
		if strings.TrimSpace(string(failed)) == "" {
			return unsafeRecords(ctx, d, img.Tag)
		}
		// A new attempt value busts the build cache from the install step
		// on, so the failed installs are tried again, not replayed.
		attempt = strconv.FormatInt(time.Now().UnixNano(), 10)
		_, _ = fmt.Fprintf(logw, "rebuilding %s: its last build left tools uninstalled\n", img.Tag)
	}
	dir := filepath.Join(ctxDir, "exec-"+img.Tag[strings.LastIndex(img.Tag, ":")+1:])
	if err := writeUnsafeContext(dir, runnerImage, img); err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(logw, "building %s (Unsafe Mode: %s + %s, not minimized)\n", img.Tag, img.Unsafe.Base, strings.Join(unsafeWhat(img), " "))
	var args map[string]string
	if attempt != "" {
		args = map[string]string{"AGENTRY_ATTEMPT": attempt}
	}
	if err := d.Build(ctx, dir, img.Tag, args); err != nil {
		return nil, fmt.Errorf("build Unsafe Mode exec image %s: %w", img.Tag, err)
	}
	return unsafeRecords(ctx, d, img.Tag)
}

// unsafeWhat names what the build installs, for its progress line.
func unsafeWhat(img spec.ExecImageSpec) []string {
	what := append([]string(nil), img.Packages...)
	for _, e := range img.Unsafe.Extras {
		if e.Kind == "pip" {
			what = append(what, e.Spec)
		}
	}
	return what
}

// writeUnsafeContext writes the build context: the Dockerfile, the static
// install script, and the data files it reads. Every value is checked
// against unsafeexec's grammar again here, because the context is where a
// value would become an argument (plan unsafe-mode.md invariant 3).
func writeUnsafeContext(dir, runnerImage string, img spec.ExecImageSpec) error {
	if err := checkUnsafeValues(img); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("exec image context: %w", err)
	}
	var pip, links strings.Builder
	for _, e := range img.Unsafe.Extras {
		switch e.Kind {
		case "pip":
			pip.WriteString(e.Command + "\t" + e.Spec + "\n")
		case "link":
			links.WriteString(e.Command + "\t" + e.Package + "\t" + e.Path + "\n")
		}
	}
	files := map[string][]byte{
		"Dockerfile":    []byte(unsafeDockerfile(runnerImage, img)),
		"install.sh":    unsafeInstallScript,
		"pip-record.py": unsafePipRecord,
		"packages.txt":  []byte(strings.Join(img.Packages, "\n") + "\n"),
		"pip.txt":       []byte(pip.String()),
		"links.txt":     []byte(links.String()),
	}
	if slices.Contains(img.Commands, "git") {
		files["git"] = []byte("declared\n")
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return fmt.Errorf("exec image context: %w", err)
		}
	}
	return nil
}

// checkUnsafeValues holds each value the context will carry to the
// grammar unsafeexec parsed it with: a plan file is data, and a hand-edited
// one must not become a shell line either.
func checkUnsafeValues(img spec.ExecImageSpec) error {
	// Parse holds the base, the packages and the extras to their grammars
	// exactly as it held the image's own files.
	var extras strings.Builder
	for _, e := range img.Unsafe.Extras {
		if e.Kind == "pip" {
			extras.WriteString("pip\t" + e.Command + "\t" + e.Spec + "\n")
		} else {
			extras.WriteString("link\t" + e.Command + "\t" + e.Package + "\t" + e.Path + "\n")
		}
	}
	var index strings.Builder
	for _, p := range img.Packages {
		index.WriteString("x\t" + p + "\n")
	}
	data, err := unsafeexec.Parse(img.Unsafe.Base, []byte(index.String()), []byte(extras.String()), nil)
	if err != nil {
		return fmt.Errorf("the plan's Unsafe Mode exec image: %w", err)
	}
	if data.Dropped > 0 {
		return fmt.Errorf("the plan's Unsafe Mode exec image names a package that is not a package name")
	}
	for _, c := range img.Commands {
		if strings.ContainsAny(c, "\t\n\"") {
			return fmt.Errorf("the plan's Unsafe Mode exec image names a command %q that cannot be written", c)
		}
	}
	return nil
}

// unsafeDockerfile renders the build: the unsafe base, the runner binary
// copied in (it is static, so it runs on Kali's glibc unchanged), the
// context, one exec-form RUN of the install script, and the runner as the
// entrypoint the tools container is started through. No minimize (D4). No
// value from the index or the extras appears here: the script reads them
// from the context's files.
func unsafeDockerfile(runnerImage string, img spec.ExecImageSpec) string {
	var b strings.Builder
	b.WriteString("# generated by agentry: Unsafe Mode, the declared commands installed from Kali Linux's repository onto the unsafe base; this machine's exec catalogue was not consulted and the image is not minimized (agentry-cli#490, plans/unsafe-mode.md)\n")
	b.WriteString("FROM " + img.Unsafe.Base + "\n")
	b.WriteString("COPY --from=" + runnerImage + " /usr/local/bin/agentry-runner /usr/local/bin/agentry-runner\n")
	b.WriteString("COPY . /agentry-build/\n")
	b.WriteString("ARG AGENTRY_ATTEMPT=0\n")
	b.WriteString("RUN [\"/bin/sh\", \"/agentry-build/install.sh\"]\n")
	b.WriteString("RUN [\"rm\", \"-rf\", \"/agentry-build\"]\n")
	b.WriteString("ENTRYPOINT [\"/usr/local/bin/agentry-runner\"]\n")
	return b.String()
}

// unsafeRecords reads what the build installed and what it did not.
func unsafeRecords(ctx context.Context, d dockerx.Client, tag string) ([]string, error) {
	installed, err := d.ImageFile(ctx, tag, ExecPackagesFile)
	if err != nil {
		return nil, fmt.Errorf("read the Unsafe Mode exec image's package record: %w", err)
	}
	failed, err := d.ImageFile(ctx, tag, ExecFailedFile)
	if err != nil {
		return nil, fmt.Errorf("read the Unsafe Mode exec image's failure record: %w", err)
	}
	var out, notInstalled []string
	for _, line := range strings.Split(string(installed), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	for _, line := range strings.Split(string(failed), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		what, why, _ := strings.Cut(line, "\t")
		notInstalled = append(notInstalled, spec.ExecNotInstalledPrefix+what+" ("+strings.TrimSpace(why)+")")
	}
	sort.Strings(out)
	sort.Strings(notInstalled)
	return append(out, notInstalled...), nil
}
