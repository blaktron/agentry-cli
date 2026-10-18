package toolsrv

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// brotliFixtures are the Brotli encodings, from a reference encoder at its
// highest quality, of the few bodies the fetch tests send in that coding,
// keyed by the SHA-256 of the body they decode to. The runner image carries
// a Brotli decoder and no encoder, so the tests carry the encoded bytes.
var brotliFixtures = map[string]string{
	"91c91c63b9a30d9a9d5feef28da904573b664b41c52309b3e2f1c63feb14fc31": "GzEA+EXXRqkSyu5A/xaVzWgXsggvE2kbCKFEGvyjn04RFQx4lNr6mGufm9QzAQ==",
	"26a13ac9cb8ca82685ce2eaba2d83dd4d4a9705aa0ded95494ac0a5743a4356c": "G0Uc+I2UqONbNjfqlmQobPh8LSociRGMtFyElw9zgRcqCUJYoTTUe54AIC619THXPveJwpK542cPAA==",
	"2b7390ff56e3c72d9aa9b8ed3aa287e7227016a65cf4343c0d37924152e7c6aa": "G6Io+MVtrJqd5b4SjkAsTQ6MBt4EAkCcxcAfreoB",
	"439292e489a0a35e4a3a0fe304ea1a680337243fa53b135aa9310881e1d7e078": "m///E/gnAOKxQCD3fgI=",
}

// brotliOf is body in Brotli, from brotliFixtures; a body without a fixture
// fails the test.
func brotliOf(t *testing.T, body []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(body)
	enc, ok := brotliFixtures[hex.EncodeToString(sum[:])]
	if !ok {
		t.Fatalf("no Brotli fixture for a body of %d bytes (sha256 %x); add its encoding to brotliFixtures", len(body), sum)
	}
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
