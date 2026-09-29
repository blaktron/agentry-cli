// `agentry builtins` (agentry-cli#470): the packages this binary embeds and
// runs as builtin — the Knowledge Creator, the Drafter, and any added to
// localrun.Builtins() — each with the digest of its release archive. The
// registry holds every one of them as these same bytes, so a person reads a
// built-in's instruction in the directory and checks it against their own
// binary with the digest printed here.
//
// `agentry builtins publish` is what the CLI's tag release runs to put them
// there. Only the registry's owner can: the built-ins live in their
// organization's namespace, and the registry refuses a publish to it from
// any account without that right (the operator, 2026-09-29), so an ordinary
// user cannot publish a built-in on a registry they do not own.
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
// its version, creating the agent record, public, when it has none.
func publishBuiltin(ctx context.Context, client *apiclient.ClientWithResponses, reg, token string, out io.Writer, b localrun.Builtin) error {
	want, err := b.ArchiveDigest()
	if err != nil {
		return err
	}
	ns, name, version := b.Namespace(), b.Name(), b.Version()

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
		_, _ = fmt.Fprintf(out, "%s already in the registry (%s, %s)\n", b.Ref(), vr.JSON200.Status, have)
		return nil
	case vr.StatusCode() != http.StatusNotFound:
		return apiProblem("look up "+b.Ref(), vr.StatusCode(), vr.ApplicationproblemJSONDefault, vr.Body)
	}

	if err := ensureBuiltinAgent(ctx, client, out, b); err != nil {
		return err
	}

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
// registry has none. The namespace is the organization that owns the
// built-ins, which the publishing token must be able to publish to.
func ensureBuiltinAgent(ctx context.Context, client *apiclient.ClientWithResponses, out io.Writer, b localrun.Builtin) error {
	ar, err := client.GetAgentWithResponse(ctx, b.Namespace(), b.Name())
	if err != nil {
		return err
	}
	if ar.JSON200 != nil {
		return nil
	}
	if ar.StatusCode() != http.StatusNotFound {
		return apiProblem("look up "+b.Namespace()+"/"+b.Name(), ar.StatusCode(), ar.ApplicationproblemJSONDefault, ar.Body)
	}

	stage, err := os.MkdirTemp("", "agentry-builtin-agent-*")
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
		Namespace: b.Namespace(), Name: b.Name(), Modes: modes, Summary: &summary, Visibility: &public,
	})
	if err != nil {
		return err
	}
	if cr.JSON201 == nil {
		p := cr.ApplicationproblemJSON422
		if p == nil {
			p = cr.ApplicationproblemJSONDefault
		}
		return apiProblem("create "+b.Namespace()+"/"+b.Name(), cr.StatusCode(), p, cr.Body)
	}
	_, _ = fmt.Fprintf(out, "created %s/%s, public\n", b.Namespace(), b.Name())
	return nil
}
