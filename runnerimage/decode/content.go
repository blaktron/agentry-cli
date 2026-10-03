package decode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

type contentKind struct {
	family       Family
	tool, format string
	text         bool
}

// Only the input path varies. Neither a detected type nor a document's
// content is interpolated into argv: types select this closed table.
var contentKinds = map[string]contentKind{
	"application/pdf": {family: FamilyPDF, tool: "pdftotext"},
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": {family: FamilyDocument, tool: "pandoc", format: "docx"},
	"application/vnd.oasis.opendocument.text":                                 {family: FamilyDocument, tool: "pandoc", format: "odt"},
	"application/epub+zip":                                                    {family: FamilyDocument, tool: "pandoc", format: "epub"},
	"application/rtf":                                                         {family: FamilyDocument, tool: "pandoc", format: "rtf"},
	"text/rtf":                                                                {family: FamilyDocument, tool: "pandoc", format: "rtf"},
	"text/html":                                                               {family: FamilyDocument, tool: "pandoc", format: "html"},
	"application/xhtml+xml":                                                   {family: FamilyDocument, tool: "pandoc", format: "html"},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": {family: FamilySpreadsheet, tool: "agentry-runner", format: "xlsx"},
	"application/vnd.oasis.opendocument.spreadsheet":                    {family: FamilySpreadsheet, tool: "agentry-runner", format: "ods"},
	"text/csv": {family: FamilySpreadsheet, tool: "agentry-runner", format: "csv"},
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": {family: FamilyPresentation, tool: "agentry-runner", format: "pptx"},
	"application/vnd.oasis.opendocument.presentation":                           {family: FamilyPresentation, tool: "agentry-runner", format: "odp"},
	"application/msword":            {family: FamilyDocument, tool: "tika"},
	"application/vnd.ms-excel":      {family: FamilySpreadsheet, tool: "tika"},
	"application/vnd.ms-powerpoint": {family: FamilyPresentation, tool: "tika"},
	"application/vnd.ms-outlook":    {family: FamilyDocument, tool: "tika"},
	"message/rfc822":                {family: FamilyDocument, tool: "tika"},
}

func detectedKind(mime string) contentKind {
	if k, ok := contentKinds[mime]; ok {
		return k
	}
	if strings.HasPrefix(mime, "image/") {
		return contentKind{family: FamilyImage, tool: "tesseract"}
	}
	switch mime {
	case "text/plain", "text/markdown", "application/json", "application/x-ndjson", "application/x-empty", "inode/x-empty":
		return contentKind{family: FamilyText, text: true}
	// Archive contents wait for M4's member, depth and expansion checks.
	case "application/octet-stream", "application/zip", "application/x-tar", "application/gzip", "application/x-gzip", "application/x-bzip2", "application/x-xz", "application/x-7z-compressed", "application/x-rar-compressed", "application/vnd.rar":
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
			return k, nil
		}
		if strings.HasPrefix(mime, "image/") {
			return detectedKind(mime), nil
		}
	}
	// libmagic cannot reliably distinguish generic ZIP/compound containers
	// or text families. Tika looks at their contents before choosing a reader.
	mime, err = toolOutput(ctx, tools.Tika, tikaArgs("--detect", src)...)
	if err != nil {
		return contentKind{family: FamilyUnknown}, err
	}
	return detectedKind(mime), nil
}

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
