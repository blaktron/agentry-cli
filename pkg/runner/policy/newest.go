package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/blaktron/agentry-cli/pkg/dirpublish"
)

// NewestFile is the record, beside the policy file, of the newest vendor
// release of each harness this machine built an image of. A harness image
// the policy does not pin itself is built from its vendor's newest release
// when that release passes every check, and from the release this build of
// agentry pins and tested when it does not; the record says which, so a
// run takes the image setup made. It is the runner's own state, never the
// operator's: the policy file and its sha are unchanged by it.
const NewestFile = "harness-newest.json"

// Newest is one adapter's entry in that record.
type Newest struct {
	// Checked is when the vendor was last asked for its newest release.
	Checked time.Time `json:"checked"`
	// Ref and Artifact are the image built from the newest release, as a
	// HarnessImage pins it. Empty when the newest release is the tested
	// one, or could not be used: the tested pin stands then.
	Ref      string `json:"ref,omitempty"`
	Artifact string `json:"artifact,omitempty"`
	// Version is the vendor's version of that release.
	Version string `json:"version,omitempty"`
	// Release is what the builder needs to build that release again, as
	// the builder writes it.
	Release json.RawMessage `json:"release,omitempty"`
	// Failed names the newest release that could not be used and why, for
	// the log and for the person asking; it is tried again at the next
	// check.
	Failed string `json:"failed,omitempty"`
}

// Image is the harness image the entry records, ok false when it records
// none.
func (n Newest) Image() (h HarnessImage, ok bool) {
	if n.Ref == "" || n.Artifact == "" {
		return HarnessImage{}, false
	}
	return HarnessImage{Ref: n.Ref, Artifact: n.Artifact}, true
}

// NewestPathFor is where the record of the newest harness images is for
// the policy file at policyPath: beside it.
func NewestPathFor(policyPath string) string {
	return filepath.Join(filepath.Dir(policyPath), NewestFile)
}

// ReadNewest reads the record at path. A missing file is an empty record;
// one that cannot be read or parsed is an error, which a caller takes as
// no record at all.
func ReadNewest(path string) (map[string]Newest, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the runner's own record, beside the policy
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Newest{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]Newest{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, nil
}

// WriteNewest sets adapterName's entry in the record at path, keeping the
// others. The file is staged beside itself and renamed over, so a run
// reading it meanwhile reads the old record or the new one.
func WriteNewest(path, adapterName string, n Newest) error {
	m, err := ReadNewest(path)
	if err != nil {
		m = map[string]Newest{}
	}
	m[adapterName] = n
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return dirpublish.ReplaceFile(path, buf.Bytes(), 0o644)
}

// loadNewest takes the record beside the policy file at path into p's
// images: an entry whose image would not pass as a pin of its own is
// ignored, as is a record that cannot be read, so the tested pins stand.
func (p *Policy) loadNewest(path string) {
	m, err := ReadNewest(NewestPathFor(path))
	if err != nil {
		return
	}
	for name, n := range m {
		if h, ok := n.Image(); ok && TriesNewest(name) && h.validate(name) == nil {
			p.Images.SetNewest(name, n)
		}
	}
}

// TriesNewest reports whether adapterName's image is built from its
// vendor's newest release first: Claude Code's and Codex's.
func TriesNewest(adapterName string) bool {
	return adapterName == "claude-image" || adapterName == "codex-image"
}

// SetNewest makes n's image the one adapterName runs in where the policy
// pins none itself, as setup does once it has built it.
func (i *Images) SetNewest(adapterName string, n Newest) {
	if i.newest == nil {
		i.newest = map[string]Newest{}
	}
	i.newest[adapterName] = n
}

// NewestRecord is the record's entry for adapterName that Load or setup
// took, ok false when there is none.
func (i Images) NewestRecord(adapterName string) (Newest, bool) {
	n, ok := i.newest[adapterName]
	return n, ok
}

// ClearNewest goes back to the tested pin for adapterName.
func (i *Images) ClearNewest(adapterName string) { delete(i.newest, adapterName) }

// Pins reports whether the policy file pins adapterName's harness image
// itself, which then is never replaced by a newer release.
func (i Images) Pins(adapterName string) bool {
	_, ok := i.HarnessImages[adapterName]
	return ok
}
