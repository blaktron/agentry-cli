package hybrid

import (
	"path"
	"strings"
)

// riskyTypes are the detected types looked up: executables
// and libraries, installers and packages, legacy and macro Office, RTF,
// PDF, CHM, disk images and shortcuts. A type is matched without its
// parameters.
var riskyTypes = map[string]bool{
	// PE, ELF and Mach-O executables and libraries.
	"application/x-dosexec":                         true,
	"application/vnd.microsoft.portable-executable": true,
	"application/x-msdownload":                      true,
	"application/x-ms-dos-executable":               true,
	"application/x-executable":                      true,
	"application/x-sharedlib":                       true,
	"application/x-pie-executable":                  true,
	"application/x-elf":                             true,
	"application/x-mach-binary":                     true,
	"application/x-mach-o-executable":               true,
	// Installers and packages.
	"application/x-msi":                       true,
	"application/x-ms-installer":              true,
	"application/vnd.android.package-archive": true,
	"application/java-archive":                true,
	"application/x-java-archive":              true,
	// Scripts a detector names as such.
	"text/javascript":          true,
	"application/javascript":   true,
	"application/x-javascript": true,
	"text/vbscript":            true,
	"text/x-vbscript":          true,
	"text/x-msdos-batch":       true,
	"application/x-bat":        true,
	"application/hta":          true,
	// Shortcuts and disk images.
	"application/x-ms-shortcut":    true,
	"application/x-iso9660-image":  true,
	"application/x-raw-disk-image": true,
	// Legacy and macro Office, and their container.
	"application/msword":                                         true,
	"application/vnd.ms-excel":                                   true,
	"application/vnd.ms-powerpoint":                              true,
	"application/vnd.ms-word.document.macroenabled.12":           true,
	"application/vnd.ms-word.template.macroenabled.12":           true,
	"application/vnd.ms-excel.sheet.macroenabled.12":             true,
	"application/vnd.ms-excel.template.macroenabled.12":          true,
	"application/vnd.ms-powerpoint.presentation.macroenabled.12": true,
	"application/x-ole-storage":                                  true,
	"application/x-tika-msoffice":                                true,
	// RTF, PDF and CHM.
	"application/rtf":             true,
	"text/rtf":                    true,
	"application/pdf":             true,
	"application/vnd.ms-htmlhelp": true,
	"application/x-chm":           true,
}

// riskyNames are the extensions looked up whatever the content was detected as:
// the scripts and shortcuts a detector reads as plain text or data, and each
// risky type's own extension, so a file whose detection failed is still looked
// up. A text file's type is libmagic's, and libmagic calls a plain .mjs
// text/plain where Tika's name globs said text/javascript. Over-asking costs a
// lookup of a hash, never a file.
var riskyNames = map[string]bool{
	"exe": true, "dll": true, "scr": true, "com": true, "sys": true, "msi": true, "apk": true, "jar": true,
	"js": true, "mjs": true, "cjs": true, "jse": true, "vbs": true, "vbe": true, "ps1": true, "psm1": true, "bat": true, "cmd": true, "hta": true, "wsf": true,
	"lnk": true, "iso": true, "img": true,
	"doc": true, "xls": true, "ppt": true, "docm": true, "xlsm": true, "pptm": true, "dotm": true, "xltm": true, "potm": true,
	"rtf": true, "pdf": true, "chm": true,
}

// Risky says whether a file of this detected type, at this report path,
// is looked up. Everything else is never sent.
func Risky(mime, p string) bool {
	mime, _, _ = strings.Cut(mime, ";")
	if riskyTypes[strings.ToLower(strings.TrimSpace(mime))] {
		return true
	}
	ext := strings.TrimPrefix(path.Ext(strings.ToLower(p)), ".")
	return riskyNames[ext]
}
