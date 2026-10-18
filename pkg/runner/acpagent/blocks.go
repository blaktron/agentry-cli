package acpagent

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/pkg/runner/deliver"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// How much of a deliverable the agent puts in the message itself: a text file
// up to InlineEach bytes, and InlineTotal bytes of them in all. The rest are
// handed back as links to the file in the run's folder.
const (
	InlineEach  = 256 << 10
	InlineTotal = 1 << 20
)

// inlineTypes are the file types read as text and embedded.
var inlineTypes = map[string]string{
	"md": "text/markdown", "txt": "text/plain", "csv": "text/csv", "json": "application/json",
}

var mimeTypes = map[string]string{
	"pdf": "application/pdf", "png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg",
	"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
}

// deliverableBlocks turns a run's deliverables into ACP content blocks:
// an embedded resource for a small text file and a resource link for any
// other. A file is handed back only after it is shown to be the one the
// envelope lists — under reports/, reached through no link, and the size and
// SHA-256 the runner computed — so a path that leaves the run's folder, a
// link, or a file changed since the run is named in a note and handed back
// as nothing.
func deliverableBlocks(runDir string, ds *receipt.Deliverables) (blocks []map[string]any, notes []string) {
	if ds == nil {
		return nil, nil
	}
	inlined := 0
	for _, f := range ds.Files {
		title := oneLine(strings.TrimSpace(f.Title))
		if title == "" {
			title = oneLine(filepath.Base(filepath.FromSlash(f.Path)))
		}
		typ := strings.ToLower(f.Type)
		if mime, ok := inlineTypes[typ]; ok && f.Bytes <= InlineEach && inlined+int(f.Bytes) <= InlineTotal {
			abs, b, err := deliver.Read(runDir, f, InlineEach)
			if err != nil {
				notes = append(notes, withheld(title, err))
				continue
			}
			if utf8.Valid(b) {
				inlined += len(b)
				blocks = append(blocks, map[string]any{
					"type": "resource",
					"resource": map[string]any{
						"uri": fileURI(abs), "mimeType": mime, "text": string(b),
						"_meta": meta(f),
					},
				})
				continue
			}
			// Not text after all: a link below.
		}
		abs, err := deliver.Locate(runDir, f)
		if err != nil {
			notes = append(notes, withheld(title, err))
			continue
		}
		mime := mimeTypes[typ]
		if mime == "" {
			mime = "application/octet-stream"
		}
		blocks = append(blocks, map[string]any{
			"type": "resource_link", "uri": fileURI(abs), "name": oneLine(filepath.Base(abs)), "title": title,
			"mimeType": mime, "size": f.Bytes, "_meta": meta(f),
		})
	}
	if ds.Truncated {
		notes = append(notes, fmt.Sprintf("The run had more than %d deliverables; the rest are not listed.", receipt.MaxDeliverables))
	}
	return blocks, notes
}

func meta(f receipt.Deliverable) map[string]any {
	return map[string]any{"agentry": map[string]any{"sha256": f.SHA256, "path": oneLine(f.Path), "marked": f.Marked}}
}

var reasonWords = map[string]string{
	receipt.DeliveryErrChanged:    "it is not the file the run listed (its bytes changed)",
	receipt.DeliveryErrMissing:    "it is not in the run's folder",
	receipt.DeliveryErrOutside:    "it is not under the run's reports folder",
	receipt.DeliveryErrLink:       "it is, or sits behind, a link",
	receipt.DeliveryErrUnreadable: "it could not be read",
	receipt.DeliveryErrType:       "its type is one a file manager acts on when it is merely viewed",
	receipt.DeliveryErrTooLarge:   "it is too large to hand back in a message",
}

func withheld(title string, err error) string {
	var re *deliver.ReadError
	reason := "it could not be checked"
	if errors.As(err, &re) {
		if w := reasonWords[re.Reason]; w != "" {
			reason = w
		}
	}
	return fmt.Sprintf("%q was not handed back: %s.", title, reason)
}

// fileURI is the file: URI of an absolute path.
func fileURI(abs string) string {
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}
