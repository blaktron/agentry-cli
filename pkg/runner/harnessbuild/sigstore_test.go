package harnessbuild

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
)

// The recorded signature (testdata): codex-0.166.0-x86_64.sigstore is the
// bundle OpenAI published beside codex-x86_64-unknown-linux-musl in the
// rust-v0.166.0 release, and codex0166 the SHA-256 of that binary, read
// from the release's tarball (whose own SHA-256 is a0bf572d…cb3978).
// trusted_root.json is Sigstore's public-good root as its TUF repository
// served it.
const (
	codex0166       = "b8a11fedf24b2dfb59a70c9e52b030f9a508dcd49cd2dd0d41956f4c785156b8"
	codex0166Tag    = "rust-v0.166.0"
	codex0166Commit = "6ff1e1ca3b30fc8b914720681fcf5fe62e95f856"
)

func recordedRoot(t *testing.T) root.TrustedMaterial {
	t.Helper()
	tr, err := root.NewTrustedRootFromPath("testdata/trusted_root.json")
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// OpenAI's signature on the genuine binary verifies at its tag and commit,
// and the same verifier refuses other bytes, another tag's identity and
// another commit, each for that reason.
func TestCodexsSignatureIsHeldToTheReleaseWorkflowAtItsTagAndCommit(t *testing.T) {
	tm := recordedRoot(t)
	raw, err := os.ReadFile("testdata/codex-0.166.0-x86_64.sigstore")
	if err != nil {
		t.Fatal(err)
	}
	sum, _ := hex.DecodeString(codex0166)
	if err := verifyCodex(tm, raw, sum, codex0166Tag, codex0166Commit); err != nil {
		t.Fatalf("the genuine binary: %v", err)
	}
	other := append([]byte(nil), sum...)
	other[0] ^= 0xff
	for _, c := range []struct {
		what        string
		sum         []byte
		tag, commit string
	}{
		{"other bytes", other, codex0166Tag, codex0166Commit},
		{"another tag", sum, "rust-v0.0.0", codex0166Commit},
		{"another commit", sum, codex0166Tag, strings.Repeat("0", 40)},
	} {
		err := verifyCodex(tm, raw, c.sum, c.tag, c.commit)
		if err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s: %v", c.what, err)
		}
	}
	if err := verifyCodex(tm, []byte(`{"base64Signature":""}`), sum, codex0166Tag, codex0166Commit); err == nil {
		t.Error("an empty bundle verified")
	}
}
