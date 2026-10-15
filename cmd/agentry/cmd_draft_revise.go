package main

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	pkgarchive "github.com/blaktron/agentry/pkg/archive"

	"github.com/blaktron/agentry-cli/internal/origin"
	"github.com/blaktron/agentry-cli/pkg/drafter"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
)

// revising says the command drafts a revision.
func (o draftOpts) revising() bool { return o.revise != "" || o.reviseRef != "" }

// checkReviseFlags refuses a command line that mixes a revision with a new
// draft's inputs, or names a revision's texts without a release.
func checkReviseFlags(args []string, o draftOpts) error {
	if o.reviseRegistry != "" {
		if o.reviseRef == "" {
			return errors.New("--revise-registry names where --revise-ref is fetched from: name the release with --revise-ref")
		}
		if !origin.Same(o.reviseRegistry, registryURL()) {
			return fmt.Errorf("--revise-registry must be on --registry's origin (%s)", registryURL())
		}
	}
	if !o.revising() {
		if o.instructionsFile != "" || o.problemsFile != "" {
			return errors.New("--instructions-file and --problems-file belong to a revision: name the release with --revise or --revise-ref")
		}
		return nil
	}
	switch {
	case o.revise != "" && o.reviseRef != "":
		return errors.New("name the release with --revise or --revise-ref, not both")
	case o.revise != "" && o.hosted:
		return errors.New("a hosted revision fetches its release with --revise-ref")
	case len(args) > 0 || o.descriptionFile != "":
		return errors.New("a revision takes no description: what you want changed goes in --instructions-file, the problems you found in --problems-file")
	case len(o.hints) > 0:
		return errors.New("a revision takes no hints")
	case len(o.knowledge) > 0:
		return errors.New("a revision takes no knowledge packs yet")
	case o.instructionsFile == "" && o.problemsFile == "":
		return errors.New("a revision takes --instructions-file, --problems-file, or both")
	}
	return nil
}

// draftRevision lays the release down in dest and reads the person's two
// texts: a folder on this computer, digested as `agentry publish` packs it,
// or a published release fetched from the registry, held to the digest the
// registry resolved.
func draftRevision(cmd *cobra.Command, o draftOpts, dest string) (*drafter.Revision, error) {
	instructions, err := readRevisionText(o.instructionsFile, "the additional instructions")
	if err != nil {
		return nil, engine.Classify(engine.ExitUsage, err)
	}
	problems, err := readRevisionText(o.problemsFile, "the problems with this draft")
	if err != nil {
		return nil, engine.Classify(engine.ExitUsage, err)
	}
	var rev *drafter.Revision
	if o.revise != "" {
		if rev, err = readLocalRevision(o.revise, dest); err != nil {
			return nil, err
		}
	} else if rev, err = fetchRevision(cmd, cmp.Or(strings.TrimRight(o.reviseRegistry, "/"), registryURL()), o.reviseRef, dest); err != nil {
		return nil, err
	}
	if err := rev.WithTexts(instructions, problems); err != nil {
		return nil, engine.Classify(engine.ExitUsage, err)
	}
	return rev, nil
}

// readLocalRevision reads a release folder on this computer as `agentry
// publish` would upload it: packed by its publish.include, and read back
// from that archive, so the digest recorded is of the bytes the Drafter is
// given and nothing outside the folder (a link out of it) is read.
func readLocalRevision(dir, dest string) (*drafter.Revision, error) {
	m, err := localrun.ManifestFrom(dir)
	if err != nil {
		return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("the release to revise: %w", err))
	}
	var archive bytes.Buffer
	info, err := localrun.PackWorkspace(dir, m, &archive)
	if err != nil {
		return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("the release to revise: %w", err))
	}
	return releaseFromArchive(archive.Bytes(), dest, "", info.Digest, engine.ExitUsage)
}

// releaseFromArchive extracts a release's archive under the install
// limits and reads it for a revision (drafter.ReadRelease); a failure is
// classified as code.
func releaseFromArchive(archive []byte, dest, ref, digest string, code int) (*drafter.Revision, error) {
	staging, err := os.MkdirTemp("", "agentry-revise-*")
	if err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := pkgarchive.Extract(bytes.NewReader(archive), staging, int64(len(archive)), installExtractLimits()); err != nil {
		return nil, engine.Classify(code, fmt.Errorf("extract the release: %w", err))
	}
	rev, err := drafter.ReadRelease(staging, dest, ref, digest)
	if err != nil {
		return nil, engine.Classify(code, err)
	}
	return rev, nil
}

// readRevisionText reads one of the person's texts; an unnamed file is
// none, and the bound is drafter.MaxRevisionTextBytes (WithTexts says so).
func readRevisionText(path, what string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // the person names the file
	if err != nil {
		return "", fmt.Errorf("read %s: %w", what, err)
	}
	return string(b), nil
}

// fetchRevision fetches a published release, an exact ns/name@version, as
// `agentry run` and `install` do: resolved from reg with this command's
// credential (for a hosted revision, reg is the sandbox host's release
// prefix and the run's grant opens it), downloaded, and held to the digest
// the registry resolved before anything of it is read.
func fetchRevision(cmd *cobra.Command, reg, ref, dest string) (*drafter.Revision, error) {
	if at := strings.LastIndex(ref, "@"); at <= 0 || at == len(ref)-1 || !strings.Contains(ref[:at], "/") {
		return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("--revise-ref takes an exact release, ns/name@version, not %q", ref))
	}
	token, err := credentialFor(reg)
	if err != nil {
		return nil, err
	}
	client, err := newAPIClientChecked(cmd, reg, token)
	if err != nil {
		return nil, err
	}
	res, err := resolveRef(cmd.Context(), client, ref, false)
	if err != nil {
		return nil, err
	}
	if got := res.Namespace + "/" + res.Name + "@" + res.Version; got != ref {
		return nil, engine.Classify(engine.ExitVerify, fmt.Errorf("the registry resolved %s for %s", got, ref))
	}
	archive, err := downloadArchive(cmd.Context(), reg, token, res)
	if err != nil {
		return nil, err
	}
	if computed := sha256Digest(archive); computed != res.Digest {
		return nil, engine.Classify(engine.ExitVerify, fmt.Errorf("digest mismatch: registry says %s, archive is %s — refusing to revise it", res.Digest, computed))
	}
	return releaseFromArchive(archive, dest, ref, res.Digest, engine.ExitVerify)
}
