package clamdb

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// The identity the bundle must be signed by: agentry-dockerimages'
// clamav-db workflow on main, keyless through GitHub's OIDC issuer
// (agentry-dockerimages#30). A bundle signed by anything else, the files
// workflow included, is refused.
const (
	SignerIdentity = "https://github.com/blaktron/agentry-dockerimages/.github/workflows/clamav-db.yaml@refs/heads/main"
	SignerIssuer   = "https://token.actions.githubusercontent.com"
)

// trustedRoot is Sigstore's public-good trust root (Fulcio's certificates,
// the transparency log's and the timestamp authorities' keys), fetched and
// verified through its TUF repository from the TUF root sigstore-go embeds,
// and cached under dir. A variable so tests use a recorded root.
var trustedRoot = func(ctx context.Context, dir string) (root.TrustedMaterial, error) {
	opts := tuf.DefaultOptions().WithCachePath(dir).WithCacheValidity(1).WithContext(ctx)
	return root.FetchTrustedRootWithOptions(opts)
}

// verifySig is verifySignature, a variable so the refresh's tests run
// without Sigstore; verifySignature's own tests use recorded signatures.
var verifySig = verifySignature

// verifySignature verifies a cosign signature bundle (sigstore bundle
// v0.3, as cosign 3 attaches it) over the manifest digest: the
// certificate chains to Fulcio and names exactly SignerIdentity from
// SignerIssuer, the signature is in the transparency log, its time is
// observed, and the signed statement's subject is this digest.
func verifySignature(tm root.TrustedMaterial, raw []byte, digest string) error {
	var b bundle.Bundle
	if err := b.UnmarshalJSON(raw); err != nil {
		return fmt.Errorf("the signature bundle: %w", err)
	}
	v, err := verify.NewVerifier(tm, verify.WithSignedCertificateTimestamps(1), verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return err
	}
	id, err := verify.NewShortCertificateIdentity(SignerIssuer, "", SignerIdentity, "")
	if err != nil {
		return err
	}
	sum, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	if err != nil {
		return err
	}
	if _, err := v.Verify(&b, verify.NewPolicy(verify.WithArtifactDigest("sha256", sum), verify.WithCertificateIdentity(id))); err != nil {
		return fmt.Errorf("the signature does not verify as %s: %w", SignerIdentity, err)
	}
	return nil
}
