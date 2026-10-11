package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/intake"
	"github.com/blaktron/agentry-cli/pkg/runner/sources"
	"github.com/blaktron/agentry-cli/runnerimage/source"
)

// plantedToken is a token a test gives a source and then looks for in
// everything the run leaves.
const plantedToken = "ghp_PLANTEDsourceTOKEN0123456789abcdef"

const testCommit = "0123456789abcdef0123456789abcdef01234567"

// sourceRun gives p one repository source, "repo", with the planted token,
// and scripts the fetch container to write files and report them as the
// runner's source role would. edit adjusts the files and the report.
func withSource(t *testing.T, p *Params, f *dockerx.Fake, files map[string]string, edit func(rep *source.Report, out string)) *dockerx.FakeForeground {
	t.Helper()
	p.Intake = &intake.Answers{Sources: map[string]*sources.Source{"repo": {
		ID: "repo", Kind: manifest.IntakeRepository, URL: "https://github.com/o/r", Host: "github.com",
		Token: plantedToken, TokenFrom: sources.TokenFromFlag,
	}}}
	fg := &dockerx.FakeForeground{}
	fg.Before = func(r dockerx.RunSpec) {
		var out, work string
		for _, m := range r.Mounts {
			switch m.Target {
			case "/out":
				out = m.Source
			case "/work":
				work = m.Source
			}
		}
		rep := source.Report{Schema: source.Schema, Kind: source.KindRepository, Ref: "refs/heads/main", Commit: testCommit, Protocol: "v2",
			Skipped: map[string]int{source.SkipLink: 1}}
		for name, body := range files {
			writeInput(t, out, name, body)
			rep.Files++
			rep.Bytes += int64(len(body))
		}
		if edit != nil {
			edit(&rep, out)
		}
		b, _ := json.Marshal(rep)
		if err := os.WriteFile(filepath.Join(work, source.ReportFile), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if f.Foreground == nil {
		f.Foreground = map[string]*dockerx.FakeForeground{}
	}
	f.Foreground["-source-repo"] = fg
	return fg
}

// TestARepositorySourceIsFetchedBeforeTheDecode pins invariants 1–4 of plan
// input-sources.md §3 at the engine: the fetch step runs in the files image
// on a network of its own, behind a proxy to the source's host alone at
// public addresses, with the token on stdin and nowhere else; its files are
// staged at /inputs/repo/ and decoded; and the agent, the gateway and the
// proxy of the run get no token, no source mount and no source network.
func TestARepositorySourceIsFetchedBeforeTheDecode(t *testing.T) {
	resetDecodeCalls()
	p, f := fixture(t)
	happyFake(f)
	p.KeepRunDir = true
	fg := withSource(t, &p, f, map[string]string{"README.md": "# r\n", "src/main.go": "package main\n"}, nil)
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(p.RunRoot, "runs", "cafe0000")

	fetch, ok := f.FindRun("-source-repo")
	if !ok {
		t.Fatal("no fetch step ran")
	}
	if fetch.Image != testDecodeImage.Tag {
		t.Errorf("the fetch step ran in %s, not the files image", fetch.Image)
	}
	if fetch.Network != sourceNetworkName("cafe0000", "repo") || len(fetch.ExtraNetworks) != 0 {
		t.Errorf("the fetch step's networks: %q %v", fetch.Network, fetch.ExtraNetworks)
	}
	if fg.GotStdin != plantedToken+"\n" {
		t.Errorf("the token did not reach the fetch step on stdin: %q", fg.GotStdin)
	}
	if !fetch.ReadOnly || fetch.User == "" && os.Getuid() >= 0 {
		t.Errorf("the fetch step's posture: read-only %v, user %q", fetch.ReadOnly, fetch.User)
	}
	proxy, ok := f.FindRun("-source-proxy-repo")
	if !ok {
		t.Fatal("no source proxy ran")
	}
	if got := strings.Join(proxy.Argv, " "); !strings.Contains(got, "--allow github.com:443") || !strings.Contains(got, "--public-only") {
		t.Errorf("the source proxy's argv: %s", got)
	}
	// The token is in no container's argv or environment, and only the
	// fetch step is on the source's network or mounts its folders.
	for _, c := range f.Calls {
		r := c.Run
		if r.Name == "" {
			continue
		}
		if strings.Contains(strings.Join(r.Argv, " "), plantedToken) {
			t.Errorf("%s carries the token in its argv", r.Name)
		}
		for k, v := range r.Env {
			if strings.Contains(v, plantedToken) || strings.Contains(os.Getenv(k), plantedToken) {
				t.Errorf("%s carries the token in its environment (%s)", r.Name, k)
			}
		}
		if r.Name == fetch.Name || r.Name == proxy.Name {
			continue
		}
		if r.Network == fetch.Network || slices.Contains(r.ExtraNetworks, fetch.Network) {
			t.Errorf("%s is on the source's network", r.Name)
		}
		for _, m := range r.Mounts {
			if strings.Contains(m.Source, filepath.Join(runDir, "sources")) {
				t.Errorf("%s mounts the source's own folder %s", r.Name, m.Source)
			}
		}
	}
	for _, c := range f.Calls {
		if c.Op == "NetworkConnect" && c.Arg == fetch.Network {
			t.Errorf("a container was connected to the source's network: %+v", c)
		}
	}
	// Staged and decoded as inputs.
	if b, err := os.ReadFile(filepath.Join(runDir, "inputs", "repo", "src", "main.go")); err != nil || string(b) != "package main\n" {
		t.Fatalf("the source was not staged at /inputs/repo/: %q %v", b, err)
	}
	idx := readIndex(t, runDir)
	var listed []string
	for _, e := range idx.Files {
		listed = append(listed, e.Path)
	}
	if !slices.Contains(listed, "repo/src/main.go") || !slices.Contains(listed, "repo/README.md") {
		t.Errorf("the index does not list the source's files: %v", listed)
	}
	// intake.json names the commit and the folder, never the token.
	var doc map[string]map[string]map[string]any
	b, err := os.ReadFile(filepath.Join(runDir, "inputs", intakeFile))
	if err != nil || json.Unmarshal(b, &doc) != nil {
		t.Fatalf("intake.json: %s %v", b, err)
	}
	if r := doc["repo"]["repository"]; r["commit"] != testCommit || r["path"] != "/inputs/repo/" || r["files"] != float64(2) || r["url"] != "https://github.com/o/r" {
		t.Errorf("intake.json's source entry: %v", doc["repo"])
	}
	line := env.CapabilitiesEnforced["source.repo"]
	for _, want := range []string{"repository https://github.com/o/r at its default branch (refs/heads/main)", "commit " + testCommit, "2 files", "through a proxy to github.com alone", "with an access token from the environment variable named by --intake-token", "left out: 1 link", "0 executable or binary risky files went to the malware check", "2 others were not checked"} {
		if !strings.Contains(line, want) {
			t.Errorf("the source's receipt line %q does not say %q", line, want)
		}
	}
	if _, err := os.Stat(filepath.Join(runDir, "sources")); !os.IsNotExist(err) {
		t.Errorf("the source's staging folder was left behind: %v", err)
	}
	assertNoToken(t, p.OutDir)
	assertNoToken(t, runDir)
}

// TestASourcesExecutableIsMalwareChecked: a repository holding an EICAR
// executable is refused before the agent starts, as an upload is
// (invariant 6, D8).
func TestASourcesExecutableIsMalwareChecked(t *testing.T) {
	resetDecodeCalls()
	p, f := fixture(t)
	happyFake(f)
	withSource(t, &p, f, map[string]string{"README.md": "EICAR in text is not checked\n", "bin/tool.exe": "MZ X5O EICAR"}, nil)
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitPolicy || !strings.Contains(err.Error(), "repo/bin/tool.exe") {
		t.Fatalf("err %v, want the policy class naming repo/bin/tool.exe", err)
	}
	if strings.Contains(err.Error(), "README.md") {
		t.Errorf("a source's text file went to the malware check: %v", err)
	}
	if _, ok := f.FindRun("-agent"); ok {
		t.Fatal("the agent ran on a refused source")
	}
}

// TestASourceOverItsCapRefusesTheRun: the fetch step's refusal is the
// run's, with the count, before anything is staged (invariant 7, D7).
func TestASourceOverItsCapRefusesTheRun(t *testing.T) {
	resetDecodeCalls()
	p, f := fixture(t)
	happyFake(f)
	p.KeepRunDir = true
	withSource(t, &p, f, nil, func(rep *source.Report, _ string) {
		rep.Refusal = "the repository holds 25000 files at " + testCommit + ", past the 20000-file cap"
	})
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitPolicy || !strings.Contains(err.Error(), "25000 files") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.RunRoot, "runs", "cafe0000", "inputs", "repo")); !os.IsNotExist(err) {
		t.Error("a refused source was staged")
	}
	if e, _ := decodeCounts(); e != 1 {
		t.Errorf("ensured the files image %d times", e)
	}
}

// TestAFetchStepThatBreaksTheRulesIsNotStaged: a link, a .git folder or
// counts that are not the report's mean the step is not the one this CLI
// ships, and nothing of it is staged (invariant 5).
func TestAFetchStepThatBreaksTheRulesIsNotStaged(t *testing.T) {
	for name, edit := range map[string]func(*source.Report, string){
		"a link": func(_ *source.Report, out string) {
			_ = os.Symlink("/etc/passwd", filepath.Join(out, "escape"))
		},
		"a .git folder": func(rep *source.Report, out string) {
			writeInput(t, out, "sub/.git/config", "x")
			rep.Files++
			rep.Bytes++
		},
		"counts it did not report": func(rep *source.Report, _ string) { rep.Files-- },
		"an unknown field":         nil,
	} {
		t.Run(name, func(t *testing.T) {
			resetDecodeCalls()
			p, f := fixture(t)
			happyFake(f)
			p.KeepRunDir = true
			fg := withSource(t, &p, f, map[string]string{"a.txt": "a"}, edit)
			if edit == nil {
				before := fg.Before
				fg.Before = func(r dockerx.RunSpec) {
					before(r)
					for _, m := range r.Mounts {
						if m.Target == "/work" {
							_ = os.WriteFile(filepath.Join(m.Source, source.ReportFile), []byte(`{"schema":"source/v1","kind":"repository","files":1,"bytes":1,"note":"x"}`), 0o644)
						}
					}
				}
			}
			_, err := Run(context.Background(), f, p)
			var ce *ClassifiedError
			if !errors.As(err, &ce) || ce.Code != ExitGateway {
				t.Fatalf("err %v, want the gateway class", err)
			}
			if _, err := os.Stat(filepath.Join(p.RunRoot, "runs", "cafe0000", "inputs", "repo")); !os.IsNotExist(err) {
				t.Error("the step's tree was staged")
			}
		})
	}
}

// TestAFetchFailureSaysWhy: a host's refusal reaches the person in the
// step's words, without the token.
func TestAFetchFailureSaysWhy(t *testing.T) {
	resetDecodeCalls()
	p, f := fixture(t)
	happyFake(f)
	fg := withSource(t, &p, f, nil, func(rep *source.Report, _ string) {
		rep.Commit = ""
		rep.Error = "the host answered 403 Forbidden: it refused the access token " + plantedToken
	})
	fg.ExitCode = 1
	env, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitCLI || !strings.Contains(err.Error(), "403 Forbidden") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error(), plantedToken) || strings.Contains(env.Error, plantedToken) {
		t.Error("the token reached the error")
	}
	assertNoToken(t, p.OutDir)
}

// TestAHostedRunTakesNoSourceYet: until agentry-enterprise#516, a hosted
// run (AI Review of its inputs) refuses a source rather than fetching it
// with local caps.
func TestAHostedRunTakesNoSourceYet(t *testing.T) {
	resetDecodeCalls()
	p, f := fixture(t)
	happyFake(f)
	withSource(t, &p, f, map[string]string{"a.txt": "a"}, nil)
	p.InputReview = &fakeReview{}
	_, err := Run(context.Background(), f, p)
	if err == nil || !strings.Contains(err.Error(), "agentry-enterprise#516") {
		t.Fatalf("err %v", err)
	}
	if _, ok := f.FindRun("-source-repo"); ok {
		t.Error("a hosted run fetched a source")
	}
}

func assertNoToken(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr == nil && strings.Contains(string(b), plantedToken) {
			t.Errorf("%s holds the token", p)
		}
		return nil
	})
}
