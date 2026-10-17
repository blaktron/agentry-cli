package deliver

import (
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// A copy carries the quarantine attribute, so Gatekeeper treats it as a
// download.
func TestACopyIsQuarantined(t *testing.T) {
	dir, files := runFolder(t, map[string]string{"reports/a.md": "a"}, "ok")
	got := ToFolder(dir, files, opts(t.TempDir()))
	if got[0].Status != receipt.DeliveryDelivered || got[0].Error != "" {
		t.Fatalf("delivery = %+v", got[0])
	}
	buf := make([]byte, 128)
	n, err := unix.Getxattr(got[0].Object, "com.apple.quarantine", buf)
	if err != nil {
		t.Fatalf("the copy has no quarantine attribute: %v", err)
	}
	if v := string(buf[:n]); !strings.HasPrefix(v, "0081;") || !strings.HasSuffix(v, ";Agentry;") {
		t.Errorf("quarantine = %q", v)
	}
}
