// `agentry publish` for a package that ships knowledge packs: the
// publisher is asked about each one, the upload carries the confirmation
// with the origins `agentry knowledge ship` recorded, and nothing is
// uploaded without it.

package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	"github.com/blaktron/agentry-cli/pkg/knowledge/ship"
)

// answers makes a terminal that types the given lines one at a time.
func answers(t *testing.T, lines string) io.Reader {
	t.Helper()
	prev := stdinIsTTY
	t.Cleanup(func() { stdinIsTTY = prev })
	stdinIsTTY = func(io.Reader) bool { return true }
	return iotest.OneByteReader(strings.NewReader(lines))
}

// shippingWorkspace is a package that ships the named packs, laid as
// `agentry knowledge ship` lays them, with an own origin recorded for each.
func shippingWorkspace(t *testing.T, packs ...string) (dir string, origins map[string]string) {
	t.Helper()
	dir = writeTree(t, skillFiles("demo", ""))
	origins = map[string]string{}
	var rec ship.Record
	for i, name := range packs {
		p := knowledgetest.Make(t, name, nil)
		laid, err := ship.Lay(dir, p)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "agent.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		e, err := ship.AddShip(raw, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), e.YAML, 0o644); err != nil {
			t.Fatal(err)
		}
		id := "6f1c0f2e-0000-4000-8000-00000000000" + string(rune('1'+i))
		origins[name] = id
		rec.Set(ship.Entry{Name: name, SHA256: laid.SHA256, Registry: registryURL(), Origin: &ship.Origin{Kind: ship.KindOwn, PackID: id}})
	}
	if err := ship.WriteRecord(dir, rec); err != nil {
		t.Fatal(err)
	}
	return dir, origins
}

func publishSetup(t *testing.T) *fakeRegistry {
	t.Helper()
	isolateEnv(t)
	t.Setenv(envToken, "agy_testtoken")
	fr := newFakeRegistry(t)
	t.Setenv(envRegistry, fr.url())
	return fr
}

// shippingPartOf is the confirmation the first upload carried.
func shippingPartOf(t *testing.T, fr *fakeRegistry) (fakePart, shippingPart) {
	t.Helper()
	if len(fr.submitParts) != 1 {
		t.Fatalf("uploads: %d", len(fr.submitParts))
	}
	part, ok := fr.submitParts[0][shippingField]
	if !ok {
		t.Fatalf("the upload carried no %s part: %v", shippingField, fr.submitParts[0])
	}
	var got shippingPart
	if err := json.Unmarshal([]byte(part.body), &got); err != nil {
		t.Fatalf("%s: %v", part.body, err)
	}
	return part, got
}

func TestPublishAsksOncePerShippedPackAndSendsTheConfirmation(t *testing.T) {
	fr := publishSetup(t)
	dir, origins := shippingWorkspace(t, "runbooks", "handbook")

	stdout, stderr, err := runCLI(t, answers(t, "yes\nyes\n"), "publish", dir)
	if err != nil {
		t.Fatalf("%v\n%s%s", err, stdout, stderr)
	}
	for _, name := range []string{"runbooks", "handbook"} {
		warning := name + " ("
		if !strings.Contains(stderr, warning) || strings.Count(stderr, "Ship "+name+" with this release?") != 1 {
			t.Errorf("no question for %s:\n%s", name, stderr)
		}
	}
	for _, want := range []string{
		"becomes public. Anyone can download it with this release, and it stays in this version for good: retiring or deleting the QuickAgent later does not recall copies already downloaded.",
		"Check that it holds no private data: names, emails, customer records, internal hostnames, keys.",
	} {
		if strings.Count(stderr, want) != 2 {
			t.Errorf("the warning %q is not shown once per pack:\n%s", want, stderr)
		}
	}
	part, got := shippingPartOf(t, fr)
	if part.contentType != "application/json" || len(got.Packs) != 2 {
		t.Fatalf("part %+v %+v", part, got)
	}
	for _, p := range got.Packs {
		if p.Origin == nil || p.Origin.Kind != ship.KindOwn || p.Origin.PackID != origins[p.Name] {
			t.Errorf("%s: origin %+v, want own %s", p.Name, p.Origin, origins[p.Name])
		}
	}
	if !strings.Contains(stdout, "published acme/demo") {
		t.Errorf("stdout %s", stdout)
	}
}

func TestPublishYesAnswersForScripts(t *testing.T) {
	fr := publishSetup(t)
	dir, _ := shippingWorkspace(t, "runbooks")
	// No terminal at all: --yes still publishes, and says it confirmed.
	_, stderr, err := runCLI(t, nil, "publish", dir, "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "runbooks (") || !strings.Contains(stderr, "Confirmed with --yes: runbooks ships with this release.") {
		t.Errorf("stderr %s", stderr)
	}
	if _, got := shippingPartOf(t, fr); len(got.Packs) != 1 || got.Packs[0].Name != "runbooks" {
		t.Errorf("part %+v", got)
	}
}

func TestPublishUploadsNothingUnlessEveryPackIsConfirmed(t *testing.T) {
	fr := publishSetup(t)
	dir, _ := shippingWorkspace(t, "runbooks", "handbook")

	t.Run("a no to the second pack, which is asked about in name order", func(t *testing.T) {
		_, _, err := runCLI(t, answers(t, "yes\nno\n"), "publish", dir)
		if err == nil || !strings.Contains(err.Error(), "runbooks was not confirmed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no terminal and no --yes", func(t *testing.T) {
		_, _, err := runCLI(t, strings.NewReader(""), "publish", dir)
		if err == nil || !strings.Contains(err.Error(), "pass --yes") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an answer that is not yes", func(t *testing.T) {
		_, _, err := runCLI(t, answers(t, "y\n"), "publish", dir)
		if err == nil {
			t.Fatal("a y was taken for a yes")
		}
	})
	if len(fr.submitParts) != 0 || len(fr.submitTags) != 0 {
		t.Errorf("something was uploaded without every confirmation: %d %d", len(fr.submitParts), len(fr.submitTags))
	}
}

func TestPublishWordsThePrivateCase(t *testing.T) {
	fr := publishSetup(t)
	fr.addAgent("acme", "demo", "autonomous", "a test agent")
	fr.agentVisibility = "private"
	dir, _ := shippingWorkspace(t, "runbooks")
	_, stderr, err := runCLI(t, nil, "publish", dir, "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "Everyone who can see this QuickAgent can read runbooks (") || strings.Contains(stderr, "becomes public") {
		t.Errorf("stderr %s", stderr)
	}
}

func TestPublishSendsNoConfirmationForAReleaseThatShipsNothing(t *testing.T) {
	fr := publishSetup(t)
	_, stderr, err := runCLI(t, nil, "publish", publishWorkspace(t))
	if err != nil {
		t.Fatalf("%v %s", err, stderr)
	}
	if _, ok := fr.submitParts[0][shippingField]; ok || strings.Contains(stderr, "Ship ") {
		t.Errorf("a release that ships nothing was asked about: %v %s", fr.submitParts[0], stderr)
	}
}

func TestPublishClaimsAnOriginOnlyForThePackThatWasLaid(t *testing.T) {
	fr := publishSetup(t)
	dir, _ := shippingWorkspace(t, "runbooks", "handbook")
	// handbook is replaced by hand after it was laid.
	mf := filepath.Join(ship.Dir(dir, "handbook"), "manifest.json")
	raw, _ := os.ReadFile(mf)
	rec, _ := ship.ReadRecord(dir)
	e, _ := rec.Find("handbook")
	if err := os.WriteFile(mf, []byte(strings.Replace(string(raw), e.SHA256, "sha256:00", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	// The registry would refuse the edited manifest.json at V11, but the
	// fake does not: what matters here is the confirmation.
	_, stderr, err := runCLI(t, nil, "publish", dir, "--yes")
	if err != nil {
		t.Fatal(err)
	}
	_, got := shippingPartOf(t, fr)
	by := map[string]*ship.Origin{}
	for _, p := range got.Packs {
		by[p.Name] = p.Origin
	}
	if by["runbooks"] == nil || by["handbook"] != nil {
		t.Errorf("origins %+v", by)
	}
	if !strings.Contains(stderr, "handbook: no origin is claimed") {
		t.Errorf("the dropped origin is not said: %s", stderr)
	}
}

func TestPublishNamesThePackTheRegistryRefused(t *testing.T) {
	fr := publishSetup(t)
	fr.submitProblem = &fakeProblem{Status: 422, Type: "knowledge-shipping-invalid", Title: "Shipped knowledge is invalid",
		Detail: "its files are not the origin pack's current content.", Members: map[string]any{"packs": []string{"runbooks"}}}
	dir, _ := shippingWorkspace(t, "runbooks")
	_, _, err := runCLI(t, nil, "publish", dir, "--yes")
	if err == nil || !strings.Contains(err.Error(), "Shipped knowledge is invalid") || !strings.Contains(err.Error(), "knowledge packs: runbooks") {
		t.Fatalf("err = %v", err)
	}
}

func TestPublishKeepsTheSidecarOutOfTheRelease(t *testing.T) {
	fr := publishSetup(t)
	dir, _ := shippingWorkspace(t, "runbooks")
	raw, _ := os.ReadFile(filepath.Join(dir, "agent.yaml"))
	wide := strings.Replace(string(raw), "include: [agent.yaml, SKILL.md, knowledge/**]", "include: [\"**\"]", 1)
	if wide == string(raw) {
		t.Fatalf("the include was not widened:\n%s", raw)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(wide), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := runCLI(t, nil, "publish", dir, "--yes")
	if err == nil || !strings.Contains(err.Error(), ship.RecordFile) {
		t.Fatalf("err = %v", err)
	}
	if len(fr.submitParts) != 0 {
		t.Error("a release carrying the sidecar was uploaded")
	}
}

func TestPublishFromATagIsAskedToo(t *testing.T) {
	fr := publishSetup(t)
	dir, _ := shippingWorkspace(t, "runbooks")

	if _, _, err := runCLI(t, strings.NewReader(""), "publish", dir, "--tag", "v1.0.0"); err == nil || len(fr.submitTags) != 0 {
		t.Fatalf("a tag with a shipped pack went without a confirmation: %v", err)
	}
	_, stderr, err := runCLI(t, nil, "publish", dir, "--tag", "v1.0.0", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if len(fr.submitTags) != 1 || !strings.Contains(fr.submitTags[0], "v1.0.0") {
		t.Errorf("tag submissions %v", fr.submitTags)
	}
	if !strings.Contains(stderr, "records the version as published from a tag") {
		t.Errorf("stderr %s", stderr)
	}
}

func TestShippingWarningWords(t *testing.T) {
	pub := shippingWarning("aws-docs", "4.2 MiB", true)
	if pub != "aws-docs (4.2 MiB) becomes public. Anyone can download it with this release, and it stays in this version for good: retiring or deleting the QuickAgent later does not recall copies already downloaded. Check that it holds no private data: names, emails, customer records, internal hostnames, keys." {
		t.Errorf("public: %s", pub)
	}
	priv := shippingWarning("aws-docs", "4.2 MiB", false)
	if !strings.HasPrefix(priv, "Everyone who can see this QuickAgent can read aws-docs (4.2 MiB).") || !strings.HasSuffix(priv, "Check that it holds no private data: names, emails, customer records, internal hostnames, keys.") {
		t.Errorf("private: %s", priv)
	}
}
