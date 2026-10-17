package deliver

import (
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxNameBytes bounds one path segment a delivery makes, below the 255 bytes
// most file systems take, so a " (2)" fits after it.
const maxNameBytes = 200

// fallbackName stands for a name nothing of which could be kept.
const fallbackName = "deliverable"

// SafeName turns a name the agent or the publisher chose into one path
// segment that is safe to create on any system: the same name read back as a
// single plain file name, never a path, a hidden file or a device. It
//
//   - replaces the path separators and the characters Windows reserves
//     (\ / : * ? " < > |, a colon among them, which would name a stream),
//     every control, format, line-break and bidirectional formatting
//     character (which could make "txt" + U+202E + "fdp.exe" read as a PDF)
//     and every other character that does not print, each with an underscore;
//   - drops leading and trailing spaces and dots (a leading dot hides a
//     file, a trailing one is dropped by Windows, and ".." is no name);
//   - puts an underscore before a reserved Windows device name (CON, PRN,
//     AUX, NUL, COM0 to COM9, LPT0 to LPT9, with any extension);
//   - shortens a long name, keeping a short extension.
//
// A name with nothing left is "deliverable".
func SafeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ':
			b.WriteRune(' ')
		case strings.ContainsRune(`\/:*?"<>|`, r):
			b.WriteRune('_')
		case !unicode.IsPrint(r),
			unicode.IsControl(r),
			unicode.Is(unicode.Cf, r),
			unicode.Is(unicode.Bidi_Control, r),
			r == utf8.RuneError:
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	name := strings.Trim(b.String(), " .")
	if name == "" {
		return fallbackName
	}
	if reservedWindowsName(name) {
		name = "_" + name
	}
	return shorten(name)
}

// reservedWindowsName says whether name, whatever follows its first dot,
// is a device Windows keeps for itself.
func reservedWindowsName(name string) bool {
	stem, _, _ := strings.Cut(name, ".")
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '0' && stem[3] <= '9' {
		return true
	}
	// COM and LPT with a superscript digit are reserved as well.
	for _, p := range []string{"COM", "LPT"} {
		if rest, ok := strings.CutPrefix(stem, p); ok && (rest == "¹" || rest == "²" || rest == "³") {
			return true
		}
	}
	return false
}

// shorten cuts name to maxNameBytes at a character boundary, keeping an
// extension of at most 16 bytes.
func shorten(name string) string {
	if len(name) <= maxNameBytes {
		return name
	}
	ext := path.Ext(name)
	if len(ext) > 16 || len(ext) == len(name) {
		ext = ""
	}
	stem := strings.TrimSuffix(name, ext)
	limit := maxNameBytes - len(ext)
	for len(stem) > limit {
		_, size := utf8.DecodeLastRuneInString(stem)
		stem = stem[:len(stem)-size]
	}
	stem = strings.TrimRight(stem, " .")
	if stem == "" {
		return fallbackName + ext
	}
	return stem + ext
}
