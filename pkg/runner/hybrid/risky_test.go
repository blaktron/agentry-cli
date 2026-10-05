package hybrid

import "testing"

// The risky types (D11), by detected content, archive members included,
// and by name for the scripts and shortcuts whose content a detector
// reads as text: these are looked up; nothing else is sent.
func TestRiskyTypes(t *testing.T) {
	for _, c := range []struct {
		mime, path string
		risky      bool
	}{
		{"application/x-dosexec", "setup.bin", true},
		{"application/vnd.microsoft.portable-executable", "a", true},
		{"application/x-msdownload", "lib.dll", true},
		{"application/x-executable", "tool", true},
		{"application/x-sharedlib", "libx.so.1", true},
		{"application/x-pie-executable", "a.out", true},
		{"application/x-mach-binary", "app", true},
		{"application/x-msi", "i.msi", true},
		{"application/vnd.android.package-archive", "app.apk", true},
		{"application/java-archive", "lib.jar", true},
		{"application/pdf", "paper.pdf", true},
		{"application/msword", "old.doc", true},
		{"application/vnd.ms-excel", "old.xls", true},
		{"application/vnd.ms-powerpoint", "old.ppt", true},
		{"application/vnd.ms-word.document.macroEnabled.12", "m.docm", true},
		{"application/vnd.ms-excel.sheet.macroEnabled.12", "m.xlsm", true},
		{"application/vnd.ms-powerpoint.presentation.macroEnabled.12", "m.pptm", true},
		{"application/rtf", "r.rtf", true},
		{"text/rtf", "r", true},
		{"application/vnd.ms-htmlhelp", "h.chm", true},
		{"application/x-iso9660-image", "disk.iso", true},
		{"application/x-ms-shortcut", "x.lnk", true},
		{"text/javascript", "a.js", true},
		{"text/plain", "run.ps1", true},
		{"text/plain", "go.BAT", true},
		{"text/plain", "x.cmd", true},
		{"text/plain", "x.vbs", true},
		{"text/plain", "x.hta", true},
		{"text/plain", "x.wsf", true},
		{"application/octet-stream", "disk.img", true},
		{"application/x-dosexec", "renamed.txt", true},
		{"", "archive.zip/inner/evil.exe", true},
		{"text/plain", "notes.txt", false},
		{"text/markdown", "README.md", false},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", "a.docx", false},
		{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "a.xlsx", false},
		{"image/png", "a.png", false},
		{"application/zip", "a.zip", false},
		{"text/csv", "a.csv", false},
		{"application/json", "a.json", false},
		{"", "doc", false},
	} {
		if got := Risky(c.mime, c.path); got != c.risky {
			t.Errorf("Risky(%q, %q) = %v, want %v", c.mime, c.path, got, c.risky)
		}
	}
}
