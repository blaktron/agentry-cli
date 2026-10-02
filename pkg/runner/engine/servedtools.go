// What a session's driver permits beyond the run's own grant
// (agentry-cli#578). A declared MCP server's tools are named by the server
// at run time unless its manifest filters them, and docker-agent asks the
// client's permission for every tool not marked read-only, so the session
// asks the gateway what it serves (the probe's --list-tools, from inside
// the run network) and permits that too. The gateway's allowlist stays the
// enforcement; the permit only stops refusing what the gateway routes.
package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/blaktron/agentry-cli/runnerimage/probe"
)

// maxProbeListing bounds what the probe may print.
const maxProbeListing = 1 << 20

// probeOutput keeps the first maxProbeListing bytes the probe prints and
// remembers that more came. It takes every write whole, as io.Writer
// requires, and drops what is past the bound.
type probeOutput struct {
	buf      bytes.Buffer
	overflow bool
}

func (b *probeOutput) Write(p []byte) (int, error) {
	keep := p
	if room := maxProbeListing - b.buf.Len(); len(keep) > room {
		b.overflow = true
		keep = keep[:max(room, 0)]
	}
	b.buf.Write(keep)
	return len(p), nil
}

// Bytes is what was kept, or nil when the listing ran past its bound.
func (b *probeOutput) Bytes() []byte {
	if b.overflow {
		return nil
	}
	return b.buf.Bytes()
}

// parseServedTools reads the probe's one line, {"tools":[…]}. No line, a
// line that is not that object, or a name that is empty or past 256 bytes
// is an error: a session that cannot tell what the gateway serves would
// refuse tools its contract grants.
func parseServedTools(out []byte) ([]string, error) {
	line := bytes.TrimSpace(out)
	if len(line) == 0 {
		return nil, errors.New("the gateway probe printed no tool listing")
	}
	var doc probe.Listing
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil || doc.Tools == nil {
		return nil, fmt.Errorf("the gateway probe's tool listing is not {\"tools\":[…]}: %v", err)
	}
	if dec.More() {
		return nil, errors.New("the gateway probe printed more than its one tool listing")
	}
	for _, n := range doc.Tools {
		if n == "" || len(n) > probe.MaxToolName {
			return nil, fmt.Errorf("the gateway probe listed a tool name of %d bytes", len(n))
		}
	}
	return doc.Tools, nil
}

// permitted is the run's own grant, then each tool the gateway serves that
// the grant does not already name.
func permitted(granted, served []string) []string {
	out := slices.Clone(granted)
	for _, n := range served {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}
