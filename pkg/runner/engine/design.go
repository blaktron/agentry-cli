package engine

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// The Report Designer's previews: render_preview records each in a ledger
// under the receipts folder and keeps its PNG in a folder beside it, from
// which the CLI takes the thumbnail of the cover it keeps.
const (
	PreviewLedgerFile = "previews.ndjson"
	PreviewsDir       = "previews"
	// DocumentLedgerFile records each image document_pages showed of the
	// person's document and each crop crop_logo took, from which the CLI
	// takes the logo it keeps.
	DocumentLedgerFile = "document.ndjson"
)

// grantDesign stamps the Report Designer's ledger and previews folder into
// the run's rendered tools spec when the plan grants its tools.
func grantDesign(gatewayDir string, rd *runDirs) error {
	if rd.previewLedger == "" {
		return nil
	}
	specPath := filepath.Join(gatewayDir, "toolspec.json")
	b, err := os.ReadFile(specPath) //nolint:gosec // our own rendered config
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var ts toolsrv.Spec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ts); err != nil {
		return fmt.Errorf("the rendered tools spec does not decode as the tools server reads it: %w", err)
	}
	if !ts.Design.Enabled {
		return nil
	}
	ts.Design.Ledger = path.Join(spec.OutMount, "receipts", PreviewLedgerFile)
	ts.Design.Previews = path.Join(spec.OutMount, "receipts", PreviewsDir)
	ts.Design.DocumentLedger = path.Join(spec.OutMount, "receipts", DocumentLedgerFile)
	out, err := json.MarshalIndent(&ts, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(specPath, append(out, '\n'), 0o644) //nolint:gosec // read by the tools container
}

// ReadPreviewLedger reads a preview ledger, skipping a line that does not
// decode.
func ReadPreviewLedger(p string) ([]toolsrv.PreviewEntry, error) {
	return readLedger[toolsrv.PreviewEntry](p)
}

// readLedger reads an NDJSON ledger the tools container appended to,
// skipping a line that does not decode as T.
func readLedger[T any](p string) ([]T, error) {
	f, err := os.Open(p) //nolint:gosec // the run's own ledger
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e T
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// previewCount is the receipt's previews: how many renders the run made.
func previewCount(rd *runDirs) int {
	if rd.previewLedger == "" {
		return 0
	}
	entries, err := ReadPreviewLedger(rd.previewLedger)
	if err != nil {
		return 0
	}
	return min(len(entries), toolsrv.MaxPreviews)
}

// ReadDocumentLedger reads a document ledger, skipping a line that does
// not decode.
func ReadDocumentLedger(p string) ([]toolsrv.DocumentEntry, error) {
	return readLedger[toolsrv.DocumentEntry](p)
}

// documentPageCount is the receipt's documentPages: how many images of
// the person's document document_pages showed the model.
func documentPageCount(rd *runDirs) int {
	if rd.documentLedger == "" {
		return 0
	}
	entries, err := ReadDocumentLedger(rd.documentLedger)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.Tool == toolsrv.ToolDocumentPages {
			n++
		}
	}
	return min(n, toolsrv.MaxDocumentViews)
}
