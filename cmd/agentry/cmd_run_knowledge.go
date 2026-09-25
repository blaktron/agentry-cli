package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// attachKnowledge loads and verifies each pack --knowledge names (plan
// knowledge.md §4.5), before anything is resolved or downloaded, and says
// on stderr what will be mounted — a pack made on a machine says it is not
// scanned. A pack whose files no longer match its digest is the verify
// class; anything else wrong with one is a usage error. The engine copies
// each pack into the run and verifies the copy again.
func attachKnowledge(errOut io.Writer, dirs []string) ([]engine.AttachedPack, error) {
	var out []engine.AttachedPack
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
			return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("--knowledge %s and %s are both packs named %s; a run mounts each pack at %s/<name>", prev, dir, name, spec.KnowledgeMount))
		}
		seen[name] = dir
		a := engine.AttachPack(p)
		_, _ = fmt.Fprintf(errOut, "knowledge pack %s: %s, %d bytes, %s, %s — mounted read-only at %s/%s\n",
			name, knowledge.CountPages(a.Pages), a.Bytes, a.Digest, a.Scan.Describe(), spec.KnowledgeMount, name)
		out = append(out, a)
	}
	return out, nil
}
