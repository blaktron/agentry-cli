package harnessbuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/sigstore/sigstore-go/pkg/root"
)

// npmSigner stands in for the npm registry's signing key: the build's keys
// are swapped for it, so the fake registry's manifests carry signatures
// the build accepts.
type npmSigner struct {
	key *ecdsa.PrivateKey
	id  string
}

func newNPMSigner(t *testing.T) *npmSigner {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	s := &npmSigner{key: k, id: "SHA256:test-key"}
	saved := npmKeys
	npmKeys = []npmKey{{id: s.id, der: base64.StdEncoding.EncodeToString(der)}}
	t.Cleanup(func() { npmKeys = saved })
	return s
}

func (s *npmSigner) sign(t *testing.T, name, version, integrity string) []npmSignature {
	t.Helper()
	sum := sha256.Sum256([]byte(name + "@" + version + ":" + integrity))
	sig, err := ecdsa.SignASN1(rand.Reader, s.key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return []npmSignature{{KeyID: s.id, Sig: base64.StdEncoding.EncodeToString(sig)}}
}

// publishLatest makes the fake registry answer 9.9.9 as Claude Code's
// latest, each manifest signed by s over the integrity of what the registry
// serves; edit changes the wrapper's manifest before it is served.
func (r *registry) publishLatest(t *testing.T, s *npmSigner, edit func(*npmManifest)) {
	t.Helper()
	wrapperInt := integrity(r.pkgs["/@anthropic-ai/claude-code/-/claude-code-9.9.9.tgz"])
	nativeName := "@anthropic-ai/claude-code-linux-x64-musl"
	nativeInt := integrity(r.pkgs["/@anthropic-ai/claude-code-linux-x64-musl/-/claude-code-linux-x64-musl-9.9.9.tgz"])
	w := npmManifest{Name: claudeWrapper, Version: "9.9.9", Optional: map[string]string{nativeName: "9.9.9"}}
	w.Dist.Integrity, w.Dist.Signatures = wrapperInt, s.sign(t, claudeWrapper, "9.9.9", wrapperInt)
	n := npmManifest{Name: nativeName, Version: "9.9.9"}
	n.Dist.Integrity, n.Dist.Signatures = nativeInt, s.sign(t, nativeName, "9.9.9", nativeInt)
	if edit != nil {
		edit(&w)
	}
	wb, _ := json.Marshal(w)
	nb, _ := json.Marshal(n)
	r.mu.Lock()
	r.pkgs["/"+claudeWrapper+"/latest"] = wb
	r.pkgs["/"+nativeName+"/9.9.9"] = nb
	r.mu.Unlock()
}

// The registry's latest Claude Code, newer than the tested one, is built
// from its two packages held to the integrities the registry signed, and
// its record builds the same image again once it is gone.
func TestTheNewestClaudeCodeIsBuiltFromWhatTheRegistrySigned(t *testing.T) {
	reg := newRegistry(t)
	reg.publishLatest(t, newNPMSigner(t), nil)
	d := &dockerx.Fake{Arch: "amd64"}
	var told []string
	b := Builder{Docker: d, Registry: reg.URL, Dir: t.TempDir(), Building: func(v string) { told = append(told, v) }}
	n, newer, err := b.AskNewest(context.Background(), ClaudeImageAdapter)
	if err != nil || !newer || n.Version != "9.9.9" || n.Tested != TestedVersion(ClaudeImageAdapter) {
		t.Fatalf("AskNewest = %+v, %v, %v", n, newer, err)
	}
	h, rec, err := b.BuildNewest(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	want := policy.HarnessImage{Ref: "agentry-claude-code:9.9.9", Artifact: integrity(reg.pkgs["/@anthropic-ai/claude-code/-/claude-code-9.9.9.tgz"])}
	if h != want || rec.Ref != h.Ref || rec.Artifact != h.Artifact || rec.Version != "9.9.9" {
		t.Fatalf("built %+v, recorded %+v", h, rec)
	}
	if got := builds(d); len(got) != 1 || len(told) != 1 || told[0] != "9.9.9" {
		t.Fatalf("builds %v, told %v", got, told)
	}
	if note := PinNote(ClaudeImageAdapter, h); !strings.Contains(note, "newest release, 9.9.9") || !strings.Contains(note, TestedVersion(ClaudeImageAdapter)) {
		t.Fatalf("the receipt's note %q", note)
	}

	// Without the record, a build of this agentry knows no such release.
	d2 := &dockerx.Fake{Arch: "amd64"}
	if err := (Builder{Docker: d2, Registry: reg.URL, Dir: t.TempDir()}).Ensure(context.Background(), ClaudeImageAdapter, h); err == nil {
		t.Fatal("an unrecorded newest release was built")
	}
	if err := (Builder{Docker: d2, Registry: reg.URL, Dir: t.TempDir(), Recorded: &rec}).Ensure(context.Background(), ClaudeImageAdapter, h); err != nil {
		t.Fatal(err)
	}
	if got := builds(d2); len(got) != 1 {
		t.Fatalf("rebuilt from the record: %v", got)
	}
}

// A newest release the registry did not sign, or whose wrapper installs
// another native package version, is refused before anything is
// downloaded.
func TestANewestClaudeCodeTheRegistryDidNotSignIsRefused(t *testing.T) {
	for name, edit := range map[string]func(*npmManifest){
		"another integrity": func(m *npmManifest) { m.Dist.Integrity = integrity([]byte("other")) },
		"no signature":      func(m *npmManifest) { m.Dist.Signatures = nil },
		"an unknown key":    func(m *npmManifest) { m.Dist.Signatures[0].KeyID = "SHA256:other" },
		"another native":    func(m *npmManifest) { m.Optional["@anthropic-ai/claude-code-linux-x64-musl"] = "9.9.8" },
		"not a release":     func(m *npmManifest) { m.Version = "9.9.9-beta.1" },
		"another package":   func(m *npmManifest) { m.Name = "@anthropic-ai/other" },
	} {
		reg := newRegistry(t)
		reg.publishLatest(t, newNPMSigner(t), edit)
		d := &dockerx.Fake{Arch: "amd64"}
		if _, _, err := (Builder{Docker: d, Registry: reg.URL, Dir: t.TempDir()}).AskNewest(context.Background(), ClaudeImageAdapter); err == nil {
			t.Errorf("%s: accepted", name)
		}
		for _, p := range reg.asked {
			if strings.HasSuffix(p, ".tgz") {
				t.Errorf("%s: downloaded %s", name, p)
			}
		}
		if len(builds(d)) != 0 {
			t.Errorf("%s: built", name)
		}
	}
}

// The registry's own key verifies a real signature: Claude Code 2.1.316's
// wrapper as the registry served it on 2026-11-02.
func TestTheRegistrysKeyVerifiesARealSignature(t *testing.T) {
	const i = "sha512-i3BpzyDkcCPnsa0CSkWgxGPs7WMsyqDkxkqT0SDxyEC5XMIxEmcUmluWRHG9VSeBY8Vyv+l9X34JvJ/Bj54nBQ=="
	sigs := []npmSignature{{KeyID: "SHA256:DhQ8wR5APBvFHLF/+Tc+AYvPOdTpcIDqOhxsBHRwC7U", Sig: "MEUCICXEbuxU4HVm4rtVjal/3VZHOW7rjw6/ISFC8P/ddyBJAiEAqQFbe9GS8tKqXjZ+0qtKoHHMrs+xdjpjUZ2mOkEhNf0="}}
	if err := checkNPMSignature(npmKeys, claudeWrapper, "2.1.316", i, sigs); err != nil {
		t.Fatal(err)
	}
	if err := checkNPMSignature(npmKeys, claudeWrapper, "2.1.317", i, sigs); err == nil {
		t.Fatal("the signature verified for another version")
	}
}

// GitHub's newest Codex release is read with the commit its annotated tag
// names; a prerelease, a tag of another shape or a tag naming no commit is
// refused.
func TestTheNewestCodexIsReadWithItsTagsCommit(t *testing.T) {
	const commit = "6ff1e1ca3b30fc8b914720681fcf5fe62e95f856"
	for _, c := range []struct {
		name, release, ref, tag string
		ok                      bool
	}{
		{"annotated", `{"tag_name":"rust-v9.9.9"}`, `{"object":{"type":"tag","sha":"aaaa"}}`, `{"object":{"type":"commit","sha":"` + commit + `"}}`, true},
		{"lightweight", `{"tag_name":"rust-v9.9.9"}`, `{"object":{"type":"commit","sha":"` + commit + `"}}`, ``, true},
		{"prerelease", `{"tag_name":"rust-v9.9.9","prerelease":true}`, ``, ``, false},
		{"another tag", `{"tag_name":"v9.9.9"}`, ``, ``, false},
		{"no commit", `{"tag_name":"rust-v9.9.9"}`, `{"object":{"type":"tree","sha":"` + commit + `"}}`, ``, false},
	} {
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/releases/latest":
				_, _ = w.Write([]byte(c.release))
			case "/git/ref/tags/rust-v9.9.9":
				_, _ = w.Write([]byte(c.ref))
			case "/git/tags/aaaa":
				_, _ = w.Write([]byte(c.tag))
			default:
				http.NotFound(w, r)
			}
		}))
		rel, err := (Builder{Docker: &dockerx.Fake{}, CodexAPI: api.URL, Dir: t.TempDir()}).newestCodex(context.Background())
		api.Close()
		if c.ok != (err == nil) {
			t.Errorf("%s: %+v, %v", c.name, rel, err)
			continue
		}
		if c.ok && (rel.Version != "9.9.9" || rel.Tag != "rust-v9.9.9" || rel.Commit != commit) {
			t.Errorf("%s: %+v", c.name, rel)
		}
	}
}

// A Codex binary whose signature bundle does not verify is refused, and
// nothing reaches the daemon.
func TestACodexBinaryWithoutOpenAIsSignatureIsNeverBuilt(t *testing.T) {
	bin := []byte("not codex")
	var tarball strings.Builder
	tarball.Write(tarOne(t, "codex-x86_64-unknown-linux-musl", bin))
	dl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".tar.gz"):
			_, _ = w.Write([]byte(tarball.String()))
		case strings.HasSuffix(r.URL.Path, ".sigstore"):
			raw, _ := readTestdata(t, "codex-0.166.0-x86_64.sigstore")
			_, _ = w.Write(raw)
		default:
			_, _ = w.Write([]byte("licence"))
		}
	}))
	defer dl.Close()
	saved := trustedRoot
	trustedRoot = func(context.Context, string) (rootMaterial, error) { return recordedRoot(t), nil }
	t.Cleanup(func() { trustedRoot = saved })
	d := &dockerx.Fake{Arch: "amd64"}
	_, _, err := (Builder{Docker: d, CodexReleases: dl.URL, CodexFiles: dl.URL, Dir: t.TempDir()}).buildCodex(context.Background(),
		codexRelease{Version: "0.166.0", Tag: codex0166Tag, Commit: codex0166Commit})
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("built a binary OpenAI did not sign: %v", err)
	}
	if len(builds(d)) != 0 {
		t.Fatalf("built %v", builds(d))
	}
}

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"2.1.316", "2.1.296", true}, {"2.1.296", "2.1.296", false}, {"2.1.29", "2.1.296", false},
		{"0.167.0", "0.166.9", true}, {"1.0.0", "0.999.999", true}, {"2.1", "2.0.0", false}, {"x.1.2", "0.0.0", false},
	} {
		if got := newerVersion(c.a, c.b); got != c.want {
			t.Errorf("newerVersion(%s, %s) = %v", c.a, c.b, got)
		}
	}
	if TestedVersion(CodexImageAdapter) != "0.166.0" && !strings.HasPrefix(policy.DefaultHarnessImages[CodexImageAdapter].Ref, "ghcr.io/blaktron/agentry-codex:"+TestedVersion(CodexImageAdapter)+"-") {
		t.Fatalf("the tested Codex version %q is not the pinned image's", TestedVersion(CodexImageAdapter))
	}
}

// rootMaterial is the trust root trustedRoot returns.
type rootMaterial = root.TrustedMaterial

func readTestdata(t *testing.T, name string) ([]byte, error) {
	t.Helper()
	return os.ReadFile(filepath.Join("testdata", name))
}

// tarOne is a gzipped tar holding one file.
func tarOne(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}
