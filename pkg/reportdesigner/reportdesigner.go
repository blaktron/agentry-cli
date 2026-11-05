// Package reportdesigner is the Report Designer: the QuickAgent package that
// designs a person's report design, the look of their PDF reports, embedded
// in the CLI and run by the runner as a built-in package, the fifth beside
// the Knowledge Creator, the Drafter, the Voice Writer and the Examiner. It
// has two jobs. Design writes a whole design document from a brief, a
// design to start from, the logo, reference images and web addresses.
// Cover writes a custom cover page for a design in Typst code. Its contract
// declares no egress, no filesystem, no exec and the files data category:
// the CLI stages what it is given as inputs, the application grants it
// render_preview, check_design and view_reference (and web_read held to the
// addresses the person gave, when a page is drawn by scripts), and it writes
// its work through write_report; pkg/reportdesigner/build keeps it.
package reportdesigner

import (
	"embed"
	"io/fs"
)

//go:embed agent.yaml INSTRUCTION.md README.md LICENSE knowledge
var files embed.FS

// FS is the package's tree: agent.yaml, INSTRUCTION.md, README.md and
// LICENSE at its root (a public release carries the last two), and under
// knowledge/ the pack it ships (GuideName).
func FS() fs.FS { return files }

// GuideName is the knowledge pack the Report Designer ships in its own
// release: the design document, its closed lists, choosing a look, a
// custom cover's conventions, a short course in cover design, a worked
// example and the checks before finishing. The runner gives it to every
// run of the package. Its manifest.json, index and lists page are
// generated (TestTheDesignGuideIsCurrent).
const GuideName = "design-guide"

// The package's identity, as its manifest declares it.
const (
	Namespace = "agentry"
	Name      = "report-designer"
	Version   = "1.0.0"
)

// LocalTimeoutFloorSeconds is the least a Report Designer run on this
// computer is given: 30 minutes, past the runner policy's 15-minute run
// timeout, since a cover is rendered and refined up to twelve times. It is
// a timeout alone, as the Voice Writer's floor is.
const LocalTimeoutFloorSeconds = 1800

// The files a run writes under reports/ with write_report.
const (
	// DesignFile is the design document: the whole design a Design run
	// made, without images.
	DesignFile = "design.json"
	// CoverFile is a Cover run's custom cover, Typst code.
	CoverFile = "cover.typ"
	// NotesFile is what both jobs say of their own work: the name a
	// Cover run gives the design it makes, and whether the model looked
	// at its renders.
	NotesFile = "designer.json"
)

// Notes is NotesFile.
type Notes struct {
	// Name is the name of the design a Cover run makes; a Design run
	// names its design in DesignFile.
	Name string `json:"name,omitempty"`
	// Looked says the model saw its renders and the references; false
	// when it could not read images.
	Looked bool `json:"looked"`
}
