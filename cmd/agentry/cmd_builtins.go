// `agentry builtins`: the packages this binary embeds and runs as builtin —
// the Knowledge Creator, the Drafter, and any added to localrun.Builtins()
// — each with the digest of its release archive. The registry holds every
// one of them as these same bytes, so a person reads a built-in's
// instruction in the directory and checks it against their own binary with
// the digest printed here.
//
// `agentry builtins publish` is what the CLI's tag release runs to put them
// there. Only the registry's owner can: the built-ins live in their
// organization's namespace, and the registry refuses a publish to it from
// any account without that right, so an ordinary user cannot publish a
// built-in on a registry they do not own.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry/pkg/apiclient"
	"github.com/blaktron/agentry/pkg/manifest"
)

func newBuiltinsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "builtins",
		Short: "List the packages built into this CLI, with the digests of their registry releases",
		Long: "List the QuickAgent packages this binary embeds and runs as built-ins\n" +
			"(the Knowledge Creator behind `agentry knowledge`, the Drafter behind\n" +
			"`agentry draft`), each with the digest of its release archive.\n\n" +
			"Every built-in is published to the registry from these bytes, so its\n" +
			"page in the directory shows the package that runs, and the release's\n" +
			"digest there is the one printed here.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return listBuiltins(cmd.OutOrStdout())
		},
	}
	cmd.AddCommand(newBuiltinsPublishCmd())
	return cmd
}

// builtinEntry is one line of `agentry builtins --json`.
type builtinEntry struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

func listBuiltins(out io.Writer) error {
	var entries []builtinEntry
	for _, b := range localrun.Builtins() {
		d, err := b.ArchiveDigest()
		if err != nil {
			return err
		}
		entries = append(entries, builtinEntry{Ref: b.Ref(), Digest: d})
	}
	if flags.jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}
	for _, e := range entries {
		_, _ = fmt.Fprintf(out, "%s\t%s\n", e.Ref, e.Digest)
	}
	return nil
}

func newBuiltinsPublishCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "publish",
		Short: "Publish every built-in package to the registry (for the registry's owner)",
		Long: "Publish every package this binary embeds to the registry, public, under\n" +
			"its own namespace, from the embedded bytes: the upload claims the\n" +
			"archive's digest and the registry's own build must equal it.\n\n" +
			"Each agent is marked built-in in the registry, which the directory\n" +
			"labels Built-in in place of its run modes; the mark is platform\n" +
			"staff's, so the token must be a staff admin's.\n\n" +
			"A version the registry already holds with the same digest is skipped;\n" +
			"one it holds with a different digest is an error, since a published\n" +
			"version is never rebuilt. A version that goes to review publishes when\n" +
			"a reviewer approves it. Exits non-zero when any built-in failed.\n\n" +
			"The built-ins live in their organization's namespace, so only an account\n" +
			"that may publish there (the registry owner's) can publish them; the\n" +
			"registry refuses anyone else.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBuiltinsPublish(cmd)
		},
	}
}

func runBuiltinsPublish(cmd *cobra.Command) error {
	reg := registryURL()
	token := credentialFor(reg)
	if token == "" {
		return fmt.Errorf("no credential for %s — run `agentry login` or set %s", reg, envToken)
	}
	client, err := newAPIClientChecked(cmd, reg, token)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	var failed []string
	for _, b := range localrun.Builtins() {
		if err := publishBuiltin(cmd.Context(), client, reg, token, out, b); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", b.Ref(), err)
			failed = append(failed, b.Ref())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d built-ins not published: %s", len(failed), len(localrun.Builtins()), strings.Join(failed, ", "))
	}
	return nil
}

// publishBuiltin publishes one built-in unless the registry already holds
// its version. First it makes sure the agent record exists, public, and is
// marked built-in, so that a version already published is labelled too.
func publishBuiltin(ctx context.Context, client *apiclient.ClientWithResponses, reg, token string, out io.Writer, b localrun.Builtin) error {
	want, err := b.ArchiveDigest()
	if err != nil {
		return err
	}
	ns, name, version := b.Namespace(), b.Name(), b.Version()

	stage, err := os.MkdirTemp("", "agentry-builtin-publish-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if err := b.Stage(stage); err != nil {
		return err
	}
	m, err := localrun.ManifestFrom(stage)
	if err != nil {
		return err
	}
	if err := ensureBuiltinAgent(ctx, client, out, m); err != nil {
		return err
	}

	vr, err := client.GetVersionInfoWithResponse(ctx, ns, name, version)
	if err != nil {
		return err
	}
	switch {
	case vr.JSON200 != nil:
		have := ""
		if vr.JSON200.ArchiveDigest != nil {
			have = *vr.JSON200.ArchiveDigest
		}
		if have != want {
			return fmt.Errorf("the registry holds this version as %s and this binary's package packs to %s: a published version is never rebuilt, so the built-in needs a new version", have, want)
		}
		switch vr.JSON200.Status {
		case "published", "needs_review", "submitted", "scanning", "approved":
			_, _ = fmt.Fprintf(out, "%s already in the registry (%s, %s)\n", b.Ref(), vr.JSON200.Status, have)
			return nil
		default:
			// rejected, quarantined, blocked, yanked: the version string is
			// burned, and the directory does not show what this binary runs.
			return fmt.Errorf("the registry holds this version in state %s: the built-in needs a new version", vr.JSON200.Status)
		}
	case vr.StatusCode() != http.StatusNotFound:
		return apiProblem("look up "+b.Ref(), vr.StatusCode(), vr.ApplicationproblemJSONDefault, vr.Body)
	}

	resp, err := submitUpload(ctx, client, out, stage, m)
	if err != nil {
		return err
	}
	if resp.JSON202 == nil {
		return apiProblem("submit "+b.Ref(), resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.Body)
	}
	v := resp.JSON202
	if v.ArchiveDigest != nil && *v.ArchiveDigest != want {
		return fmt.Errorf("the registry built %s from the upload, not %s", *v.ArchiveDigest, want)
	}
	return waitVerdict(ctx, client, httpClient, reg, token, out, ns, name, v)
}

// ensureBuiltinAgent creates the built-in's agent record, public, when the
// registry has none, and marks it built-in when it is not: the directory
// then labels it Built-in in place of its run modes. The namespace is the
// organization that owns the built-ins, which the publishing token must be
// able to publish to; the mark is platform staff's alone, so the release's
// token is a staff admin's, and a registry that refuses the mark fails
// this built-in.
func ensureBuiltinAgent(ctx context.Context, client *apiclient.ClientWithResponses, out io.Writer, m *manifest.Manifest) error {
	ar, err := client.GetAgentWithResponse(ctx, m.Namespace, m.Name)
	if err != nil {
		return err
	}
	if ar.JSON200 != nil {
		if ar.JSON200.Builtin {
			return nil
		}
		return markBuiltin(ctx, client, out, m)
	}
	if ar.StatusCode() != http.StatusNotFound {
		return apiProblem("look up "+m.Namespace+"/"+m.Name, ar.StatusCode(), ar.ApplicationproblemJSONDefault, ar.Body)
	}
	var modes apiclient.AgentModes
	for _, md := range m.SupportedModes() {
		modes = append(modes, apiclient.AgentMode(md))
	}
	if len(modes) == 0 {
		return errors.New("its manifest declares no run mode")
	}
	public := apiclient.CreateAgentRequestVisibilityPublic
	summary := m.Summary
	cr, err := client.CreateAgentWithResponse(ctx, apiclient.CreateAgentJSONRequestBody{
		Namespace: m.Namespace, Name: m.Name, Modes: modes, Summary: &summary, Visibility: &public,
	})
	if err != nil {
		return err
	}
	if cr.JSON201 == nil {
		p := cr.ApplicationproblemJSON422
		if p == nil {
			p = cr.ApplicationproblemJSONDefault
		}
		return apiProblem("create "+m.Namespace+"/"+m.Name, cr.StatusCode(), p, cr.Body)
	}
	_, _ = fmt.Fprintf(out, "created %s/%s, public\n", m.Namespace, m.Name)
	return markBuiltin(ctx, client, out, m)
}

// markBuiltin sets the registry's built-in mark on the agent.
func markBuiltin(ctx context.Context, client *apiclient.ClientWithResponses, out io.Writer, m *manifest.Manifest) error {
	yes := true
	ur, err := client.UpdateAgentWithResponse(ctx, m.Namespace, m.Name, apiclient.UpdateAgentJSONRequestBody{Builtin: &yes})
	if err != nil {
		return err
	}
	if ur.JSON200 == nil {
		return apiProblem("mark "+m.Namespace+"/"+m.Name+" built-in", ur.StatusCode(), ur.ApplicationproblemJSONDefault, ur.Body)
	}
	if !ur.JSON200.Builtin {
		return fmt.Errorf("the registry answered the mark on %s/%s without it: does it carry the built-in attribute?", m.Namespace, m.Name)
	}
	_, _ = fmt.Fprintf(out, "marked %s/%s built-in\n", m.Namespace, m.Name)
	return nil
}
