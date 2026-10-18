package decode

import (
	"bytes"
	"io"
	"os"
	"path"
	"strings"
)

// In an input source, ClamAV checks only the executable and binary risky
// files: "a ClamAV scan of
// 10000 files might take a long time", and a repository's source code,
// text and data are not where an executable payload hides. The host looks
// the same files up on Hybrid Analysis. Uploads and a url source are
// checked whole, as before.

// sourceRiskyTypes are the detected types checked in a source, the binary
// half of the host's Hybrid Analysis table (pkg/runner/hybrid riskyTypes,
// which this stdlib-only package cannot import; TestSourceRiskyIsNarrower
// holds the two together): PE, ELF and
// Mach-O executables, libraries and objects, Java and WebAssembly code,
// installers and packages, legacy and macro Office and their container,
// PDF and CHM. A type is matched without its parameters.
var sourceRiskyTypes = map[string]bool{
	"application/x-dosexec":                                      true,
	"application/vnd.microsoft.portable-executable":              true,
	"application/x-msdownload":                                   true,
	"application/x-ms-dos-executable":                            true,
	"application/x-executable":                                   true,
	"application/x-sharedlib":                                    true,
	"application/x-pie-executable":                               true,
	"application/x-object":                                       true,
	"application/x-elf":                                          true,
	"application/x-mach-binary":                                  true,
	"application/x-mach-o-executable":                            true,
	"application/x-java-applet":                                  true,
	"application/java-vm":                                        true,
	"application/wasm":                                           true,
	"application/x-msi":                                          true,
	"application/x-ms-installer":                                 true,
	"application/vnd.android.package-archive":                    true,
	"application/java-archive":                                   true,
	"application/x-java-archive":                                 true,
	"application/msword":                                         true,
	"application/vnd.ms-excel":                                   true,
	"application/vnd.ms-powerpoint":                              true,
	"application/vnd.ms-word.document.macroenabled.12":           true,
	"application/vnd.ms-word.template.macroenabled.12":           true,
	"application/vnd.ms-excel.sheet.macroenabled.12":             true,
	"application/vnd.ms-excel.template.macroenabled.12":          true,
	"application/vnd.ms-powerpoint.presentation.macroenabled.12": true,
	"application/x-ole-storage":                                  true,
	"application/cdfv2":                                          true,
	"application/pdf":                                            true,
	"application/vnd.ms-htmlhelp":                                true,
	"application/x-chm":                                          true,
}

// sourceRiskyNames are the extensions checked whatever the content was
// detected as, so a package a detector names a plain zip (.jar, .apk), or a
// binary whose detection failed, is still checked. No script or text
// extension is here: those are what a source leaves unchecked.
var sourceRiskyNames = map[string]bool{
	"exe": true, "dll": true, "scr": true, "sys": true, "so": true, "dylib": true, "msi": true, "apk": true, "jar": true, "class": true, "wasm": true,
	"doc": true, "xls": true, "ppt": true, "docm": true, "xlsm": true, "pptm": true, "dotm": true, "xltm": true, "potm": true,
	"pdf": true, "chm": true,
}

// SourceRisky says whether a file of this detected type, at this report
// path, is checked in an input source.
func SourceRisky(mime, p string) bool {
	mime, _, _ = strings.Cut(mime, ";")
	if sourceRiskyTypes[strings.ToLower(strings.TrimSpace(mime))] {
		return true
	}
	// A versioned library, libfoo.so.1.2, has the version as its extension.
	if strings.Contains(strings.ToLower(path.Base(p)), ".so.") {
		return true
	}
	return sourceRiskyNames[strings.TrimPrefix(path.Ext(strings.ToLower(p)), ".")]
}

// WhySourceImage: an image in an input source is not read with OCR. A
// codebase's screenshots and icons run to hundreds, tesseract takes seconds
// each, and the whole decode has 30 minutes (a desktop repository's 896
// screenshots alone would take about 40); the image stays readable as its
// original.
const WhySourceImage Why = "source-image"

// InSource says whether a report path is inside one of the sources'
// folders.
func InSource(rel string, sources []string) bool {
	first, _, _ := strings.Cut(rel, "/")
	for _, s := range sources {
		if s == first {
			return true
		}
	}
	return false
}

// riskyMagic are the first bytes of an executable, a library, Java or
// WebAssembly code, an OLE container or a PDF.
var riskyMagic = [][]byte{
	[]byte("MZ"), []byte("\x7fELF"), []byte("%PDF"), []byte("\x00asm"),
	{0xfe, 0xed, 0xfa, 0xce}, {0xfe, 0xed, 0xfa, 0xcf}, {0xce, 0xfa, 0xed, 0xfe}, {0xcf, 0xfa, 0xed, 0xfe},
	{0xca, 0xfe, 0xba, 0xbe}, {0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1},
}

// BinaryMagic says whether a file's first bytes are an executable's, a
// library's, Java's, WebAssembly's, an OLE container's or a PDF's. A
// source file that opens so is checked whatever its detected type: an
// Outlook message is an OLE container under a type of its own, and the
// host holds a file left unchecked to these same bytes.
func BinaryMagic(b []byte) bool {
	for _, m := range riskyMagic {
		if bytes.HasPrefix(b, m) {
			return true
		}
	}
	return false
}

// opensRisky reads a file's first bytes for BinaryMagic. A file that
// cannot be read is taken as risky, so it is checked.
func opensRisky(src string) bool {
	f, err := os.Open(src)
	if err != nil {
		return true
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 8)
	n, _ := io.ReadFull(f, head)
	return BinaryMagic(head[:n])
}
