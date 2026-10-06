package drafter

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// archiveReaders are the base commands that read archives. A draft never
// declares one: the runner unpacks every archive before the run, and the
// input tools read each file in it (DR-64 amended by agentry-cli#634;
// plan quickagents-that-run.md M7). Exec is for computation the tools
// cannot do, never for reading inputs.
var archiveReaders = []string{"tar", "unzip", "gunzip", "bunzip2", "unxz", "zcat", "bzcat", "xzcat"}

// execProblems is the Drafter's rule on exec, one line each: a declared
// command that is not one every machine provides (policy.BaseExecCommands),
// which a run would refuse at plan time, and an archive reader, which only
// reads what the runner already unpacked. Nil when the draft declares no
// commands, or its manifest does not parse (validate says so).
func execProblems(raw json.RawMessage) []string {
	var m struct {
		Capabilities struct {
			Exec json.RawMessage `json:"exec"`
		} `json:"capabilities"`
	}
	var exec struct {
		Commands []string `json:"commands"`
	}
	// exec "none", or absent, is not an object: nothing declared.
	if json.Unmarshal(raw, &m) != nil || json.Unmarshal(m.Capabilities.Exec, &exec) != nil {
		return nil
	}
	base := policy.BaseExecCommands()
	var foreign, archives []string
	for _, c := range exec.Commands {
		switch {
		case !slices.Contains(base, c):
			foreign = append(foreign, c)
		case slices.Contains(archiveReaders, c):
			archives = append(archives, c)
		}
	}
	var out []string
	if len(foreign) > 0 {
		out = append(out, fmt.Sprintf("capabilities.exec: %s: not a command every machine provides, so a run would be refused before it starts; declare only %s, or exec none",
			strings.Join(foreign, ", "), strings.Join(base, ", ")))
	}
	if len(archives) > 0 {
		out = append(out, fmt.Sprintf("capabilities.exec: %s: reads archives, which the runner unpacks before the run, each file in them read through the input tools; leave it out",
			strings.Join(archives, ", ")))
	}
	return out
}

// noteExecProblems says in the notes that the finished draft still
// declares commands its runs would refuse, so the person fixes them before
// running it.
func (d *Draft) noteExecProblems() {
	probs := execProblems(d.Manifest)
	if len(probs) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("## Commands to fix before a run\n\nThis draft declares commands its runs would refuse:\n\n")
	for _, p := range probs {
		b.WriteString("- " + p + "\n")
	}
	d.composeNotes(b.String())
}
