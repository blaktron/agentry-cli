package clamdb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Version is what a CVD's header says of it: its version and when Talos
// built it.
type Version struct {
	Number int       `json:"number"`
	Built  time.Time `json:"built"`
}

// cvdHeaderSize is a CVD's header, "ClamAV-VDB:<time>:<version>:..." padded
// to 512 bytes, before its signed archive.
const cvdHeaderSize = 512

// cvdTimeLayout is the header's build time, e.g. "04 Oct 2026 06-25 +0000".
const cvdTimeLayout = "02 Jan 2006 15-04 -0700"

// readCVDHeader reads the version and build time from a CVD's header. The
// signed archive behind it is clamscan's to check when it loads the file;
// the header only dates the signatures for the receipt and orders bundles.
func readCVDHeader(path string) (Version, error) {
	f, err := os.Open(path) //nolint:gosec // a database this package wrote
	if err != nil {
		return Version{}, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, cvdHeaderSize)
	if _, err := io.ReadFull(f, buf); err != nil {
		return Version{}, fmt.Errorf("%s: no CVD header: %w", path, err)
	}
	return parseCVDHeader(buf)
}

func parseCVDHeader(buf []byte) (Version, error) {
	h := string(bytes.TrimRight(buf, "\x00 "))
	if !strings.HasPrefix(h, "ClamAV-VDB:") {
		return Version{}, errors.New("not a ClamAV CVD header")
	}
	fields := strings.Split(h, ":")
	if len(fields) < 5 {
		return Version{}, errors.New("a CVD header with too few fields")
	}
	built, err := time.Parse(cvdTimeLayout, fields[1])
	if err != nil {
		return Version{}, fmt.Errorf("a CVD header's build time %q: %w", fields[1], err)
	}
	n, err := strconv.Atoi(fields[2])
	if err != nil || n <= 0 {
		return Version{}, fmt.Errorf("a CVD header's version %q is not a positive number", fields[2])
	}
	return Version{Number: n, Built: built.UTC()}, nil
}
