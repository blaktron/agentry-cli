package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/knowledge/store"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// attachKnowledge loads and verifies each pack --knowledge names (plan
// knowledge.md §4.5), before anything is resolved or downloaded, and says
// on stderr what will be mounted — a pack made on a machine says it is not
// scanned. A value is a pack directory, or the name of a pack kept sealed
// in the local store (agentry-cli#319), which is opened into a private
// directory that done removes at teardown. A bare name is the store's
// first; a directory of that name in the working directory is taken only
// when the store keeps no such pack.
//
// For a directory, files that no longer match its digest are the verify
// class and anything else wrong is a usage error. A kept pack that is not
// there or will not open (signed out of the registry that wraps its key,
// another account's, altered) is a plan-time refusal, the policy class,
// naming the pack. The engine copies each pack into the run and verifies
// the copy again.
func attachKnowledge(cmd *cobra.Command, args []string) (_ []engine.AttachedPack, done func(), err error) {
	errOut := cmd.ErrOrStderr()
	var cleanups []func()
	done = func() {
		for _, f := range cleanups {
			f()
		}
	}
	defer func() {
		if err != nil {
			done()
		}
	}()
	var out []engine.AttachedPack
	seen := map[string]string{}
	var kept *store.Store
	for _, arg := range args {
		var p *knowledge.Pack
		if !keptPackNamed(cmd, arg) && (knowledgeArgIsPath(arg) || isDir(arg)) {
			if p, err = knowledge.Load(arg); err != nil {
				if errors.Is(err, knowledge.ErrDigestMismatch) {
					return nil, done, engine.Classify(engine.ExitVerify, fmt.Errorf("--knowledge %s: %w", arg, err))
				}
				return nil, done, engine.Classify(engine.ExitCLI, fmt.Errorf("--knowledge %s: %w", arg, err))
			}
		} else {
			if err := knowledge.ValidName(arg); err != nil {
				return nil, done, engine.Classify(engine.ExitCLI, fmt.Errorf("--knowledge %s: not a directory, and %w", arg, err))
			}
			if kept == nil {
				if kept, err = knowledgeStore(cmd); err != nil {
					return nil, done, err
				}
			}
			var cleanup func()
			if p, cleanup, err = kept.OpenScratch(cmd.Context(), arg); err != nil {
				return nil, done, engine.Classify(engine.ExitPolicy, fmt.Errorf("--knowledge %s: %w", arg, err))
			}
			cleanups = append(cleanups, cleanup)
		}
		name := p.Manifest.Name
		if prev, dup := seen[name]; dup {
			return nil, done, engine.Classify(engine.ExitCLI, fmt.Errorf("--knowledge %s and %s are both packs named %s; a run mounts each pack at %s/<name>", prev, arg, name, spec.KnowledgeMount))
		}
		seen[name] = arg
		a := engine.AttachPack(p)
		_, _ = fmt.Fprintf(errOut, "knowledge pack %s: %s, %d bytes, %s, %s — mounted read-only at %s/%s\n",
			name, knowledge.CountPages(a.Pages), a.Bytes, a.Digest, a.Scan.Describe(), spec.KnowledgeMount, name)
		out = append(out, a)
	}
	return out, done, nil
}
