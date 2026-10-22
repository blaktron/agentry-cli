package localrun

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/blaktron/agentry-cli/pkg/runner/clamdb"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

// checkSignatures is clamdb.Ensure; the tests put a bundle of their own here.
var checkSignatures = clamdb.Ensure

// CheckFolder runs the decode step over the files of dir, a copy the caller
// made, as the share's check runs over a share file: with ClamAV when the
// policy's malwareCheck is on. A file ClamAV found refuses the folder,
// naming it and its signature. clamav says ClamAV checked every file and
// found nothing; false when the check is off, or when a file could not be
// checked (no signatures, past ClamAV's limits).
func CheckFolder(ctx context.Context, d dockerx.Client, pol *policy.Policy, home, dir string, logw io.Writer) (clamav bool, err error) {
	if logw == nil {
		logw = io.Discard
	}
	runnerTag := pol.RunnerImageTag(runnerimage.ContentVersion())
	ctxDir := filepath.Join(home, "image")
	if err := gateway.EnsureInstalled(ctx, d, pol, runnerTag, ctxDir, logw, true); err != nil {
		return false, err
	}
	img, err := decode.Ensure(ctx, d, runnerTag, pol, ctxDir, logw)
	if err != nil {
		return false, err
	}
	if pol.MalwareCheckOn() {
		img.Signatures = checkSignatures(ctx, home, pol.Images.ClamAVSignaturesRef(), logw)
	}
	out, err := os.MkdirTemp(filepath.Dir(dir), ".check-*")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(out) }()
	res, err := decode.RunWith(ctx, d, img, dir, out, pol.Docker.Limits.WithDefaults(), decode.Options{}, logw)
	if err != nil {
		return false, err
	}
	if len(res.Found()) > 0 {
		return false, errors.New(res.Refusal())
	}
	if !pol.MalwareCheckOn() {
		return false, nil
	}
	for _, f := range res.Report.Files {
		if f.Check != runnerdecode.CheckClean {
			return false, nil
		}
	}
	return len(res.Report.Files) > 0, nil
}
