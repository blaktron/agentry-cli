package docpages

import (
	"errors"
	"fmt"
	"strconv"
)

// The document as the CLI stages it for a Report Designer run, under the
// run's inputs folder: StagedFile with what was read of it, the pages the
// model looks at (ViewDir/<n>.png, at ViewPPI), the same pages at
// RenderPPI a logo is cropped from (FullDir/<n>.png), the text of each
// page (TextDir/<n>.txt; a Word document's whole text is 1.txt) and a
// Word document's pictures (MediaDir/<n>.png). Every image is the CLI's
// own encoding of the pixels it read; the document itself is never
// staged.
const (
	StagedDir  = "document"
	StagedFile = "document/document.json"
	ViewDir    = "document/pages"
	FullDir    = "document/full"
	TextDir    = "document/text"
	MediaDir   = "document/media"
)

// The bounds of the staged text.
const (
	// MaxPageText caps one page's text as staged.
	MaxPageText = 16 << 10
	// MaxDocxText caps a Word document's whole text as staged.
	MaxDocxText = 64 << 10
	// MaxStagedBytes caps StagedFile as the tools server reads it.
	MaxStagedBytes = 64 << 10
)

// Staged is StagedFile.
type Staged struct {
	Kind Kind `json:"kind"`
	// Name is the document's file name as the person gave it, one line.
	Name     string   `json:"name"`
	Pages    int      `json:"pages,omitempty"`
	Rendered int      `json:"rendered"`
	Fonts    []Font   `json:"fonts,omitempty"`
	Colours  []Colour `json:"colours,omitempty"`
	Headers  []string `json:"headers,omitempty"`
	Footers  []string `json:"footers,omitempty"`
	// Media is how many pictures are staged, MediaDir/1.png on, and
	// MediaSkipped how many the document has that were not read.
	Media        int `json:"media,omitempty"`
	MediaSkipped int `json:"mediaSkipped,omitempty"`
	// Texts is how many texts are staged, TextDir/1.txt on; TextCut says
	// one was cut to its cap.
	Texts   int  `json:"texts,omitempty"`
	TextCut bool `json:"textCut,omitempty"`
	// Failure and Stopped are Facts'.
	Failure string `json:"failure,omitempty"`
	Stopped string `json:"stopped,omitempty"`
	// LogoGiven says the person gave the run a logo of their own, which a
	// crop never replaces.
	LogoGiven bool `json:"logoGiven,omitempty"`
}

// StagedFrom is the staged record of what was read: f's facts, under the
// document's name.
func StagedFrom(f Facts, name string, media, texts int, cut, logoGiven bool) Staged {
	return Staged{Kind: f.Kind, Name: Line(name, 120), Pages: f.Pages, Rendered: f.Rendered, Fonts: f.Fonts, Colours: f.Colours,
		Headers: f.Headers, Footers: f.Footers, Media: media, MediaSkipped: f.MediaSkipped, Texts: texts, TextCut: cut, Failure: f.Failure, Stopped: f.Stopped, LogoGiven: logoGiven}
}

// Validate holds a staged record to its bounds.
func (s Staged) Validate() error {
	f := Facts{Schema: Schema, Kind: s.Kind, Pages: s.Pages, Rendered: s.Rendered, Fonts: s.Fonts, Colours: s.Colours, Headers: s.Headers, Footers: s.Footers, MediaSkipped: s.MediaSkipped, Failure: s.Failure, Stopped: s.Stopped}
	for i := range s.Media {
		f.Media = append(f.Media, MediaFile(i+1, "png"))
	}
	if err := f.Validate(); err != nil {
		return err
	}
	if s.Texts < 0 || s.Texts > MaxPages || (s.Kind == KindDOCX && s.Texts > 1) {
		return fmt.Errorf("staged: %d texts", s.Texts)
	}
	if !cleanText(s.Name, 120) {
		return errors.New("staged: the name is not one line of text")
	}
	return nil
}

// ViewFile, FullFile, TextFile and MediaFileOf are the staged files of
// page or picture n.
func ViewFile(n int) string    { return ViewDir + "/" + strconv.Itoa(n) + ".png" }
func FullFile(n int) string    { return FullDir + "/" + strconv.Itoa(n) + ".png" }
func TextFile(n int) string    { return TextDir + "/" + strconv.Itoa(n) + ".txt" }
func MediaFileOf(n int) string { return MediaDir + "/" + strconv.Itoa(n) + ".png" }
