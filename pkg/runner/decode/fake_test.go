package decode

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/clamdb"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

// ScriptFake lets a run with inputs pass through the real decode step on a
// dockerx.Fake: Ensure finds the image and builds and pulls nothing, the
// package record is read back, and the real check takes the container's
// record and report, with and without signatures, leaving each staged file
// as given and not read.
func TestScriptFakePlaysTheDecodeStep(t *testing.T) {
	for _, signed := range []bool{false, true} {
		f := &dockerx.Fake{}
		ScriptFake(f, "agentry-runner:abc", testFiles)
		img, err := Ensure(context.Background(), f, "agentry-runner:abc", ensurePolicy(), t.TempDir(), io.Discard)
		if err != nil {
			t.Fatalf("signed=%t: Ensure: %v", signed, err)
		}
		if b, p := callsOf(f, "Build"), callsOf(f, "Pull"); len(b) != 0 || len(p) != 0 {
			t.Fatalf("signed=%t: Ensure built %v and pulled %v, want nothing", signed, b, p)
		}
		if len(img.Packages) != 1 || img.Packages[0] != "pandoc-cli-3.6.4-r0" {
			t.Fatalf("signed=%t: the package record read back as %v", signed, img.Packages)
		}
		if signed {
			img.Signatures = clamdb.DB{Dir: "/stand-in/clamav", Ref: policy.DefaultClamAVSignatures, Digest: "sha256:84cbc016d58b"}
		}
		in := t.TempDir()
		if err := os.WriteFile(filepath.Join(in, "notes.txt"), []byte("hello\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		res, err := Run(context.Background(), f, img, in, t.TempDir(), policy.Limits{}.WithDefaults(), io.Discard)
		if err != nil {
			t.Fatalf("signed=%t: the decode on the fake: %v", signed, err)
		}
		if res.Refused() || len(res.Found()) != 0 {
			t.Fatalf("signed=%t: the fake's decode refused or found something: %+v", signed, res.Report)
		}
		var got *runnerdecode.File
		for i := range res.Report.Files {
			if res.Report.Files[i].Path == "notes.txt" {
				got = &res.Report.Files[i]
			}
		}
		if got == nil || got.HasText() {
			t.Fatalf("signed=%t: notes.txt in the report: %+v, want it given and not read", signed, res.Report.Files)
		}
	}
}
