package deliver

import (
	"os"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// A copy carries the Mark-of-the-Web, so Explorer and Office treat it as a
// download.
func TestACopyCarriesTheMarkOfTheWeb(t *testing.T) {
	dir, files := runFolder(t, map[string]string{"reports/a.md": "a"}, "ok")
	got := ToFolder(dir, files, opts(t.TempDir()))
	if got[0].Status != receipt.DeliveryDelivered || got[0].Error != "" {
		t.Fatalf("delivery = %+v", got[0])
	}
	b, err := os.ReadFile(got[0].Object + ":Zone.Identifier")
	if err != nil {
		t.Fatalf("the copy has no Zone.Identifier stream: %v", err)
	}
	if string(b) != zoneIdentifier {
		t.Errorf("stream = %q", b)
	}
}
