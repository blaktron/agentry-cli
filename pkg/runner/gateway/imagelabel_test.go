package gateway

import (
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/imagelabel"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// The exec and Unsafe Mode exec images carry their own kind, over the
// runner kind an exec image would otherwise inherit.
func TestExecImagesCarryTheImageLabel(t *testing.T) {
	for _, pkgs := range [][]string{nil, {"busybox", "git"}} {
		df := execDockerfile("agentry-runner:v1", spec.ExecImageSpec{Commands: []string{"tar"}, Packages: pkgs, APKMirror: "https://mirror.example/alpine"})
		if err := imagelabel.Check(df, imagelabel.Exec); err != nil {
			t.Errorf("exec %v: %v\n%s", pkgs, err, df)
		}
	}
	df := unsafeDockerfile("agentry-runner:v1", unsafeImage())
	if err := imagelabel.Check(df, imagelabel.UnsafeExec); err != nil {
		t.Errorf("unsafe exec: %v\n%s", err, df)
	}
}
