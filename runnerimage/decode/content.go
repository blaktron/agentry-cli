package decode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

type contentKind struct {
	family       Family
	tool, format string
	text         bool
	archive      archiveFormat
	// mime is the type the file was detected as, without parameters: the
	// report carries it so the host can tell a risky type to look up
	// (agentry-cli#564). It never selects a command line.
	mime string
}

// Only the input path varies. Neither a detected type nor a document's
// content is interpolated into argv: types select this closed table. It
// names the documents alone, those whose text a converter reads out of
// them. RTF and mail are the documents that are also UTF-8 text, so they
// are matched here before the text fast path (detect). Code, markup,
// configuration and data text (HTML, XML, SVG, CSV, JSON, YAML, scripts,
// source) are not documents and never converted: they are read as
// uploaded (agentry-cli#630).
var contentKinds = map[string]contentKind{
	"application/pdf": {family: FamilyPDF, tool: "pdftotext"},
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": {family: FamilyDocument, tool: "pandoc", format: "docx"},
	"application/vnd.oasis.opendocument.text":                                 {family: FamilyDocument, tool: "pandoc", format: "odt"},
	"application/epub+zip":                                                    {family: FamilyDocument, tool: "pandoc", format: "epub"},
	"application/rtf":                                                         {family: FamilyDocument, tool: "pandoc", format: "rtf"},
	"text/rtf":                                                                {family: FamilyDocument, tool: "pandoc", format: "rtf"},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         {family: FamilySpreadsheet, tool: "agentry-runner", format: "xlsx"},
	"application/vnd.oasis.opendocument.spreadsheet":                            {family: FamilySpreadsheet, tool: "agentry-runner", format: "ods"},
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": {family: FamilyPresentation, tool: "agentry-runner", format: "pptx"},
	"application/vnd.oasis.opendocument.presentation":                           {family: FamilyPresentation, tool: "agentry-runner", format: "odp"},
	"application/msword":            {family: FamilyDocument, tool: "tika"},
	"application/vnd.ms-excel":      {family: FamilySpreadsheet, tool: "tika"},
	"application/vnd.ms-powerpoint": {family: FamilyPresentation, tool: "tika"},
	"application/vnd.ms-outlook":    {family: FamilyDocument, tool: "tika"},
	"message/rfc822":                {family: FamilyDocument, tool: "tika"},
}

func detectedKind(mime string) contentKind {
	// Tika qualifies some types ("application/x-rar-compressed;
	// version=5"); the type alone picks the reader.
	mime, _, _ = strings.Cut(mime, ";")
	mime = strings.TrimSpace(mime)
	if k, ok := contentKinds[mime]; ok {
		return k
	}
	if k, ok := archiveKinds[mime]; ok {
		return k
	}
	if isImage(mime) {
		return contentKind{family: FamilyImage, tool: "tesseract"}
	}
	switch mime {
	case "image/svg+xml", "application/xml", "application/xhtml+xml":
		return contentKind{family: FamilyText, text: true}
	case "text/plain", "text/markdown", "application/json", "application/x-ndjson", "application/x-empty", "inode/x-empty":
		return contentKind{family: FamilyText, text: true}
	case "application/octet-stream":
		return contentKind{family: FamilyUnknown}
	}
	if strings.HasPrefix(mime, "text/") {
		return contentKind{family: FamilyText, text: true}
	}
	return contentKind{family: FamilyDocument, tool: "tika"}
}

func detect(ctx context.Context, src string, tools Tools) (contentKind, error) {
	mime, err := toolOutput(ctx, tools.File, "--brief", "--mime-type", "--", src)
	if err == nil {
		if k, ok := contentKinds[mime]; ok {
			return withMIME(k, mime), nil
		}
		if isImage(mime) {
			return withMIME(detectedKind(mime), mime), nil
		}
		// libmagic names every archive but a zip reliably; a zip may be
		// an Office file, which only Tika tells apart.
		if k, ok := archiveKinds[mime]; ok && mime != "application/zip" {
			return withMIME(k, mime), nil
		}
	}
	// The text fast path: content that is UTF-8 text and no document
	// above is code, markup, configuration or data, read as uploaded
	// whatever a detector calls it (Tika names a .ts file video/mp2t by
	// its name), and told without a JVM. A repository's working tree
	// took one Tika start per text file before (agentry-cli#630). A file
	// libmagic calls a zip has a zip's signature, and goes to Tika even
	// when the bytes after it happen to be text.
	if mime != "application/zip" && isText(src) {
		if err != nil || !ValidMIME(mime) {
			mime = "text/plain"
		}
		return withMIME(contentKind{family: FamilyText, text: true}, mime), nil
	}
	// libmagic cannot reliably distinguish generic ZIP/compound containers.
	// Tika looks at their contents before choosing a reader.
	mime, err = toolOutput(ctx, tools.Tika, tikaArgs("--detect", src)...)
	if err != nil {
		return contentKind{family: FamilyUnknown}, err
	}
	return withMIME(detectedKind(mime), mime), nil
}

// isImage says whether a type is an image OCR reads: SVG is an image
// written as markup, and read as text.
func isImage(mime string) bool {
	return strings.HasPrefix(mime, "image/") && mime != "image/svg+xml"
}

// withMIME records the detected type on its kind, without parameters, and
// only when it has the shape of a media type: a detector's output is not
// carried otherwise.
func withMIME(k contentKind, mime string) contentKind {
	mime, _, _ = strings.Cut(mime, ";")
	mime = strings.ToLower(strings.TrimSpace(mime))
	if ValidMIME(mime) {
		k.mime = mime
	}
	return k
}

// mimeRE is a media type's shape: type/subtype in RFC 6838's characters.
var mimeRE = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]{0,63}/[a-z0-9][a-z0-9!#$&^_.+-]{0,126}$`)

// ValidMIME says whether s is a bare media type, as the report may carry
// it; the host holds the report to the same rule.
func ValidMIME(s string) bool { return mimeRE.MatchString(s) }

func tikaArgs(mode, src string) []string {
	return []string{"-Xmx512m", "-Djava.io.tmpdir=/tmp", "-jar", "/opt/tika/tika-app.jar", mode, src}
}

func decoderCommand(ctx context.Context, tool string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, tool, args...) //nolint:gosec // fixed tools and flags; only the staged path varies
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/tmp", "OMP_THREAD_LIMIT=1", "LC_ALL=C.UTF-8"}
	return cmd
}

func toolOutput(ctx context.Context, tool string, args ...string) (string, error) {
	cmd := decoderCommand(ctx, tool, args...)
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, n: 1024}
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

func tikaText(ctx context.Context, tool, src, dst string, capBytes int64) error {
	f, err := os.Create(dst) //nolint:gosec // decoder-owned scratch, never a document path
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	cmd := decoderCommand(ctx, tool, tikaArgs("--text", src)...)
	cmd.Stdout = &limitedWriter{w: f, n: int(capBytes + 1)}
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() == 0 {
		return fmt.Errorf("no readable text")
	}
	return nil
}
