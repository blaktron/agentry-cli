package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry/pkg/manifest"
)

// shippedKnowledge loads the knowledge packs an installed release ships
// (engine.ShippedPacks), naming the release's bytes for the receipt: its
// archive, or a local package's tree, which has no archive.
func shippedKnowledge(workspaceDir string, m *manifest.Manifest, meta *installdir.Meta) ([]engine.AttachedPack, error) {
	release := "archive " + meta.Digest
	if meta.Source == "local" {
		release = "local package tree " + meta.Digest
	}
	return engine.ShippedPacks(workspaceDir, m, release)
}

// withShipped puts a release's shipped packs before the person's attached
// ones (engine.WithShipped). A run mounts each pack at /knowledge/<name>,
// so an attached pack that takes a shipped pack's name is refused, naming
// both.
func withShipped(shipped, attached []engine.AttachedPack) ([]engine.AttachedPack, error) {
	out, err := engine.WithShipped(shipped, attached)
	if name, taken := engine.IsPackNameTaken(err); taken {
		return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("--knowledge %s: the release ships a knowledge pack of that name, mounted at %s/%s; attach a pack with another name", name, spec.KnowledgeMount, name))
	}
	return out, err
}

// byteSize is a size as a person reads it, in the binary units the rest of
// the CLI and the registry's caps use: "216 bytes", "46.9 KiB", "4.2 MiB".
func byteSize(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d bytes", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

// packSizes are the loaded packs' sizes: each tree's bytes, as the CLI
// counts a pack.
func packSizes(packs []engine.AttachedPack) map[string]int64 {
	out := make(map[string]int64, len(packs))
	for _, p := range packs {
		out[p.Name] = p.Bytes
	}
	return out
}

// archivePackSizes reads a release archive's tar headers (zstd, as the
// registry builds it) and sums, for each shipped pack, the sizes of the
// files of its tree — what the consent shows before anything is unpacked.
func archivePackSizes(archive []byte, ships []string) (map[string]int64, error) {
	if len(ships) == 0 {
		return nil, nil
	}
	zr, err := zstd.NewReader(bytes.NewReader(archive), zstd.WithDecoderMaxMemory(64<<20))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	// Bounded as the install's extraction is, since this reads before the
	// consent: past its file count or its total, the sizes are left out.
	lim := installExtractLimits()
	tr := tar.NewReader(io.LimitReader(zr, lim.MaxTotalBytes+(64<<20)))
	out := map[string]int64{}
	var total int64
	for files := 0; ; files++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read the archive's entries: %w", err)
		}
		if files >= lim.MaxFiles || hdr.Size < 0 || hdr.Size > lim.MaxTotalBytes-total {
			return nil, errors.New("the archive is past the install's bounds")
		}
		total += hdr.Size
		if name, ok := manifest.ShippedPackFile(ships, strings.TrimPrefix(hdr.Name, "./")); ok && hdr.Typeflag == tar.TypeReg {
			out[name] += hdr.Size
		}
	}
}

// sayKnowledge narrates each pack a run mounts, a shipped one as the
// release's and an attached one with its scan record.
func sayKnowledge(w io.Writer, packs []engine.AttachedPack) {
	for _, a := range packs {
		from := a.Scan.Describe()
		if a.Shipped != nil {
			from = "shipped with the release"
		}
		_, _ = fmt.Fprintf(w, "knowledge pack %s: %s, %d bytes, %s, %s — mounted read-only at %s/%s\n",
			a.Name, knowledge.CountPages(a.Pages), a.Bytes, a.Digest, from, spec.KnowledgeMount, a.Name)
	}
}
