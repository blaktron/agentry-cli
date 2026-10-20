package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/drafter"
	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/knowledge/ship"
	"github.com/blaktron/agentry-cli/pkg/knowledge/store"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// draftKnowledge loads the packs `agentry draft --knowledge` names, as
// `agentry run --knowledge` does, held to the draft's bounds before
// anything else happens, and says for each where its scan comes from:
//
//   - a pack kept on this computer that was uploaded to or downloaded
//     from Agentry: its scan there, confirmed with the registry now
//     (store.AgentryScan); one that cannot be confirmed (signed out,
//     changed here since, not ready there) is scanned first;
//   - a pack directory, or a pack kept here only: not scanned, so scanned
//     first. A directory's manifest may carry a record, but anyone can
//     write one into a file, so it is never taken on this computer;
//   - with --hosted, only a pack directory, which a sandbox host laid down
//     from Agentry's store for this draft: its manifest's record is
//     Agentry's, as a hosted run's attached pack's is.
//
// The packs are held to the draft's bounds before any registry is asked.
//
// stored says for each pack whether it was opened from the store, and done
// removes the packs opened from the store.
func draftKnowledge(cmd *cobra.Command, args []string, hosted bool) (_ []drafter.KnowledgeIn, stored []bool, done func(), err error) {
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
				return nil, nil, done, engine.Classify(engine.ExitCLI, fmt.Errorf("--knowledge %s: a hosted draft takes the pack directories its sandbox host laid down: %w", arg, err))
			}
		} else {
			var cleanup func()
			if p, stored, cleanup, err = loadKnowledgeArg(cmd, arg, &kept); err != nil {
				return nil, nil, done, err
			}
			cleanups = append(cleanups, cleanup)
		}
		if err := onePackPerName(seen, p, arg); err != nil {
			return nil, nil, done, err
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
		return nil, nil, done, err
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
				return nil, nil, done, err
			}
		case p.Manifest.Scan.Scanned:
			out[i].Unconfirmed = "the scan record in its manifest.json cannot be confirmed for a pack directory"
		}
	}
	return out, fromStore, done, nil
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

// shipPackArgs are the packs a draft loads for --ship: each --ship value
// is a pack to read, as --knowledge's are, except in a hosted draft, where
// a value that is a bare pack name names one of the --knowledge directories
// its sandbox host laid down (by the name in its manifest.json). The
// directories are not repeated.
func shipPackArgs(o *draftOpts) (packArgs []string) {
	packArgs = slices.Clone(o.knowledge)
	for _, a := range o.ships {
		if o.hosted && hostedShipName(a) {
			continue
		}
		if !slices.Contains(packArgs, a) {
			packArgs = append(packArgs, a)
		}
	}
	return packArgs
}

// hostedShipName says a hosted --ship value names a pack rather than a
// directory.
func hostedShipName(a string) bool {
	return !knowledgeArgIsPath(a) && !isDir(a) && knowledge.ValidName(a) == nil
}

// draftShips resolves the --ship packs among the packs a draft loads: each
// is held to what a release ships (at most five packs, 10 MB and 2,000
// files each) and, for a pack kept on this computer, given the origin that
// `agentry knowledge ship` would record.
//
// An organization's pack ships into a QuickAgent in that organization's
// namespace, from its administrators: --namespace names the namespace, and
// the CLI checks it is the organization's, that the signed-in account
// administers the organization, and that the copy kept here is the pack as
// Agentry holds it now. Without --namespace the pack is refused. In a
// hosted draft the registry has made those checks for the job's account
// before the draft started, and a pack is a directory its host laid, with
// no origin the CLI can see.
func draftShips(cmd *cobra.Command, o *draftOpts, packArgs []string, packs []drafter.KnowledgeIn, stored []bool) ([]drafter.ShipIn, error) {
	if len(o.ships) > ship.MaxPacks {
		return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("--ship names %d packs; a release ships at most %d", len(o.ships), ship.MaxPacks))
	}
	errOut := cmd.ErrOrStderr()
	var out []drafter.ShipIn
	var kept *store.Store
	for _, arg := range o.ships {
		i := slices.Index(packArgs, arg)
		if o.hosted && hostedShipName(arg) {
			i = slices.IndexFunc(packs, func(k drafter.KnowledgeIn) bool { return k.Pack.Manifest.Name == arg })
			if i < 0 {
				return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("--ship %s names no --knowledge pack: a hosted draft ships a pack its host laid down with --knowledge", arg))
			}
		}
		if i < 0 || i >= len(packs) {
			return nil, fmt.Errorf("--ship %s was not loaded", arg)
		}
		p := packs[i].Pack
		if slices.ContainsFunc(out, func(s drafter.ShipIn) bool { return s.Pack == p }) {
			continue
		}
		if err := ship.Check(p); err != nil {
			return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("--ship %s: %w", arg, err))
		}
		in := drafter.ShipIn{Pack: p}
		if stored[i] {
			if kept == nil {
				var err error
				if kept, err = knowledgeStore(cmd); err != nil {
					return nil, err
				}
			}
			e, err := kept.Find(p.Manifest.Name)
			if err != nil {
				return nil, storeErr(err)
			}
			if e.Key.Org != "" {
				if in.Origin, err = orgShipOrigin(cmd, e, o.namespace); err != nil {
					return nil, err
				}
			} else {
				var note string
				in.Origin, note = ownOrigin(e)
				if note != "" {
					_, _ = fmt.Fprintln(errOut, note)
				}
			}
		}
		out = append(out, in)
		_, _ = fmt.Fprintf(errOut, "Knowledge pack %s ships with the QuickAgent: everyone who can download the release can read it. `agentry publish` asks you to confirm it, so check it holds no private data first.\n", p.Manifest.Name)
	}
	return out, nil
}

// orgShipOrigin lets a kept organization pack ship with a draft meant for
// the QuickAgent namespace ns, and answers its origin.
func orgShipOrigin(cmd *cobra.Command, e store.Entry, ns string) (*ship.Origin, error) {
	org, name := e.Key.Org, e.Name()
	switch {
	case ns == "":
		return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s is %s's shared pack, which ships only with a QuickAgent in %s's own namespace: name it with --namespace %s", name, org, org, org))
	case ns != org:
		return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s's pack %s ships only with a QuickAgent in %s's own namespace, and --namespace is %s", org, name, org, ns))
	case e.Key.Agentry == nil || e.Key.Registry != registryURL():
		return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s's pack %s was not kept from %s: download it from there with `agentry knowledge download --org %s %s`", org, name, registryURL(), org, name))
	}
	ctx := cmd.Context()
	reg := registryURL()
	token, err := signedInToken(cmd, reg)
	if err != nil {
		return nil, err
	}
	api, err := newAPIClient(reg, token)
	if err != nil {
		return nil, err
	}
	if err := requireOrgAdmin(ctx, api, org); err != nil {
		return nil, err
	}
	c, err := knowledgeClient(cmd, reg)
	if err != nil {
		return nil, err
	}
	packs, err := c.Shared(org).List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list %s's knowledge packs: %w", org, err)
	}
	i := slices.IndexFunc(packs, func(r store.Remote) bool { return r.ID == e.Key.Agentry.ID })
	switch {
	case i < 0:
		return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s no longer shares a pack %s in Agentry", org, name))
	case packs[i].State != store.StateReady:
		return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s's pack %s is %s in Agentry: it ships once its scan has finished", org, name, packs[i].State))
	case packs[i].Digest != e.Manifest.SHA256:
		return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s's pack %s changed in Agentry since it was kept here: download it again with `agentry knowledge download --org %s %s`", org, name, org, name))
	}
	return &ship.Origin{Kind: ship.KindOrg, Org: org, PackID: e.Key.Agentry.ID}, nil
}
