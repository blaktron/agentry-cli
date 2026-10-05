package decode

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/clamdb"
	"github.com/blaktron/agentry-cli/pkg/runner/hybrid"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

const (
	hashEICAR = "275a021bbfb6489e54d471899f7db9d1663fc695ec2fe2a2c4538aabf651fd0f"
	hashSus   = "1111111111111111111111111111111111111111111111111111111111111111"
	hashClean = "3333333333333333333333333333333333333333333333333333333333333333"
	hashText  = "4444444444444444444444444444444444444444444444444444444444444444"
)

// overviewServer answers as Hybrid Analysis: EICAR malicious, hashSus
// suspicious, anything else unseen. It records the hashes asked.
func overviewServer(t *testing.T) (*hybrid.Client, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := strings.TrimPrefix(r.URL.Path, "/api/v2/overview/")
		mu.Lock()
		asked = append(asked, h)
		mu.Unlock()
		switch h {
		case hashEICAR:
			_, _ = io.WriteString(w, `{"verdict":"malicious","threat_score":100}`)
		case hashSus:
			_, _ = io.WriteString(w, `{"verdict":"suspicious","threat_score":45}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &hybrid.Client{Key: "ha-test-key", Base: srv.URL, HTTP: srv.Client()}, &asked
}

func lookupResult(files ...runnerdecode.File) *Result {
	return &Result{Report: runnerdecode.Report{Schema: runnerdecode.Schema, Files: files},
		Image: Image{Tag: "agentry-decode:test", Signatures: clamdb.DB{Dir: "/sigs", Ref: "bundle", Digest: "sha256:feed"}}}
}

func riskyFile(path, mime, sum string, check runnerdecode.Check) runnerdecode.File {
	return runnerdecode.File{Path: path, MIME: mime, SHA256: sum, Check: check, Outcome: runnerdecode.OutcomeDecoded, Family: runnerdecode.FamilyDocument}
}

// Only the risky files are looked up, by hash, archive members included;
// a file ClamAV found and a .git internal are not asked about; malicious
// refuses, suspicious is noted, unseen is clean, and the receipt names
// each outcome (plan §3 invariant 7).
func TestLookUpAsksOnlyForRiskyFilesAndRefusesOnMalicious(t *testing.T) {
	c, asked := overviewServer(t)
	res := lookupResult(
		riskyFile("tool.exe", "application/x-dosexec", hashEICAR, runnerdecode.CheckClean),
		riskyFile("bundle.zip/paper.pdf", "application/pdf", hashSus, runnerdecode.CheckClean),
		riskyFile("old.doc", "application/msword", hashClean, runnerdecode.CheckClean),
		riskyFile("notes.txt", "text/plain", hashText, runnerdecode.CheckClean),
		riskyFile("found.pdf", "application/pdf", hashClean, runnerdecode.CheckFound),
		runnerdecode.File{Path: "repo/.git/objects/ab/cd.exe", SHA256: hashClean, Why: runnerdecode.WhyRepository, Check: runnerdecode.CheckClean},
	)
	res.LookUp(context.Background(), c)
	if strings.Join(*asked, " ") != strings.Join([]string{hashEICAR, hashSus, hashClean}, " ") {
		t.Fatalf("asked %v", *asked)
	}
	if !res.Refused() || len(res.Malicious()) != 1 || res.Malicious()[0].Path != "tool.exe" {
		t.Fatalf("refused=%v malicious=%+v", res.Refused(), res.Malicious())
	}
	if got := res.Refusal(); !strings.Contains(got, "ClamAV found malware in found.pdf") || !strings.Contains(got, "Hybrid Analysis's verdict on tool.exe is malicious (threat score 100)") {
		t.Fatalf("refusal: %q", got)
	}
	line := res.Checks()
	for _, want := range []string{
		"Hybrid Analysis (CrowdStrike), asked by SHA-256 alone and never sent a file, on the files of a type it is asked about: 1 clean",
		"1 noted (bundle.zip/paper.pdf: suspicious, threat score 45)",
		"1 refused (tool.exe: malicious, threat score 100)",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the receipt's checks lack %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "ha-test-key") {
		t.Fatal("the key is in the receipt")
	}
}

// No key: nothing is asked, and the receipt says the risky files were not
// checked, no key; nothing risky: it says none was of a type looked up.
func TestLookUpWithoutAKeyOrARiskyFile(t *testing.T) {
	res := lookupResult(riskyFile("a.pdf", "application/pdf", hashClean, runnerdecode.CheckClean), riskyFile("b.exe", "", hashSus, runnerdecode.CheckClean))
	res.LookUp(context.Background(), &hybrid.Client{})
	if res.Refused() || !strings.Contains(res.Checks(), "on the files of a type it is asked about: 2 not checked: no key") {
		t.Fatalf("no key: refused=%v %s", res.Refused(), res.Checks())
	}
	plain := lookupResult(riskyFile("notes.txt", "text/plain", hashText, runnerdecode.CheckClean))
	plain.LookUp(context.Background(), nil)
	if !strings.HasSuffix(plain.Checks(), "; no file was asked about on Hybrid Analysis") {
		t.Fatalf("nothing risky: %s", plain.Checks())
	}
	never := lookupResult(riskyFile("a.pdf", "application/pdf", hashClean, runnerdecode.CheckClean))
	if strings.Contains(never.Checks(), "Hybrid Analysis") {
		t.Fatalf("a decode never looked up claims a lookup: %s", never.Checks())
	}
}

// A risky member the decode reported no digest for is not asked about,
// and the receipt says it was not checked rather than leaving it out.
func TestARiskyFileWithNoDigestIsNotChecked(t *testing.T) {
	c, asked := overviewServer(t)
	res := lookupResult(riskyFile("bundle.zip/x.exe", "application/x-dosexec", "", runnerdecode.CheckClean))
	res.LookUp(context.Background(), c)
	if len(*asked) != 0 || !strings.Contains(res.Checks(), "1 not checked: no digest") {
		t.Fatalf("asked %v; %s", *asked, res.Checks())
	}
}

// With no ClamAV signatures the receipt still names the lookups: the two
// checks are independent, and one missing does not hide the other.
func TestTheLookupClauseStandsWithoutSignatures(t *testing.T) {
	c, _ := overviewServer(t)
	res := lookupResult(riskyFile("tool.exe", "application/x-dosexec", hashEICAR, runnerdecode.CheckUnchecked))
	res.Image.Signatures = clamdb.DB{Problem: clamdb.ProblemNone}
	res.LookUp(context.Background(), c)
	if got := res.Checks(); got != "no file was checked for malware: no ClamAV signatures could be fetched; Hybrid Analysis (CrowdStrike), asked by SHA-256 alone and never sent a file, on the files of a type it is asked about: 0 clean, 1 refused (tool.exe: malicious, threat score 100)" {
		t.Fatalf("checks: %q", got)
	}
}
