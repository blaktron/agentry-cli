// The publisher's confirmation of the knowledge packs a release ships:
// `agentry publish` lists each one with its size, asks about it, and sends
// the registry the confirmation with the origins `agentry knowledge ship`
// recorded.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/knowledge/ship"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry/pkg/apiclient"
	"github.com/blaktron/agentry/pkg/manifest"
)

// shippingField is the multipart field that carries the confirmation.
const shippingField = "knowledgeShipping"

// shippingPart is the confirmation as the registry takes it: one entry for
// each pack the release ships, no more and no fewer.
type shippingPart struct {
	Packs []ship.Pack `json:"packs"`
}

// writeTo adds the confirmation to an upload.
func (s *shippingPart) writeTo(mw *multipart.Writer) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q`, shippingField))
	h.Set("Content-Type", "application/json")
	w, err := mw.CreatePart(h)
	if err != nil {
		return fmt.Errorf("write %s part: %w", shippingField, err)
	}
	_, err = w.Write(b)
	return err
}

// confirmShipping decides the confirmation of the packs an upload ships:
// nil when it ships none, an error when the publisher declines.
type confirmShipping func(packs []string) (*shippingPart, error)

// builtinShipping is the confirmation of a built-in's packs: they are the
// platform's own, published by the release, so nothing is asked.
func builtinShipping(packs []string) (*shippingPart, error) {
	if len(packs) == 0 {
		return nil, nil
	}
	s := &shippingPart{Packs: make([]ship.Pack, 0, len(packs))}
	for _, n := range packs {
		s.Packs = append(s.Packs, ship.Pack{Name: n})
	}
	return s, nil
}

// publishShipping asks the publisher about each pack of the package in
// dir that the release ships, and answers the confirmation with the
// origins `agentry knowledge ship` recorded.
func publishShipping(cmd *cobra.Command, client *apiclient.ClientWithResponses, dir string, m *manifest.Manifest, reg string, yes bool) confirmShipping {
	return func(packs []string) (*shippingPart, error) {
		if len(packs) == 0 {
			return nil, nil
		}
		public := !agentIsPrivate(cmd.Context(), client, m)
		rec, err := ship.ReadRecord(dir)
		if err != nil {
			return nil, err
		}
		part := &shippingPart{Packs: make([]ship.Pack, 0, len(packs))}
		errOut := cmd.ErrOrStderr()
		for _, name := range packs {
			n, _, err := ship.DirSize(dir, name)
			if err != nil {
				return nil, fmt.Errorf("measure the shipped pack %s: %w", name, err)
			}
			if err := confirmPack(cmd, name, byteSize(n), public, yes); err != nil {
				return nil, err
			}
			origin, why := ship.OriginOf(dir, rec, name, reg)
			if why != "" {
				_, _ = fmt.Fprintf(errOut, "%s: no origin is claimed, because %s; it is sent as your own files.\n", name, why)
			}
			part.Packs = append(part.Packs, ship.Pack{Name: name, Origin: origin})
		}
		return part, nil
	}
}

// confirmTag asks the same questions of a release published from a tag,
// which carries no confirmation to the registry: the publisher is still
// asked, and the registry records the version as published from a tag.
func confirmTag(cmd *cobra.Command, dir string, m *manifest.Manifest, client *apiclient.ClientWithResponses, yes bool) error {
	packs := m.ShippedPacks()
	if len(packs) == 0 {
		return nil
	}
	public := !agentIsPrivate(cmd.Context(), client, m)
	for _, name := range packs {
		sz := "size unknown"
		if n, _, err := ship.DirSize(dir, name); err == nil {
			sz = byteSize(n)
		}
		if err := confirmPack(cmd, name, sz, public, yes); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "A release submitted from a tag carries no confirmation to the registry: it records the version as published from a tag.")
	return nil
}

// agentIsPrivate says the registry knows the QuickAgent and it is private.
// A QuickAgent the registry cannot be asked about is taken to be public,
// the case that warns more.
func agentIsPrivate(ctx context.Context, client *apiclient.ClientWithResponses, m *manifest.Manifest) bool {
	res, err := client.GetAgentWithResponse(ctx, m.Namespace, m.Name)
	return err == nil && res.JSON200 != nil && res.JSON200.Visibility == apiclient.AgentVisibilityPrivate
}

// shippingWarning is what a publisher is told about a pack before it ships
// in a release: who can read it, that it stays, and what to check.
func shippingWarning(pack, size string, public bool) string {
	var first string
	if public {
		first = fmt.Sprintf("%s (%s) becomes public. Anyone can download it with this release", pack, size)
	} else {
		first = fmt.Sprintf("Everyone who can see this QuickAgent can read %s (%s). Anyone who can see it can download the pack with this release", pack, size)
	}
	return first + ", and it stays in this version for good: retiring or deleting the QuickAgent later does not recall copies already downloaded. " +
		"Check that it holds no private data: names, emails, customer records, internal hostnames, keys."
}

// confirmPack asks about one pack, or, with --yes, says what it answered.
func confirmPack(cmd *cobra.Command, name, size string, public, yes bool) error {
	warning := shippingWarning(name, size, public)
	errOut := cmd.ErrOrStderr()
	_, _ = fmt.Fprintln(errOut, warning)
	if yes {
		_, _ = fmt.Fprintf(errOut, "Confirmed with --yes: %s ships with this release.\n", name)
		return nil
	}
	if err := promptYes(cmd, fmt.Sprintf("Ship %s with this release? Type \"yes\": ", name),
		fmt.Sprintf("%s ships in this release only if you confirm it: answer on a terminal, or pass --yes", name),
		fmt.Sprintf("publish cancelled: %s was not confirmed, and nothing was uploaded", name)); err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	return nil
}

// stripSidecar refuses a release that would carry the sidecar: its pack
// ids are the publisher's, not part of the release.
func stripSidecar(files []manifest.FileInfo) error {
	for _, f := range files {
		if f.Path == ship.RecordFile {
			return fmt.Errorf("publish.include puts %s in the release; it records where shipped packs came from and stays in your package, so narrow publish.include to leave out %s", ship.RecordFile, filepath.Dir(ship.RecordFile)+"/")
		}
	}
	return nil
}

// namePacks adds the packs a refused knowledge confirmation is about to the
// refusal, from the problem's own list, so the publisher sees which pack
// the registry would not take.
func namePacks(err error, body []byte) error {
	var p struct {
		Packs []string `json:"packs"`
	}
	if json.Unmarshal(body, &p) != nil || len(p.Packs) == 0 {
		return err
	}
	names := make([]string, 0, len(p.Packs))
	for _, n := range p.Packs {
		names = append(names, safeTerminalText(n, 64))
	}
	return fmt.Errorf("%w [knowledge packs: %s]", err, strings.Join(names, ", "))
}
