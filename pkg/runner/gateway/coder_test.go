package gateway

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/imagelabel"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

func TestCoderDockerfileIsTheToolchainWithTheRunner(t *testing.T) {
	base := "ghcr.io/blaktron/agentry-coder:1@sha256:" + strings.Repeat("a", 64)
	img := spec.ExecImageSpec{Tag: spec.CoderExecImageTag("agentry-runner:1", base), Coder: base}
	df := coderDockerfile("agentry-runner:1", img)
	for _, want := range []string{"FROM " + base + "\n", "COPY --from=agentry-runner:1 /usr/local/bin/agentry-runner ", "USER 0:0\n", `ENTRYPOINT ["/usr/local/bin/agentry-runner"]`} {
		if !strings.Contains(df, want) {
			t.Errorf("the Dockerfile lacks %q:\n%s", want, df)
		}
	}
	if strings.Contains(df, "RUN") {
		t.Errorf("the Coding agent's exec image installs or minimizes nothing:\n%s", df)
	}
	if err := imagelabel.Check(df, imagelabel.CoderExec); err != nil {
		t.Error(err)
	}
	if !strings.Contains(img.Tag, "-exec:1-c") || img.Tag == spec.CoderExecImageTag("agentry-runner:1", base[:len(base)-1]+"b") {
		t.Errorf("the tag %q is not a coder tag keyed by its base", img.Tag)
	}
}
