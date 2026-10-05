package decode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/clamdb"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

// eicarBytes is the EICAR anti-virus test file, assembled from two halves
// so this source is not itself the test file to a scanner.
func eicarBytes() []byte {
	return []byte(`X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STAN` + `DARD-ANTIVIRUS-TEST-FILE!$H+H*`)
}

// TestClamAVForReal checks the malware check end to end on this machine:
// the signatures refreshed from the published bundle (GHCR, its signature
// verified against the clamav-db workflow through Sigstore's TUF root), the
// pinned files image's clamscan in the decode container with no network,
// over the EICAR file as given, inside a zip and nested in a tar.gz inside a
// zip, beside the Creator's corpus, which must all pass clean. It needs a
// docker daemon and the network, so it runs only when AGENTRY_DECODE_E2E=1.
func TestClamAVForReal(t *testing.T) {
	if os.Getenv("AGENTRY_DECODE_E2E") != "1" {
		t.Skip("set AGENTRY_DECODE_E2E=1 to fetch the signatures and check files with the real clamscan")
	}
	ctx := context.Background()
	d := dockerx.New()
	pol := policy.Default()
	home := t.TempDir()
	tag := pol.RunnerImageTag(runnerimage.ContentVersion())
	if err := gateway.EnsureInstalled(ctx, d, pol, tag, filepath.Join(home, "image"), os.Stderr, false); err != nil {
		t.Fatal(err)
	}
	img, err := Ensure(ctx, d, tag, pol, filepath.Join(home, "image"), os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	img.Signatures = clamdb.Ensure(ctx, home, pol.Images.ClamAVSignaturesRef(), os.Stderr)
	if img.Signatures.Dir == "" || img.Signatures.Problem != "" {
		t.Fatalf("no signatures: %+v", img.Signatures)
	}
	t.Logf("signatures: %s@%s, daily %d of %s", img.Signatures.Ref, img.Signatures.Digest, img.Signatures.Daily.Number, img.Signatures.Daily.Built)

	in, out := t.TempDir(), t.TempDir()
	if err := os.CopyFS(in, os.DirFS(filepath.Join("..", "..", "knowledge", "creator", "testdata", "corpus"))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in, "eicar.com"), eicarBytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	inner := gzipped(t, tarred(t, map[string][]byte{"deep/eicar.txt": eicarBytes()}))
	for name, data := range map[string][]byte{
		"bundle.zip": zipped(t, zipMember{name: "docs/eicar.txt", data: eicarBytes()}, zipMember{name: "docs/clean.md", data: []byte("# clean\n")}),
		"nested.zip": zipped(t, zipMember{name: "inner.tar.gz", data: inner}),
	} {
		if err := os.WriteFile(filepath.Join(in, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res, err := Run(ctx, d, img, in, out, pol.Docker.Limits.WithDefaults(), os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]runnerdecode.File{}
	for _, f := range res.Found() {
		found[f.Path] = f
	}
	for _, path := range []string{"eicar.com", "bundle.zip/docs/eicar.txt", "nested.zip/inner.tar.gz/deep/eicar.txt"} {
		f, ok := found[path]
		if !ok {
			t.Errorf("%s was not found", path)
			continue
		}
		if !strings.Contains(f.Signature, "Eicar") || f.Outcome != runnerdecode.OutcomeRefused || f.Output != "" {
			t.Errorf("%s = %+v", path, f)
		}
	}
	// clamscan reads into the archives too, so they are found themselves.
	for _, path := range []string{"bundle.zip", "nested.zip"} {
		if _, ok := found[path]; !ok {
			t.Errorf("clamscan's own unpacking did not find %s", path)
		}
	}
	for _, f := range res.Report.Files {
		if strings.Contains(f.Path, "eicar") || f.Path == "bundle.zip" || f.Path == "nested.zip" || strings.HasPrefix(f.Path, "nested.zip/") {
			continue
		}
		if f.Check != runnerdecode.CheckClean {
			t.Errorf("%s = check %q (%q), want clean", f.Path, f.Check, f.Unchecked)
		}
	}
	if got := res.Refusal(); !strings.HasPrefix(got, "ClamAV found malware in ") {
		t.Errorf("refusal %q", got)
	}
	t.Logf("receipt: %s", res.Checks())
	t.Logf("refusal: %s", res.Refusal())
}
