package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// GVisorRelease is the gVisor release a runhost runs, the one Agentry's own
// runhosts pin. Each architecture's archive is pinned by its SHA-512, so a
// changed archive is refused whatever the download says of itself.
const GVisorRelease = "20261102.0"

var gvisorSHA512 = map[string]string{
	"x86_64":  "3c400f72be459b57b99b1c4f69be96bcd67eef3caf2078e595fc98e688b6b9a3af7802646b3dde7da8a9aba96d34f86be303e0430c7868ab2d13becdd403c0c1",
	"aarch64": "e829ca32eef36f4e960460a6dcb094d7945ab507e9bfe0275f09467ab9bfeb846528fddfd42349ad64bb4f5b02148e1065c06e573feb8f6a8b3f47efa30a7d2e",
}

// GVisorBaseURL is gVisor's release bucket.
var GVisorBaseURL = "https://storage.googleapis.com/gvisor/releases/release"

// maxGVisor bounds the archive and what it unpacks to.
const maxGVisor = 512 << 20

// FetchGVisor downloads gVisor's release archive for a server architecture
// (uname -m: x86_64, aarch64), checks it against the pinned SHA-512, and
// repacks runsc and its helper binaries as a gzip tar, which every server can
// unpack. The containerd shim is left out: Docker runs runsc directly.
func FetchGVisor(ctx context.Context, client *http.Client, arch string) ([]byte, error) {
	archive, err := FetchGVisorArchive(ctx, client, arch)
	if err != nil {
		return nil, err
	}
	return RepackGVisor(archive)
}

// FetchGVisorArchive downloads gVisor's release archive for a server
// architecture and checks it against the pinned SHA-512 (CheckGVisor). It is
// the archive as gVisor publishes it, which a runhost checks again.
func FetchGVisorArchive(ctx context.Context, client *http.Client, arch string) ([]byte, error) {
	if _, ok := gvisorSHA512[arch]; !ok {
		return nil, fmt.Errorf("no gVisor release is pinned for %s", arch)
	}
	u := GVisorBaseURL + "/" + GVisorRelease + "/" + arch + "/gvisor.tar.zstd"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download gVisor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download gVisor %s: %s", u, resp.Status)
	}
	archive, err := io.ReadAll(io.LimitReader(resp.Body, maxGVisor+1))
	if err != nil {
		return nil, fmt.Errorf("download gVisor: %w", err)
	}
	if err := CheckGVisor(arch, archive); err != nil {
		return nil, err
	}
	return archive, nil
}

// CheckGVisor checks that archive is the pinned GVisorRelease's archive for
// arch (uname -m), by its SHA-512.
func CheckGVisor(arch string, archive []byte) error {
	want, ok := gvisorSHA512[arch]
	if !ok {
		return fmt.Errorf("no gVisor release is pinned for %s", arch)
	}
	if len(archive) > maxGVisor {
		return fmt.Errorf("the gVisor archive is larger than %d bytes", maxGVisor)
	}
	got := sha512.Sum512(archive)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("gVisor %s for %s does not match its pinned SHA-512", GVisorRelease, arch)
	}
	return nil
}

// UnpackGVisor unpacks what RepackGVisor made into dir, which it creates:
// runsc and its helpers, each executable.
func UnpackGVisor(repacked []byte, dir string) error {
	gz, err := gzip.NewReader(bytes.NewReader(repacked))
	if err != nil {
		return err
	}
	tr := tar.NewReader(io.LimitReader(gz, maxGVisor))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(h.Name)
		if name == "." || strings.HasPrefix(name, "../") || path.IsAbs(name) {
			return fmt.Errorf("the gVisor archive names %q", h.Name)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755) //nolint:gosec // a path checked above, under dir
			if err != nil {
				return err
			}
			_, cerr := io.CopyN(f, tr, h.Size)
			if err := f.Close(); cerr == nil {
				cerr = err
			}
			if cerr != nil {
				return cerr
			}
		}
	}
}

// RepackGVisor turns a checked gvisor.tar.zstd into the gzip tar the server
// unpacks: regular files and directories only, runsc at the top and the
// helpers under gvisor-bin/, no path that climbs out.
func RepackGVisor(archive []byte) ([]byte, error) {
	zr, err := zstd.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	tr := tar.NewReader(io.LimitReader(zr, maxGVisor))
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	sawRunsc := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the gVisor archive: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." || strings.HasPrefix(name, "../") || path.IsAbs(name) {
			continue
		}
		keep := name == "runsc" || name == "gvisor-bin" || strings.HasPrefix(name, "gvisor-bin/")
		if !keep {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: h.Size}); err != nil {
				return nil, err
			}
			if _, err := io.CopyN(tw, tr, h.Size); err != nil {
				return nil, err
			}
			if name == "runsc" {
				sawRunsc = true
			}
		}
	}
	if !sawRunsc {
		return nil, fmt.Errorf("the gVisor archive holds no runsc")
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
