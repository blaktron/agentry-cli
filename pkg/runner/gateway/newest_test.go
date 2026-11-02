package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/harnessbuild"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// fakeVendor stands in for a vendor and the builder: the newest release it
// answers, whether its build passes, and what it was asked to do.
type fakeVendor struct {
	version  string
	askErr   error
	buildErr error
	asked    int
	built    []string
	ensured  []policy.HarnessImage
	recorded []*policy.Newest
}

func (v *fakeVendor) maker(_ harnessbuild.Builder, recorded *policy.Newest) newestMaker {
	v.recorded = append(v.recorded, recorded)
	return v
}

func (v *fakeVendor) AskNewest(_ context.Context, adapterName string) (harnessbuild.Newest, bool, error) {
	v.asked++
	if v.askErr != nil {
		return harnessbuild.Newest{}, false, v.askErr
	}
	tested := harnessbuild.TestedVersion(adapterName)
	return harnessbuild.Newest{Adapter: adapterName, Version: v.version, Tested: tested}, v.version != tested, nil
}

func (v *fakeVendor) BuildNewest(_ context.Context, n harnessbuild.Newest) (policy.HarnessImage, policy.Newest, error) {
	v.built = append(v.built, n.Version)
	if v.buildErr != nil {
		return policy.HarnessImage{}, policy.Newest{}, v.buildErr
	}
	h := newestImage(n.Adapter, n.Version)
	return h, policy.Newest{Ref: h.Ref, Artifact: h.Artifact, Version: n.Version, Release: []byte(`{}`)}, nil
}

func (v *fakeVendor) Ensure(_ context.Context, _ string, h policy.HarnessImage) error {
	v.ensured = append(v.ensured, h)
	return nil
}

func newestImage(adapterName, version string) policy.HarnessImage {
	name := "agentry-claude-code"
	if adapterName == harnessbuild.CodexImageAdapter {
		name = "agentry-codex"
	}
	return policy.HarnessImage{Ref: name + ":" + version, Artifact: "sha256:" + strings.Repeat("ab", 32)}
}

// loadedPolicy is the default policy loaded from a file of its own, and
// the path of its record.
func loadedPolicy(t *testing.T) (*policy.Policy, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	pol, _, err := policy.EnsureDefault(path)
	if err != nil {
		t.Fatal(err)
	}
	return pol, policy.NewestPathFor(path)
}

func withVendor(t *testing.T, v *fakeVendor, at *time.Time) {
	t.Helper()
	savedB, savedNow := newestBuilder, now
	newestBuilder = v.maker
	now = func() time.Time { return *at }
	t.Cleanup(func() { newestBuilder, now = savedB, savedNow })
}

// A newer release that passes is built, recorded and run; until the next
// check the vendor is not asked again and the record's image is used, and
// a policy loaded after it runs in it too.
func TestTheNewestReleaseIsBuiltRecordedAndUsedUntilTheNextCheck(t *testing.T) {
	v := &fakeVendor{version: "9.9.9"}
	clock := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	withVendor(t, v, &clock)
	pol, rec := loadedPolicy(t)
	d := &dockerx.Fake{}
	h, err := EnsureNewestHarnessImage(context.Background(), d, pol, rec, harnessbuild.ClaudeImageAdapter, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := newestImage(harnessbuild.ClaudeImageAdapter, "9.9.9")
	if h != want || v.asked != 1 || len(v.built) != 1 {
		t.Fatalf("got %+v, asked %d, built %v", h, v.asked, v.built)
	}
	if got, _ := pol.Images.HarnessImageFor(harnessbuild.ClaudeImageAdapter); got != want {
		t.Fatalf("the policy runs %+v", got)
	}
	again, _, err := policy.Load(filepath.Join(filepath.Dir(rec), "policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := again.Images.HarnessImageFor(harnessbuild.ClaudeImageAdapter); got != want {
		t.Fatalf("a policy loaded after setup runs %+v", got)
	}

	clock = clock.Add(NewestCheckEvery - time.Minute)
	if h, err := EnsureNewestHarnessImage(context.Background(), d, again, rec, harnessbuild.ClaudeImageAdapter, nil, nil); err != nil || h != want {
		t.Fatalf("within the day: %+v, %v", h, err)
	}
	if v.asked != 1 || len(v.ensured) != 1 || v.ensured[0] != want || v.recorded[len(v.recorded)-1] == nil {
		t.Fatalf("within the day: asked %d, ensured %v", v.asked, v.ensured)
	}

	clock = clock.Add(2 * time.Minute)
	if _, err := EnsureNewestHarnessImage(context.Background(), d, again, rec, harnessbuild.ClaudeImageAdapter, nil, nil); err != nil {
		t.Fatal(err)
	}
	if v.asked != 2 || len(v.built) != 1 {
		t.Fatalf("after the day: asked %d, built %v (the same release is not built again)", v.asked, v.built)
	}
}

// A newest release that fails a check is not used: the tested pin is made,
// the policy runs it, and the record says what failed.
func TestANewestReleaseThatFailsFallsBackToTheTestedOne(t *testing.T) {
	for _, c := range []struct {
		name    string
		vendor  fakeVendor
		adapter string
	}{
		{"claude, refused", fakeVendor{version: "9.9.9", buildErr: errors.New("refused: not signed")}, harnessbuild.ClaudeImageAdapter},
		{"claude, vendor down", fakeVendor{askErr: errors.New("connection refused")}, harnessbuild.ClaudeImageAdapter},
		{"codex, refused", fakeVendor{version: "9.9.9", buildErr: errors.New("refused: does not verify")}, harnessbuild.CodexImageAdapter},
	} {
		v := c.vendor
		clock := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
		withVendor(t, &v, &clock)
		pol, rec := loadedPolicy(t)
		tested := policy.DefaultHarnessImages[c.adapter]
		d := withTested(c.adapter)
		h, err := EnsureNewestHarnessImage(context.Background(), d, pol, rec, c.adapter, nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if h != tested {
			t.Errorf("%s: made %+v, not the tested %+v", c.name, h, tested)
		}
		if got, _ := pol.Images.HarnessImageFor(c.adapter); got != tested {
			t.Errorf("%s: the policy runs %+v", c.name, got)
		}
		if c.adapter == harnessbuild.CodexImageAdapter && !strings.Contains(strings.Join(d.Ops(), " "), "Pull:"+tested.Ref) {
			t.Errorf("%s: the tested Codex image was not pulled: %v", c.name, d.Ops())
		}
		records, err := policy.ReadNewest(rec)
		if err != nil {
			t.Fatal(err)
		}
		r := records[c.adapter]
		if _, ok := r.Image(); ok || !r.Checked.Equal(clock) {
			t.Errorf("%s: record %+v", c.name, r)
		}
		if v.buildErr != nil && !strings.Contains(r.Failed, "9.9.9: refused") {
			t.Errorf("%s: the record does not say what failed: %+v", c.name, r)
		}
	}
}

// A policy that pins the image itself, or a newest release no newer than
// the tested one, never builds anything new.
func TestAPinnedPolicyOrATestedNewestBuildsNothingNew(t *testing.T) {
	v := &fakeVendor{version: harnessbuild.TestedVersion(harnessbuild.ClaudeImageAdapter)}
	clock := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	withVendor(t, v, &clock)
	pol, rec := loadedPolicy(t)
	if h, err := EnsureNewestHarnessImage(context.Background(), withTested(harnessbuild.ClaudeImageAdapter), pol, rec, harnessbuild.ClaudeImageAdapter, nil, nil); err != nil || h != policy.DefaultHarnessImages[harnessbuild.ClaudeImageAdapter] {
		t.Fatalf("a tested newest: %+v, %v", h, err)
	}
	if len(v.built) != 0 {
		t.Fatalf("built %v", v.built)
	}

	path := filepath.Join(t.TempDir(), "policy.yaml")
	pinned := policy.DefaultHarnessImages[harnessbuild.ClaudeImageAdapter]
	text := strings.Replace(policyText(t), "  harnessImages:\n", "  harnessImages:\n    claude-image:\n      ref: "+pinned.Ref+"\n      artifact: "+pinned.Artifact+"\n", 1)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	own, _, err := policy.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	asked := v.asked
	if h, err := EnsureNewestHarnessImage(context.Background(), withTested(harnessbuild.ClaudeImageAdapter), own, policy.NewestPathFor(path), harnessbuild.ClaudeImageAdapter, nil, nil); err != nil || h != pinned {
		t.Fatalf("the pinned image: %+v, %v", h, err)
	}
	if v.asked != asked {
		t.Fatal("the vendor was asked for a policy that pins its own image")
	}
}

// withTested is a daemon that already holds the tested Claude Code image,
// built from its artifact, so making it fetches nothing; the tested Codex
// image is published and is pulled.
func withTested(adapterName string) *dockerx.Fake {
	h := policy.DefaultHarnessImages[adapterName]
	if h.Artifact == "" {
		return &dockerx.Fake{}
	}
	return &dockerx.Fake{
		ExistingImages: map[string]bool{h.Ref: true},
		Labels:         map[string]map[string]string{h.Ref: {harnessbuild.LabelArtifact: h.Artifact, harnessbuild.LabelRecipe: harnessbuild.ClaudeRecipe}},
	}
}

func policyText(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if _, _, err := policy.EnsureDefault(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
