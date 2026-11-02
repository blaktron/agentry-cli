package harnessbuild

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
)

// npmKey is one of the npm registry's signing keys, as
// https://registry.npmjs.org/-/npm/v1/keys publishes it: the registry
// signs "<name>@<version>:<integrity>" of every version it serves with
// ECDSA P-256 over SHA-256 (dist.signatures).
type npmKey struct {
	id  string
	der string
}

// npmKeys are the registry keys a newest release's signature is held to.
// The key that expired on 2025-01-29 is not carried: a release newer than
// the one this build pins is signed by the current key. A rotation by npm
// is a new build of agentry; until then a newest release signed only by
// an unknown key is refused, and the tested pin is built.
var npmKeys = []npmKey{{
	id:  "SHA256:DhQ8wR5APBvFHLF/+Tc+AYvPOdTpcIDqOhxsBHRwC7U",
	der: "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEY6Ya7W++7aUPzvMTrezH6Ycx3c+HOKYCcNGybJZSCJq/fd7Qa8uuAKtdIkUQtQiEKERhAmE5lMMJhP8OkDOa2g==",
}}

// npmSignature is one entry of a version's dist.signatures.
type npmSignature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

// checkNPMSignature holds name@version's integrity to the registry's
// signature: at least one of sigs must be a valid signature over
// "<name>@<version>:<integrity>" by a key in keys.
func checkNPMSignature(keys []npmKey, name, version, integrity string, sigs []npmSignature) error {
	msg := sha256.Sum256([]byte(name + "@" + version + ":" + integrity))
	for _, s := range sigs {
		for _, k := range keys {
			if s.KeyID != k.id {
				continue
			}
			der, err := base64.StdEncoding.DecodeString(k.der)
			if err != nil {
				return err
			}
			pub, err := x509.ParsePKIXPublicKey(der)
			if err != nil {
				return err
			}
			ec, ok := pub.(*ecdsa.PublicKey)
			if !ok {
				return fmt.Errorf("npm key %s is not an ECDSA key", k.id)
			}
			sig, err := base64.StdEncoding.DecodeString(s.Sig)
			if err == nil && ecdsa.VerifyASN1(ec, msg[:], sig) {
				return nil
			}
		}
	}
	return fmt.Errorf("refused: the registry's %s@%s carries no signature by an npm registry key this build of agentry knows over its integrity %s", name, version, integrity)
}
