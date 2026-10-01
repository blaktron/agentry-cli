package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/drafter"
	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/knowledge/store"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// draftKnowledge loads the packs `agentry draft --knowledge` names (plan
// knowledge-and-drafter.md §6, D3, D14; agentry-cli#522), as `agentry run
// --knowledge` does, held to the draft's bounds before anything else
// happens, and says for each where its scan comes from:
//
//   - a pack kept on this computer that was uploaded to or downloaded
//     from Agentry: its scan there, confirmed with the registry now
//     (store.AgentryScan); one that cannot be confirmed (signed out,
//     changed here since, not ready there) is scanned first;
//   - a pack directory, or a pack kept here only: not scanned, so scanned
//     first. A directory's manifest may carry a record, but anyone can
//     write one into a file, so it is never taken on this computer;
//   - with --hosted, only a pack directory, which a sandbox host laid down
//     from Agentry's store for this draft (agentry-enterprise#295): its
//     manifest's record is Agentry's, as a hosted run's attached pack's is.
//
// The packs are held to the draft's bounds before any registry is asked.
//
// done removes the packs opened from the store.
func draftKnowledge(cmd *cobra.Command, args []string, hosted bool) (_ []drafter.KnowledgeIn, done func(), err error) {
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
	var out []drafter.KnowledgeIn
	var fromStore []bool
	seen := map[string]string{}
	var kept *store.Store
	for _, arg := range args {
		var p *knowledge.Pack
		stored := false
		if hosted {
			// A sandbox host lays each pack down as a directory with
			// Agentry's record; nothing else is taken as Agentry's.
			if p, err = knowledge.Load(arg); err != nil {
				return nil, done, engine.Classify(engine.ExitCLI, fmt.Errorf("--knowledge %s: a hosted draft takes the pack directories its sandbox host laid down: %w", arg, err))
			}
		} else {
			var cleanup func()
			if p, stored, cleanup, err = loadKnowledgeArg(cmd, arg, &kept); err != nil {
				return nil, done, err
			}
			cleanups = append(cleanups, cleanup)
		}
		if err := onePackPerName(seen, p, arg); err != nil {
			return nil, done, err
		}
		out = append(out, drafter.KnowledgeIn{Pack: p})
		fromStore = append(fromStore, stored)
		if len(out) > drafter.MaxKnowledgePacks {
			break
		}
	}
	packs := make([]*knowledge.Pack, 0, len(out))
	for _, in := range out {
		packs = append(packs, in.Pack)
	}
	if err := drafter.CheckKnowledgeSize(packs); err != nil {
		return nil, done, err
	}
	// Only then is a registry asked about any of them.
	for i := range out {
		p := out[i].Pack
		switch {
		case hosted:
			if p.Manifest.Scan.Scanned {
				out[i].Agentry = p.Manifest.Scan.Report
			}
		case fromStore[i]:
			rec, err := kept.AgentryScan(cmd.Context(), p.Manifest.Name, p.Manifest.SHA256)
			switch {
			case err == nil:
				out[i].Agentry = rec
			case errors.Is(err, store.ErrScanUnconfirmed):
				out[i].Unconfirmed = strings.TrimPrefix(err.Error(), "knowledge pack "+p.Manifest.Name+": ")
			default:
				return nil, done, err
			}
		case p.Manifest.Scan.Scanned:
			out[i].Unconfirmed = "the scan record in its manifest.json cannot be confirmed for a pack directory"
		}
	}
	return out, done, nil
}

// noteKnowledge says, before anything is scanned, what the draft will do
// with each pack: read it as Agentry scanned it, or scan it first.
func noteKnowledge(cmd *cobra.Command, in []drafter.KnowledgeIn, hosted bool) {
	errOut := cmd.ErrOrStderr()
	with := " with your key"
	if hosted {
		with = " through the registry"
	}
	for _, k := range in {
		m := k.Pack.Manifest
		if ok, _ := drafter.AgentryScanStandsIn(k.Pack, k.Agentry); ok {
			_, _ = fmt.Fprintf(errOut, "Knowledge pack %s: %d pages, scanned in Agentry; what that scan flagged is withheld from the draft.\n", m.Name, m.Pages)
			continue
		}
		_, _ = fmt.Fprintf(errOut, "Knowledge pack %s: %d pages, not scanned in Agentry. Your knowledge will be scanned first: AI Review reads every file\n"+
			"of it%s before the draft starts, and what it flags is withheld from the draft.\n", m.Name, m.Pages, with)
	}
}
