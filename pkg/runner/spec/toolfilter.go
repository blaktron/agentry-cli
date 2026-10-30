package spec

import (
	"fmt"
	"slices"
	"strings"
)

// CaseOnlyMatch is a tool filter entry that names a real tool only when case
// is ignored: the gateway compares a server's tool names with its allowlist
// exactly, so the entry matches nothing and the tool is dropped without a word.
type CaseOnlyMatch struct {
	// Entry is the filter entry as the manifest writes it; Tool is the exact
	// name the server lists, which the filter must use.
	Entry, Tool string
}

// ToolFilterDiff is how a declared server's tool filter reads against the
// tool names the server lists.
type ToolFilterDiff struct {
	// CaseOnly are the entries that match a listed tool only ignoring case.
	CaseOnly []CaseOnlyMatch
	// Absent are the entries that match no listed tool even ignoring case.
	Absent []string
}

// Empty says every entry names a listed tool exactly.
func (d ToolFilterDiff) Empty() bool { return len(d.CaseOnly) == 0 && len(d.Absent) == 0 }

// DiffToolFilter reads a tool filter against the names a server lists. An
// entry equal to a listed name is fine; one equal to a listed name only when
// case is ignored is a CaseOnly match (naming the first such listed name in
// sorted order); any other is Absent. Entries come back in sorted order, each
// once.
func DiffToolFilter(filter, listed []string) ToolFilterDiff {
	var d ToolFilterDiff
	names := slices.Compact(slices.Sorted(slices.Values(listed)))
	for _, entry := range slices.Compact(slices.Sorted(slices.Values(filter))) {
		if slices.Contains(names, entry) {
			continue
		}
		if i := slices.IndexFunc(names, func(n string) bool { return strings.EqualFold(n, entry) }); i >= 0 {
			d.CaseOnly = append(d.CaseOnly, CaseOnlyMatch{Entry: entry, Tool: names[i]})
			continue
		}
		d.Absent = append(d.Absent, entry)
	}
	return d
}

// maxQuotedTool bounds a tool name quoted in a message: names come from the
// server or the manifest, never from the runner.
const maxQuotedTool = 128

func quoteTool(name string) string {
	if len(name) > maxQuotedTool {
		name = name[:maxQuotedTool] + "..."
	}
	return fmt.Sprintf("%q", name)
}

// CaseMessage says, for each case-only entry, which exact name to write. It
// is "" when there is none.
func (d ToolFilterDiff) CaseMessage() string {
	parts := make([]string, 0, len(d.CaseOnly))
	for _, m := range d.CaseOnly {
		parts = append(parts, fmt.Sprintf("%s matches the server's tool %s only when case is ignored, and the gateway compares tool names exactly, so the tool would be dropped without a word: write %s", quoteTool(m.Entry), quoteTool(m.Tool), quoteTool(m.Tool)))
	}
	return strings.Join(parts, "; ")
}
