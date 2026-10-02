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
)

// maxProbeListing bounds what the probe may print.
const maxProbeListing = 1 << 20

// boundedBuffer keeps the first maxProbeListing bytes written to it and
// remembers that more came.
type boundedBuffer struct {
	buf      bytes.Buffer
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := maxProbeListing - b.buf.Len(); len(p) > room {
		b.overflow = true
		p = p[:max(room, 0)]
	}
	b.buf.Write(p)
	return len(p), nil
}

// Bytes is what was kept, or nil when the listing ran past its bound.
func (b *boundedBuffer) Bytes() []byte {
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
	var doc struct {
		Tools *[]string `json:"tools"`
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil || doc.Tools == nil {
		return nil, fmt.Errorf("the gateway probe's tool listing is not {\"tools\":[…]}: %v", err)
	}
	for _, n := range *doc.Tools {
		if n == "" || len(n) > 256 {
			return nil, fmt.Errorf("the gateway probe listed a tool name of %d bytes", len(n))
		}
	}
	return *doc.Tools, nil
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
