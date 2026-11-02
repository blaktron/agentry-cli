package harnessbuild

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"time"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/theupdateframework/go-tuf/v2/metadata/fetcher"
)

// The identity OpenAI's Codex release binaries are signed by: the
// rust-release workflow of openai/codex at the release's tag, from a push of
// that tag, through GitHub's OIDC issuer.
const (
	codexRepository = "openai/codex"
	codexWorkflow   = "https://github.com/openai/codex/.github/workflows/rust-release.yml"
	githubIssuer    = "https://token.actions.githubusercontent.com"
)

// trustedRoot is Sigstore's public-good trust root, fetched and verified
// through its TUF repository from the TUF root sigstore-go embeds, and
// cached under dir. A variable so tests use a recorded root.
var trustedRoot = func(ctx context.Context, dir string) (root.TrustedMaterial, error) {
	f := fetcher.NewDefaultFetcher()
	f.SetHTTPClient(&http.Client{Timeout: time.Minute})
	opts := tuf.DefaultOptions().WithCachePath(dir).WithCacheValidity(1).WithContext(ctx).WithFetcher(f)
	return root.FetchTrustedRootWithOptions(opts)
}

// cosignBundle is the bundle OpenAI publishes beside each release binary
// (codex-<triple>.sigstore): cosign's own format, a signature over the
// binary, the Fulcio certificate that signed it, and the transparency log's
// promise of the entry.
type cosignBundle struct {
	Signature   string `json:"base64Signature"`
	Cert        string `json:"cert"`
	RekorBundle struct {
		SignedEntryTimestamp string `json:"SignedEntryTimestamp"`
		Payload              struct {
			Body           string `json:"body"`
			IntegratedTime int64  `json:"integratedTime"`
			LogIndex       int64  `json:"logIndex"`
			LogID          string `json:"logID"`
		} `json:"Payload"`
	} `json:"rekorBundle"`
}

// sigstoreBundle reads raw, a cosign bundle, as the Sigstore bundle
// (v0.1) that holds the same signature, certificate and log entry, over
// the artifact whose SHA-256 is sum.
func sigstoreBundle(raw, sum []byte) (*bundle.Bundle, error) {
	var c cosignBundle
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("the signature bundle: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(c.Signature)
	if err != nil || len(sig) == 0 {
		return nil, errors.New("the signature bundle has no signature")
	}
	pemCert, err := base64.StdEncoding.DecodeString(c.Cert)
	if err != nil {
		return nil, errors.New("the signature bundle's certificate is not base64")
	}
	block, _ := pem.Decode(pemCert)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("the signature bundle's certificate is not a PEM certificate")
	}
	p := c.RekorBundle.Payload
	set, err := base64.StdEncoding.DecodeString(c.RekorBundle.SignedEntryTimestamp)
	if err != nil || len(set) == 0 {
		return nil, errors.New("the signature bundle has no transparency log promise")
	}
	body, err := base64.StdEncoding.DecodeString(p.Body)
	if err != nil {
		return nil, errors.New("the signature bundle's log entry is not base64")
	}
	logID, err := hex.DecodeString(p.LogID)
	if err != nil {
		return nil, errors.New("the signature bundle's log id is not hex")
	}
	var entry struct {
		Kind       string `json:"kind"`
		APIVersion string `json:"apiVersion"`
	}
	if err := json.Unmarshal(body, &entry); err != nil || entry.Kind == "" || entry.APIVersion == "" {
		return nil, errors.New("the signature bundle's log entry names no kind")
	}
	pb := &protobundle.Bundle{
		MediaType: "application/vnd.dev.sigstore.bundle+json;version=0.1",
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_X509CertificateChain{
				X509CertificateChain: &protocommon.X509CertificateChain{
					Certificates: []*protocommon.X509Certificate{{RawBytes: block.Bytes}},
				},
			},
			TlogEntries: []*protorekor.TransparencyLogEntry{{
				LogIndex:          p.LogIndex,
				LogId:             &protocommon.LogId{KeyId: logID},
				KindVersion:       &protorekor.KindVersion{Kind: entry.Kind, Version: entry.APIVersion},
				IntegratedTime:    p.IntegratedTime,
				InclusionPromise:  &protorekor.InclusionPromise{SignedEntryTimestamp: set},
				CanonicalizedBody: body,
			}},
		},
		Content: &protobundle.Bundle_MessageSignature{
			MessageSignature: &protocommon.MessageSignature{
				MessageDigest: &protocommon.HashOutput{Algorithm: protocommon.HashAlgorithm_SHA2_256, Digest: sum},
				Signature:     sig,
			},
		},
	}
	return bundle.NewBundle(pb)
}

// verifyCodex verifies raw, the cosign bundle published beside a Codex
// release binary whose SHA-256 is sum, as OpenAI's rust-release workflow at
// tag and commit: the certificate chains to Fulcio, carries its timestamps
// and names that workflow at refs/tags/<tag>, from a push of that tag, of
// openai/codex, at that commit; the signature is in the transparency log;
// and it is over these bytes.
func verifyCodex(tm root.TrustedMaterial, raw, sum []byte, tag, commit string) error {
	b, err := sigstoreBundle(raw, sum)
	if err != nil {
		return err
	}
	v, err := verify.NewVerifier(tm, verify.WithSignedCertificateTimestamps(1), verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return err
	}
	ref := "refs/tags/" + tag
	san, err := verify.NewSANMatcher(codexWorkflow+"@"+ref, "")
	if err != nil {
		return err
	}
	issuer, err := verify.NewIssuerMatcher(githubIssuer, "")
	if err != nil {
		return err
	}
	id, err := verify.NewCertificateIdentity(san, issuer, certificate.Extensions{
		GithubWorkflowTrigger:    "push",
		GithubWorkflowSHA:        commit,
		GithubWorkflowRepository: codexRepository,
		GithubWorkflowRef:        ref,
	})
	if err != nil {
		return err
	}
	if _, err := v.Verify(b, verify.NewPolicy(verify.WithArtifactDigest("sha256", sum), verify.WithCertificateIdentity(id))); err != nil {
		return fmt.Errorf("refused: Codex %s's binary does not verify as OpenAI's release workflow at that tag (%s): %w", tag, commit, err)
	}
	return nil
}
