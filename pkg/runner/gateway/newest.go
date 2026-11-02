package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/harnessbuild"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// NewestCheckEvery is how often a vendor is asked for its newest release.
// Between asks, the image built from the newest release (or the tested one)
// is used as it is.
const NewestCheckEvery = 24 * time.Hour

// now is the clock the record is kept by; a variable for tests.
var now = time.Now

// newestMaker is what EnsureNewestHarnessImage needs of the builder.
type newestMaker interface {
	AskNewest(ctx context.Context, adapterName string) (harnessbuild.Newest, bool, error)
	BuildNewest(ctx context.Context, n harnessbuild.Newest) (policy.HarnessImage, policy.Newest, error)
	Ensure(ctx context.Context, adapterName string, h policy.HarnessImage) error
}

// newestBuilder is the builder EnsureNewestHarnessImage uses, knowing the
// record's entry recorded; a variable so tests reach fake vendors.
var newestBuilder = func(b harnessbuild.Builder, recorded *policy.Newest) newestMaker {
	b.Recorded = recorded
	return b
}

// EnsureNewestHarnessImage makes the image adapterName runs in present the
// way setup does: from the vendor's newest release first, when it is newer
// than the release this build pins and passes every check, else from the
// tested pin. The image is recorded beside pol's file and set in pol, so a
// run compiled after it runs in it; it is returned. A policy that pins the
// image itself, an adapter whose newest release is never tried, or a policy
// read from no file takes its pin as EnsureHarnessImageBuilding does. The
// vendor is asked at most once every NewestCheckEvery; between asks the
// recorded image is used, and built again from the record if it is gone.
// building is told the version of any image built, before its download.
func EnsureNewestHarnessImage(ctx context.Context, d dockerx.Client, pol *policy.Policy, recordPath, adapterName string, logw io.Writer, building func(version string)) (policy.HarnessImage, error) {
	if logw == nil {
		logw = io.Discard
	}
	tested, ok := policy.DefaultHarnessImages[adapterName]
	path := recordPath
	if !ok || pol.Images.Pins(adapterName) || !policy.TriesNewest(adapterName) || path == "" {
		h, found := pol.Images.HarnessImageFor(adapterName)
		if !found {
			return policy.HarnessImage{}, fmt.Errorf("the policy names no %s harness image", adapterName)
		}
		return h, EnsureHarnessImageBuilding(ctx, d, adapterName, h, logw, building)
	}
	useTested := func() (policy.HarnessImage, error) {
		pol.Images.ClearNewest(adapterName)
		return tested, EnsureHarnessImageBuilding(ctx, d, adapterName, tested, logw, building)
	}
	records, err := policy.ReadNewest(path)
	if err != nil {
		_, _ = fmt.Fprintf(logw, "the record of the newest harness images could not be read (%v); asking the vendor again\n", err)
		records = map[string]policy.Newest{}
	}
	rec := records[adapterName]
	base := harnessbuild.Builder{Docker: d, Log: logw, Building: building, CacheDir: filepath.Dir(path)}
	b := newestBuilder(base, nil)

	// useRecorded makes the recorded image present, building it again from
	// the record when it is gone.
	useRecorded := func(rec policy.Newest) (policy.HarnessImage, bool) {
		h, ok := rec.Image()
		if !ok {
			return policy.HarnessImage{}, false
		}
		if err := checkHarnessImage(adapterName, h); err == nil {
			err = newestBuilder(base, &rec).Ensure(ctx, adapterName, h)
			if err == nil {
				pol.Images.SetNewest(adapterName, rec)
				return h, true
			}
			if ctx.Err() == nil {
				_, _ = fmt.Fprintf(logw, "the newest %s image %s could not be made again (%v); using the tested release %s\n", adapterName, h.Ref, err, harnessbuild.TestedVersion(adapterName))
			}
		}
		return policy.HarnessImage{}, false
	}

	if now().Sub(rec.Checked) < NewestCheckEvery {
		if h, ok := useRecorded(rec); ok {
			return h, nil
		}
		if err := ctx.Err(); err != nil {
			return policy.HarnessImage{}, err
		}
		return useTested()
	}

	n, newer, err := b.AskNewest(ctx, adapterName)
	if err := ctx.Err(); err != nil {
		return policy.HarnessImage{}, err
	}
	keep := func(entry policy.Newest) {
		entry.Checked = now()
		if werr := policy.WriteNewest(path, adapterName, entry); werr != nil {
			_, _ = fmt.Fprintf(logw, "the record of the newest harness images could not be written: %v\n", werr)
		}
	}
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(logw, "could not ask for the newest %s release (%v); using the tested release %s\n", adapterName, err, harnessbuild.TestedVersion(adapterName))
		// The vendor is asked again at the next check; a recorded image
		// already verified stays usable meanwhile.
		rec.Failed = ""
		keep(rec)
		if h, ok := useRecorded(rec); ok {
			return h, nil
		}
		return useTested()
	case !newer:
		keep(policy.Newest{})
		return useTested()
	case rec.Version == n.Version:
		keep(rec)
		if h, ok := useRecorded(rec); ok {
			return h, nil
		}
		if err := ctx.Err(); err != nil {
			return policy.HarnessImage{}, err
		}
	}
	_, _ = fmt.Fprintf(logw, "%s: the vendor's newest release is %s, newer than the tested %s; building it first, with the tested release as the fallback\n", adapterName, n.Version, n.Tested)
	h, entry, err := b.BuildNewest(ctx, n)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return policy.HarnessImage{}, errors.Join(cerr, err)
		}
		_, _ = fmt.Fprintf(logw, "the newest %s release %s could not be used (%v); using the tested release %s\n", adapterName, n.Version, err, n.Tested)
		keep(policy.Newest{Failed: n.Version + ": " + err.Error()})
		return useTested()
	}
	keep(entry)
	pol.Images.SetNewest(adapterName, entry)
	_, _ = fmt.Fprintf(logw, "%s runs on %s (the vendor's newest release, %s)\n", adapterName, h.Ref, n.Version)
	return h, nil
}
