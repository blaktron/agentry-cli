package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// attachKnowledge loads and verifies each pack --knowledge names (plan
// knowledge.md §4.5), before anything is resolved or downloaded, and says
// on stderr what will be mounted — a pack made on a machine says it is not
// scanned. A pack whose files no longer match its digest is the verify
// class; anything else wrong with one is a usage error. The engine copies
// each pack into the run and verifies the copy again.
func attachKnowledge(errOut io.Writer, dirs []string) ([]engine.KnowledgeMount, error) {
	var out []engine.KnowledgeMount
	seen := map[string]string{}
	for _, dir := range dirs {
		p, err := knowledge.Load(dir)
		if err != nil {
			if errors.Is(err, knowledge.ErrDigestMismatch) {
				return nil, engine.Classify(engine.ExitVerify, fmt.Errorf("--knowledge %s: %w", dir, err))
			}
			return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("--knowledge %s: %w", dir, err))
		}
		name := p.Manifest.Name
		if prev, dup := seen[name]; dup {
			return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("--knowledge %s and %s are both packs named %s; a run mounts each pack at /knowledge/<name>", prev, dir, name))
		}
		seen[name] = dir
		m := engine.Attach(p)
		pages := fmt.Sprintf("%d pages", m.Pages)
		if m.Pages == 1 {
			pages = "1 page"
		}
		_, _ = fmt.Fprintf(errOut, "knowledge pack %s: %s, %d bytes, %s, %s — mounted read-only at /knowledge/%s\n",
			name, pages, m.Bytes, m.Digest, m.Scan, name)
		out = append(out, m)
	}
	return out, nil
}
