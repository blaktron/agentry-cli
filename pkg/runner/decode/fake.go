package decode

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

// FakeFilesPackages is the package record ScriptFake has the files image
// answer: one decoder's version, as a real files image records it.
const FakeFilesPackages = "pandoc-cli-3.6.4-r0\n"

// ScriptFake makes f play the decode step of a run with inputs, for a test of
// anything that drives the engine with a dockerx.Fake: the decode image built
// from runnerImage and filesImage is present, so Ensure neither pulls nor
// builds; the files image's package record answers; and the decode container
// states its scan record on stdout before it decodes anything, as the runner
// image does, then writes a report that decodes no file. The real CheckWith
// then records each staged file as given and not read, and the run goes on.
//
// The scan record says the files were checked against signatures exactly
// when the container was given them (the /clamav mount), so an image with
// signatures and one without both pass the real check. Call it after
// anything that replaces f.Foreground, f.ExistingImages or f.ImageFiles
// wholesale.
func ScriptFake(f *dockerx.Fake, runnerImage, filesImage string) {
	s := ImageSpec(runnerImage, filesImage)
	if f.ExistingImages == nil {
		f.ExistingImages = map[string]bool{}
	}
	f.ExistingImages[s.Tag] = true
	if f.ImageFiles == nil {
		f.ImageFiles = map[string]string{}
	}
	f.ImageFiles[s.Tag+":"+FilesPackagesFile] = FakeFilesPackages
	if f.Foreground == nil {
		f.Foreground = map[string]*dockerx.FakeForeground{}
	}
	ff := &dockerx.FakeForeground{}
	ff.Before = func(r dockerx.RunSpec) {
		checked := false
		for _, m := range r.Mounts {
			if m.Target == clamMount {
				checked = true
			}
		}
		ff.Stdout = string(runnerdecode.RecordOf(nil, checked)) + "\n"
		for _, m := range r.Mounts {
			if m.Target == "/out" {
				report, _ := json.Marshal(map[string]any{"schema": runnerdecode.Schema, "files": []any{}})
				_ = os.WriteFile(filepath.Join(m.Source, runnerdecode.ReportFile), report, 0o600)
			}
		}
	}
	f.Foreground["agentry-decode-"] = ff
}
