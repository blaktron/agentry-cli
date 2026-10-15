// `agentry publish`: submit a version and follow the pipeline to a
// verdict. --tag submits an existing repo tag (git-tag source); without it
// the CLI packs the deterministic archive locally and uploads it
// (cli-upload source). Progress arrives over the registry's event stream
// when it has one, else it is polled every 2s.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry/pkg/apiclient"
	"github.com/blaktron/agentry/pkg/manifest"
)

// Publish poll cadence and budget.
const (
	publishPollInterval = 2 * time.Second
	publishPollBudget   = 5 * time.Minute
	// submitAttempts bounds upload retries. Each attempt carries the same
	// Idempotency-Key, so a retry after a transport failure or a 5xx
	// replays the accepted submission instead of colliding on V3 "version
	// exists".
	submitAttempts = 3
)

// newPublishCmd builds the publish command.
func newPublishCmd() *cobra.Command {
	var tag string
	cmd := &cobra.Command{
		Use:   "publish [dir]",
		Short: "Submit a version for publication and follow the pipeline to a verdict",
		Long: "Submit a version for publication.\n\n" +
			"With --tag vX.Y.Z the registry builds the archive from that repo tag\n" +
			"(git-tag source). Without --tag the CLI packs the deterministic archive\n" +
			"locally from publish.include and uploads it (cli-upload source); the\n" +
			"server re-validates and re-packs — client packing is never trusted.\n\n" +
			"Progress streams from the registry (stage by stage) until published, rejected,\n" +
			"or needs_review; registries without the stream are polled every 2s.\n" +
			"Exits non-zero on rejection.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			return runPublish(cmd, dir, tag)
		},
	}
	cmd.Flags().StringVar(&tag, "tag", "", "existing repo tag vX.Y.Z to submit (git-tag source)")
	return cmd
}

// runPublish submits the version and polls to a verdict.
func runPublish(cmd *cobra.Command, dir, tag string) error {
	m, err := readWorkspaceManifest(dir)
	if err != nil {
		return err
	}

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

	var resp *apiclient.SubmitVersionResponse
	if tag != "" {
		_, _ = fmt.Fprintf(out, "submitting %s/%s tag %s (git-tag source)\n", m.Namespace, m.Name, tag)
		resp, err = client.SubmitVersionWithResponse(cmd.Context(), m.Namespace, m.Name,
			apiclient.SubmitVersionJSONRequestBody{Tag: tag})
	} else {
		resp, err = submitUpload(cmd.Context(), client, out, dir, m)
	}
	if err != nil {
		return err
	}
	if resp.JSON202 == nil {
		return apiProblem("submit version", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.Body)
	}
	v := resp.JSON202
	_, _ = fmt.Fprintf(out, "version %s accepted (status %s) — waiting for the pipeline verdict\n", v.Version, v.Status)

	return waitVerdict(cmd.Context(), client, httpClient, reg, token, out, m.Namespace, m.Name, v)
}

// submitUpload packs the deterministic archive locally (WalkWorkspace →
// ResolveInclude → BuildArchive) and POSTs it as multipart with the
// client-computed digest for the server cross-check.
func submitUpload(ctx context.Context, client *apiclient.ClientWithResponses, out io.Writer, dir string, m *manifest.Manifest) (*apiclient.SubmitVersionResponse, error) {
	var archive bytes.Buffer
	info, err := localrun.PackWorkspace(dir, m, &archive)
	if err != nil {
		return nil, err
	}
	if err := shippedPacksIncluded(m, info.Files); err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(out, "submitting %s/%s %s (cli-upload source): %d file(s), %d bytes, %s\n",
		m.Namespace, m.Name, m.Version, len(info.Files), info.Size, info.Digest)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("archive", "agent.tar.zst")
	if err != nil {
		return nil, fmt.Errorf("build multipart body: %w", err)
	}
	if _, err := part.Write(archive.Bytes()); err != nil {
		return nil, fmt.Errorf("write archive part: %w", err)
	}
	if err := mw.WriteField("digest", info.Digest); err != nil {
		return nil, fmt.Errorf("write digest field: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("finish multipart body: %w", err)
	}
	if body.Len() > maxUploadBytes {
		return nil, fmt.Errorf("this release's upload is %s compressed; the registry takes at most %s through an upload (the edge's request cap) — publish it from its tag instead: push the tag to the registry's git remote, which publishes it when the agent auto-publishes tags, or push it and run agentry publish --tag vX.Y.Z",
			byteSize(int64(body.Len())), byteSize(maxUploadBytes))
	}

	key, err := newIdempotencyKey()
	if err != nil {
		return nil, err
	}
	raw := body.Bytes()
	withKey := func(_ context.Context, req *http.Request) error {
		req.Header.Set("Idempotency-Key", key)
		return nil
	}
	var last error
	for attempt := 1; attempt <= submitAttempts; attempt++ {
		resp, err := client.SubmitVersionWithBodyWithResponse(ctx, m.Namespace, m.Name,
			mw.FormDataContentType(), bytes.NewReader(raw), withKey)
		switch {
		case err != nil:
			last = fmt.Errorf("upload version: %w", err)
		case resp.StatusCode() >= 500:
			last = fmt.Errorf("upload version: registry answered %d", resp.StatusCode())
		default:
			return resp, nil
		}
		if attempt == submitAttempts {
			break
		}
		_, _ = fmt.Fprintf(out, "upload attempt %d failed (%v); retrying with the same Idempotency-Key\n", attempt, last)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt) * 2 * time.Second):
		}
	}
	return nil, last
}

// newIdempotencyKey is one publish invocation's key: random, so two
// deliberate publishes of the same bytes are not confused, and constant
// across this invocation's retries, so the server replays rather than
// re-runs.
func newIdempotencyKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("idempotency key: %w", err)
	}
	return "publish-" + hex.EncodeToString(b), nil
}

// pollVerdict polls the version until it reaches a terminal state, printing
// every status change.
func pollVerdict(ctx context.Context, client *apiclient.ClientWithResponses, out io.Writer, ns, name string, v *apiclient.AgentVersion) error {
	deadline := time.Now().Add(publishPollBudget)
	last := v.Status
	cur := v
	for {
		if done, err := verdict(out, ns, name, cur); done {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("version %s still %s after %s — check `agentry info %s/%s` later",
				cur.Version, cur.Status, publishPollBudget, ns, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(publishPollInterval):
		}
		resp, err := client.GetVersionInfoWithResponse(ctx, ns, name, v.Version)
		if err != nil {
			return fmt.Errorf("poll version %s: %w", v.Version, err)
		}
		if resp.JSON200 == nil {
			return apiProblem("poll version "+v.Version, resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.Body)
		}
		cur = resp.JSON200
		if cur.Status != last {
			_, _ = fmt.Fprintf(out, "status: %s\n", cur.Status)
			last = cur.Status
		}
	}
}

// verdict inspects a version status; done reports a terminal state and err
// carries the failure (rejection) if any.
func verdict(out io.Writer, ns, name string, v *apiclient.AgentVersion) (bool, error) {
	reason := ""
	if v.StatusReason != nil && *v.StatusReason != "" {
		reason = *v.StatusReason
	}
	switch v.Status {
	case "published":
		_, _ = fmt.Fprintf(out, "published %s/%s@%s", ns, name, v.Version)
		if v.ArchiveDigest != nil {
			_, _ = fmt.Fprintf(out, " (%s)", *v.ArchiveDigest)
		}
		if v.ScanStatus != nil && *v.ScanStatus == apiclient.AgentVersionScanStatusSkipped {
			// The registry admitted it without a scan (the organization's
			// administrator took the agent off the pipeline): said here,
			// so a publisher never reads "published" as "scanned".
			_, _ = fmt.Fprint(out, " — unscanned: the registry admitted it without a scan, and installs and runs will ask for --allow-unscanned")
		}
		_, _ = fmt.Fprintln(out)
		return true, nil
	case "rejected":
		if reason == "" {
			reason = "see the scan report in the web UI"
		}
		return true, fmt.Errorf("version %s rejected: %s", v.Version, reason)
	case "needs_review":
		_, _ = fmt.Fprintf(out, "version %s needs human review", v.Version)
		if reason != "" {
			_, _ = fmt.Fprintf(out, " (%s)", reason)
		}
		_, _ = fmt.Fprintln(out, " — it will publish (or not) when a reviewer decides")
		return true, nil
	case "quarantined", "blocked", "yanked":
		return true, fmt.Errorf("version %s ended in state %s: %s", v.Version, v.Status, reason)
	default: // submitted, scanning, approved — keep polling
		return false, nil
	}
}

// maxUploadBytes is the most the registry takes through an upload: the
// edge's request cap on /api/, which the core's own cap matches. A
// release that ships knowledge packs can pass it; its tag push has no
// such cap.
const maxUploadBytes = 16 << 20

// shippedPacksIncluded refuses, before anything is uploaded, a release whose
// shipped knowledge packs (knowledge.ships) publish.include leaves out of the
// archive: the registry would refuse it under V11, and the runner mounts a
// shipped pack from the release's own files.
func shippedPacksIncluded(m *manifest.Manifest, include []manifest.FileInfo) error {
	have := map[string]bool{}
	for _, f := range include {
		have[f.Path] = true
	}
	for _, name := range m.ShippedPacks() {
		if mf := manifest.KnowledgeDir + "/" + name + "/manifest.json"; !have[mf] {
			return fmt.Errorf("knowledge.ships names %s, and publish.include does not put %s in the archive (V11): add %s/** to publish.include, or the pack to %s/%s/",
				name, mf, manifest.KnowledgeDir, manifest.KnowledgeDir, name)
		}
	}
	return nil
}
