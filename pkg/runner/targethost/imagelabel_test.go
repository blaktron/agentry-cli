package targethost

import (
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/imagelabel"
)

// The host link's image carries its kind.
func TestHostLinkImageCarriesTheImageLabel(t *testing.T) {
	df := HostLinkImage("agentry-runner:v1", "ghcr.io/blaktron/agentry-hostlink:1@sha256:00").Dockerfile()
	if err := imagelabel.Check(df, imagelabel.HostLink); err != nil {
		t.Errorf("%v\n%s", err, df)
	}
}
