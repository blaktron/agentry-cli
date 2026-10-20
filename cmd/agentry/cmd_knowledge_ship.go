// `agentry knowledge ship`: lay a stored knowledge pack into a QuickAgent's
// package so the release carries it, and take one out again.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/knowledge/ship"
	"github.com/blaktron/agentry-cli/pkg/knowledge/store"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry/pkg/apiclient"
	"github.com/blaktron/agentry/pkg/manifest"
)

func newKnowledgeShipCmd() *cobra.Command {
	var org, dir string
	var remove bool
	cmd := &cobra.Command{
		Use:   "ship PACK [--org ORG] [--dir DIR] | ship --remove PACK",
		Short: "Ship a knowledge pack with a QuickAgent, so every run of it can read the pack",
		Long: "Lay the knowledge pack PACK into the QuickAgent package in DIR (default the working directory), so the\n" +
			"release carries it and every run of the QuickAgent reads it at /knowledge/PACK. PACK is a pack kept on\n" +
			"this computer, or your own pack in Agentry, which is downloaded first. The command copies the pack's\n" +
			"files into knowledge/PACK/ with the manifest.json the registry checks a shipped pack against, adds PACK\n" +
			"to knowledge.ships and knowledge/** to publish.include in agent.yaml, and records where the pack came\n" +
			"from in .agentry/knowledge-ships.json, which is not part of the release. Running it again on a shipped\n" +
			"pack replaces it with the store's current content.\n\n" +
			"A pack in a release is readable by everyone who can download the release (anyone, when the QuickAgent\n" +
			"is public), and it stays in that version for good. Nothing is published here: `agentry publish` asks you\n" +
			"to confirm each shipped pack. Check that a pack holds no private data first: names, emails, customer\n" +
			"records, internal hostnames, keys.\n\n" +
			"A release ships at most " + fmt.Sprint(ship.MaxPacks) + " packs, each at most " + knowledge.FormatCap(ship.MaxPackBytes) +
			" and " + fmt.Sprint(ship.MaxPackFiles) + " files; a QuickAgent that needs a larger pack asks for it with\n" +
			"knowledge.accepts and the person attaches it to the run.\n\n" +
			"With --org ORG, PACK is the knowledge pack ORG shares in Agentry. Only an administrator of ORG can ship\n" +
			"it, and only into a QuickAgent in ORG's own namespace (the namespace in agent.yaml); the registry checks\n" +
			"both when the release is published, and records the shipment in ORG's audit log.\n\n" +
			"--remove takes PACK out: its folder, its knowledge.ships entry and its record. `agentry draft --ship PACK`\n" +
			"lays a pack into a draft as it is made.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := knowledge.ValidName(args[0]); err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			if remove {
				if org != "" {
					return engine.Classify(engine.ExitUsage, errors.New("--org does not go with --remove"))
				}
				return runShipRemove(cmd, dir, args[0])
			}
			return runShip(cmd, dir, args[0], org)
		},
	}
	f := cmd.Flags()
	f.StringVar(&org, "org", "", "ship this organization's shared pack (its administrators only, into a QuickAgent in its namespace)")
	f.StringVar(&dir, "dir", ".", "the QuickAgent package to lay the pack into")
	f.BoolVar(&remove, "remove", false, "take the pack out of the package instead")
	return cmd
}

// shipSource is the pack to lay down, opened in a private directory, and
// where it came from.
type shipSource struct {
	pack   *knowledge.Pack
	origin *ship.Origin
	// note says why a pack has no origin, for the person.
	note string
	done func()
}

func runShip(cmd *cobra.Command, dir, name, org string) error {
	dir, m, err := shipPackage(dir)
	if err != nil {
		return err
	}
	ships := m.ShippedPacks()
	if !slices.Contains(ships, name) && len(ships) >= ship.MaxPacks {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%s already ships %d packs (%s), the most a release ships; take one out with `agentry knowledge ship --remove PACK` first",
			dir, len(ships), strings.Join(ships, ", ")))
	}
	if m.Major() != 2 {
		return engine.Classify(engine.ExitUsage, errors.New("agent.yaml is not schemaVersion 2: knowledge.ships is a schemaVersion 2 field"))
	}
	src, err := openShipSource(cmd, m, name, org)
	if err != nil {
		return err
	}
	defer src.done()
	if err := ship.Check(src.pack); err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	laid, err := layShipped(cmd, dir, src)
	if err != nil {
		return err
	}
	if flags.jsonOut {
		printJSON(cmd.OutOrStdout(), shipResult{Name: laid.Name, Dir: ship.Dir(dir, laid.Name), Pages: laid.Pages, Bytes: laid.Bytes, Files: laid.Files, SHA256: laid.SHA256, Origin: src.origin})
		return nil
	}
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(out, "knowledge pack %s laid under %s: %s, %s\n", laid.Name, filepath.Join(manifest.KnowledgeDir, laid.Name), knowledge.CountPages(laid.Pages), byteSize(laid.Bytes))
	if src.note != "" {
		_, _ = fmt.Fprintf(errOut, "%s\n", src.note)
	}
	_, _ = fmt.Fprintf(errOut, "It ships with the release once it is published: `agentry publish` asks you to confirm each pack, since everyone who can download the release can read it.\n")
	return nil
}

// shipResult is `knowledge ship --json`.
type shipResult struct {
	Name   string       `json:"name"`
	Dir    string       `json:"dir"`
	Pages  int          `json:"pages"`
	Bytes  int64        `json:"bytes"`
	Files  int          `json:"files"`
	SHA256 string       `json:"sha256"`
	Origin *ship.Origin `json:"origin,omitempty"`
}

// shipPackage is the package folder, absolute, and its manifest.
func shipPackage(dir string) (string, *manifest.Manifest, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", nil, err
	}
	m, err := readWorkspaceManifest(abs)
	if err != nil {
		return "", nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s holds no QuickAgent package to ship a pack with: %w", abs, err))
	}
	return abs, m, nil
}

// layShipped puts the pack under the package, edits agent.yaml and writes
// the sidecar: the three move together, and a failure after the folder is
// laid leaves it for a rerun to replace.
func layShipped(cmd *cobra.Command, dir string, src *shipSource) (ship.Laid, error) {
	ymlPath := filepath.Join(dir, "agent.yaml")
	raw, err := os.ReadFile(ymlPath) //nolint:gosec // the publisher's own package
	if err != nil {
		return ship.Laid{}, err
	}
	name := src.pack.Manifest.Name
	edit, err := ship.AddShip(raw, name)
	if err != nil {
		return ship.Laid{}, engine.Classify(engine.ExitUsage, err)
	}
	rec, err := ship.ReadRecord(dir)
	if err != nil {
		return ship.Laid{}, engine.Classify(engine.ExitUsage, err)
	}
	laid, err := ship.Lay(dir, src.pack)
	if err != nil {
		return ship.Laid{}, engine.Classify(engine.ExitUsage, err)
	}
	if len(edit.Did) > 0 {
		mode := os.FileMode(0o644)
		if fi, err := os.Stat(ymlPath); err == nil {
			mode = fi.Mode().Perm()
		}
		if err := os.WriteFile(ymlPath, edit.YAML, mode); err != nil { //nolint:gosec // the publisher's own package
			return ship.Laid{}, err
		}
		for _, d := range edit.Did {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "agent.yaml: %s\n", d)
		}
	}
	e := ship.Entry{Name: name, SHA256: laid.SHA256}
	if src.origin != nil {
		e.Origin, e.Registry = src.origin, registryURL()
	}
	rec.Set(e)
	if err := ship.WriteRecord(dir, rec); err != nil {
		return ship.Laid{}, err
	}
	return laid, nil
}

func runShipRemove(cmd *cobra.Command, dir, name string) error {
	dir, m, err := shipPackage(dir)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(ship.Dir(dir, name))
	if !slices.Contains(m.ShippedPacks(), name) && statErr != nil {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%s does not ship a pack named %s", dir, name))
	}
	ymlPath := filepath.Join(dir, "agent.yaml")
	raw, err := os.ReadFile(ymlPath) //nolint:gosec // the publisher's own package
	if err != nil {
		return err
	}
	edit, err := ship.RemoveShip(raw, name)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	rec, err := ship.ReadRecord(dir)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	if len(edit.Did) > 0 {
		mode := os.FileMode(0o644)
		if fi, err := os.Stat(ymlPath); err == nil {
			mode = fi.Mode().Perm()
		}
		if err := os.WriteFile(ymlPath, edit.YAML, mode); err != nil { //nolint:gosec // the publisher's own package
			return err
		}
	}
	if err := ship.Remove(dir, name); err != nil {
		return err
	}
	rec.Remove(name)
	if err := ship.WriteRecord(dir, rec); err != nil {
		return err
	}
	if flags.jsonOut {
		printJSON(cmd.OutOrStdout(), map[string]any{"name": name, "removed": true, "edits": edit.Did})
		return nil
	}
	for _, d := range edit.Did {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "agent.yaml: %s\n", d)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "knowledge pack %s taken out of %s\n", name, dir)
	return nil
}

// openShipSource finds the pack to lay down: an organization's pack from
// Agentry, or one kept on this computer (downloaded first when it is only
// in Agentry), opened in a private directory.
func openShipSource(cmd *cobra.Command, m *manifest.Manifest, name, org string) (*shipSource, error) {
	ctx := cmd.Context()
	s, err := knowledgeStore(cmd)
	if err != nil {
		return nil, err
	}
	if org == "" {
		e, err := s.Find(name)
		if errors.Is(err, store.ErrNotFound) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "No pack named %s is kept on this computer; downloading your copy from %s.\n", name, registryURL())
			if e, err = s.Download(ctx, name); err != nil {
				return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("no knowledge pack named %s is kept here or in Agentry for your account (an organization's pack: --org ORG): %w", name, err))
			}
		} else if err != nil {
			return nil, storeErr(err)
		}
		org = e.Key.Org
		if org == "" {
			return openOwnShipSource(cmd, s, e)
		}
	}
	return openOrgShipSource(cmd, s, m, name, org)
}

func openOwnShipSource(cmd *cobra.Command, s *store.Store, e store.Entry) (*shipSource, error) {
	p, done, err := s.OpenScratch(cmd.Context(), e.Name())
	if err != nil {
		return nil, engine.Classify(engine.ExitPolicy, err)
	}
	src := &shipSource{pack: p, done: done}
	src.origin, src.note = ownOrigin(e)
	return src, nil
}

// openOrgShipSource reads org's shared pack through the registry's export,
// which answers an administrator, after the two rules of shipping it have
// been checked: the caller is an administrator of org, and the package is
// in org's namespace.
func openOrgShipSource(cmd *cobra.Command, s *store.Store, m *manifest.Manifest, name, org string) (*shipSource, error) {
	ctx := cmd.Context()
	if m.Namespace != org {
		return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s's pack ships only with a QuickAgent in %s's own namespace, and this package is %s/%s", org, org, m.Namespace, m.Name))
	}
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
	sh := c.Shared(org)
	packs, err := sh.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list %s's knowledge packs: %w", org, err)
	}
	i := slices.IndexFunc(packs, func(r store.Remote) bool { return r.Name == name })
	if i < 0 {
		return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s shares no knowledge pack named %s in Agentry (`agentry knowledge list --org %s` lists them)", org, name, org))
	}
	r := packs[i]
	if r.State != store.StateReady {
		return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s's pack %s is %s in Agentry: it ships once its scan has finished", org, name, r.State))
	}
	tree, digest, err := sh.Export(ctx, r.ID)
	if err != nil {
		return nil, fmt.Errorf("export %s's pack %s: %w", org, name, err)
	}
	if digest == "" || digest != r.Digest {
		return nil, fmt.Errorf("%s's pack %s: the tree Agentry sent is not the one it describes (%q, not %q)", org, name, digest, r.Digest)
	}
	scratch, done, err := s.Scratch()
	if err != nil {
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	p, err := knowledge.Unpack(bytes.NewReader(tree), digest, filepath.Join(scratch, name),
		knowledge.Manifest{Name: r.Name, Created: r.Created.UTC().Truncate(time.Second), Updated: r.Updated.UTC().Truncate(time.Second)})
	if err != nil {
		done()
		return nil, fmt.Errorf("%s's pack %s: %w", org, name, err)
	}
	return &shipSource{pack: p, done: done, origin: &ship.Origin{Kind: ship.KindOrg, Org: org, PackID: r.ID}}, nil
}

// requireOrgAdmin refuses unless the signed-in account is an active
// administrator of org.
func requireOrgAdmin(ctx context.Context, api *apiclient.ClientWithResponses, org string) error {
	me, err := api.GetCurrentUserWithResponse(ctx)
	if err != nil {
		return err
	}
	if me.JSON200 == nil {
		return apiProblem("look up your account", me.StatusCode(), me.ApplicationproblemJSONDefault, me.Body)
	}
	members, err := api.ListOrgMembersWithResponse(ctx, org)
	if err != nil {
		return err
	}
	if members.JSON200 == nil {
		return apiProblem("list the members of "+org, members.StatusCode(), members.ApplicationproblemJSONDefault, members.Body)
	}
	for _, mb := range *members.JSON200 {
		active := mb.State == nil || *mb.State == apiclient.MembershipStateActive
		if mb.User.Handle == me.JSON200.Handle && mb.Role == apiclient.OrgRoleAdmin && active {
			return nil
		}
	}
	return engine.Classify(engine.ExitUsage, fmt.Errorf("only an administrator of %s can ship its knowledge pack, and %s is not one", org, me.JSON200.Handle))
}

// ownOrigin is the origin to record for one of the person's own kept packs:
// its copy in Agentry, when it has one at this registry that is the pack as
// it stands. Without one it says why none is recorded, and the release
// ships the pack as the person's own files.
func ownOrigin(e store.Entry) (*ship.Origin, string) {
	switch a := e.Key.Agentry; {
	case a == nil:
		return nil, fmt.Sprintf("%s is kept only on this computer, so the release ships it as your own files with no origin recorded. Upload it first (`agentry knowledge upload %s`) and ship it again to record the pack in Agentry as its origin.", e.Name(), e.Name())
	case a.Registry != registryURL():
		return nil, fmt.Sprintf("%s's copy in Agentry is at %s, not %s, so no origin is recorded.", e.Name(), a.Registry, registryURL())
	case a.Digest != e.Manifest.SHA256:
		return nil, fmt.Sprintf("%s changed on this computer since its upload to Agentry, so no origin is recorded: the release ships it as your own files. Upload it (`agentry knowledge upload %s`) and ship it again to record the origin.", e.Name(), e.Name())
	default:
		return &ship.Origin{Kind: ship.KindOwn, PackID: a.ID}, ""
	}
}
