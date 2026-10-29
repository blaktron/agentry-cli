package send

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/runnerimage/netproxy"
)

const (
	testSecret = "whsec_PLANTEDsignSECRET"
	testAuth   = "Bearer PLANTEDauthorizationVALUE0123456789"
)

// stage writes content into a staging folder and answers the job's file for
// it, with the digest of what was written.
func stage(t *testing.T, dir, name, rel, content string) File {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	return File{Path: rel, Title: strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel)), SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(content)), Type: strings.TrimPrefix(filepath.Ext(rel), "."), Marked: true, Staged: name}
}

type fixture struct {
	staging, work string
	job           Job
}

func newFixture(t *testing.T, url string) *fixture {
	t.Helper()
	return &fixture{staging: t.TempDir(), work: t.TempDir(), job: Job{Kind: KindWebhook, URL: url, RunID: "cafe0000", Ref: "acme/weekly@1.0.0", Digest: "sha256:abc", Result: json.RawMessage(`{"verdict":"pass","n":3}`)}}
}

func (f *fixture) options() Options {
	return Options{Job: f.job, Staging: f.staging, Work: f.work, Cred: Credential{Secret: testSecret, Authorization: testAuth}, unchecked: true}
}

// received is what a reference receiver saw.
type received struct {
	header http.Header
	body   []byte
}

// reference is a receiver written as a third party would write one from the
// documentation: it recomputes the HMAC over the timestamp header, a full stop
// and the raw body, compares in constant time, and checks the timestamp.
func reference(t *testing.T, got chan<- received, status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ts := r.Header.Get("X-Agentry-Timestamp")
		sec, err := strconv.ParseInt(ts, 10, 64)
		if err != nil || time.Since(time.Unix(sec, 0)) > 5*time.Minute {
			t.Errorf("the timestamp %q is not fresh", ts)
		}
		mac := hmac.New(sha256.New, []byte(testSecret))
		mac.Write([]byte(ts + "." + string(b)))
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(want), []byte(r.Header.Get("X-Agentry-Signature"))) {
			t.Errorf("the signature %q does not verify", r.Header.Get("X-Agentry-Signature"))
		}
		got <- received{header: r.Header.Clone(), body: b}
		w.WriteHeader(status)
		_, _ = w.Write([]byte("receiver-secret-text-never-kept"))
	})
}

// TestAWebhookIsSignedAndItsBodyIsTheRunsRecord: a reference receiver verifies
// the signature from the documented recipe, finds the run's id, release ref and
// digest, the result as parsed JSON, and each deliverable's digest and bytes.
func TestAWebhookIsSignedAndItsBodyIsTheRunsRecord(t *testing.T) {
	got := make(chan received, 1)
	srv := httptest.NewServer(reference(t, got, http.StatusOK))
	defer srv.Close()
	f := newFixture(t, srv.URL)
	f.job.Files = []File{stage(t, f.staging, "f0", "reports/a.md", "# hello\n"), stage(t, f.staging, "f1", "reports/b.csv", "x,y\n1,2\n")}
	rep, err := Run(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != StatusDelivered || rep.Error != "" {
		t.Fatalf("report: %+v", rep)
	}
	r := <-got
	var b struct {
		Schema, RunID, Ref, Digest string
		Result                     map[string]any
		Deliverables               []struct {
			Path, Title, SHA256, Type string
			Bytes                     int64
			Marked                    bool
			Content                   *string
			Omitted                   string
		}
	}
	if err := json.Unmarshal(r.body, &b); err != nil {
		t.Fatal(err)
	}
	if b.Schema != BodySchema || b.RunID != "cafe0000" || b.Ref != "acme/weekly@1.0.0" || b.Digest != "sha256:abc" || b.Result["verdict"] != "pass" {
		t.Errorf("body: %s", r.body)
	}
	if len(b.Deliverables) != 2 || b.Deliverables[0].Content == nil {
		t.Fatalf("deliverables: %s", r.body)
	}
	if raw, _ := base64.StdEncoding.DecodeString(*b.Deliverables[0].Content); string(raw) != "# hello\n" || b.Deliverables[0].SHA256 != f.job.Files[0].SHA256 || b.Deliverables[0].Path != "reports/a.md" {
		t.Errorf("first deliverable: %+v", b.Deliverables[0])
	}
	if r.header.Get("Authorization") != testAuth || r.header.Get("Content-Type") != "application/json" || r.header.Get("X-Agentry-Run") != "cafe0000" {
		t.Errorf("headers: %v", r.header)
	}
	// No credential is in the body, and none in the report.
	if strings.Contains(string(r.body), testSecret) || strings.Contains(string(r.body), "PLANTEDauthorization") {
		t.Error("the body carries a credential")
	}
	raw, err := os.ReadFile(filepath.Join(f.work, ReportFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PLANTED") || strings.Contains(string(raw), "receiver-secret-text") {
		t.Errorf("the report carries a credential or the receiver's words: %s", raw)
	}
	for _, fr := range rep.Files {
		if fr.Status != StatusDelivered {
			t.Errorf("file %+v", fr)
		}
	}
}

// TestAWebhookWithNoFilesStillSendsTheResult: a run that acted and left no
// file is delivered as its result alone.
func TestAWebhookWithNoFilesStillSendsTheResult(t *testing.T) {
	got := make(chan received, 1)
	srv := httptest.NewServer(reference(t, got, http.StatusNoContent))
	defer srv.Close()
	f := newFixture(t, srv.URL)
	rep, err := Run(context.Background(), f.options())
	if err != nil || rep.Status != StatusDelivered || rep.Result != StatusDelivered {
		t.Fatalf("%+v %v", rep, err)
	}
	if r := <-got; !strings.Contains(string(r.body), `"deliverables":[]`) || !strings.Contains(string(r.body), `"verdict":"pass"`) {
		t.Errorf("body: %s", r.body)
	}
}

// TestTheCapOmitsWhatDoesNotFit: a body is held to its cap, and a deliverable
// that would pass it is listed with its digest and omitted "too-large".
func TestTheCapOmitsWhatDoesNotFit(t *testing.T) {
	got := make(chan received, 1)
	srv := httptest.NewServer(reference(t, got, http.StatusOK))
	defer srv.Close()
	f := newFixture(t, srv.URL)
	f.job.MaxBody = 8 << 10
	f.job.Files = []File{stage(t, f.staging, "f0", "reports/small.txt", "small"), stage(t, f.staging, "f1", "reports/big.bin", strings.Repeat("x", 20<<10)), stage(t, f.staging, "f2", "reports/after.txt", "after")}
	rep, err := Run(context.Background(), f.options())
	if err != nil || rep.Status != StatusDelivered {
		t.Fatalf("%+v %v", rep, err)
	}
	r := <-got
	if int64(len(r.body)) > f.job.MaxBody {
		t.Errorf("the body is %d bytes, past its cap of %d", len(r.body), f.job.MaxBody)
	}
	var b struct {
		Deliverables []struct {
			Path, SHA256, Omitted string
			Content               *string
		}
	}
	if err := json.Unmarshal(r.body, &b); err != nil {
		t.Fatal(err)
	}
	if len(b.Deliverables) != 3 || b.Deliverables[1].Omitted != "too-large" || b.Deliverables[1].Content != nil || b.Deliverables[1].SHA256 != f.job.Files[1].SHA256 {
		t.Errorf("deliverables: %s", r.body)
	}
	if b.Deliverables[0].Content == nil || b.Deliverables[2].Content == nil {
		t.Errorf("the files that fit were not sent: %s", r.body)
	}
	if rep.Files[1].Status != StatusRefused || rep.Files[1].Error != ReasonTooLarge || rep.Files[0].Status != StatusDelivered {
		t.Errorf("files: %+v", rep.Files)
	}
}

// TestAHugeResultLeavesRoomForTheFiles: a result past half the cap is left out
// and said to be.
func TestAHugeResultLeavesRoomForTheFiles(t *testing.T) {
	got := make(chan received, 1)
	srv := httptest.NewServer(reference(t, got, http.StatusOK))
	defer srv.Close()
	f := newFixture(t, srv.URL)
	f.job.MaxBody = 4 << 10
	f.job.Result = json.RawMessage(`"` + strings.Repeat("r", 3<<10) + `"`)
	rep, _ := Run(context.Background(), f.options())
	r := <-got
	if int64(len(r.body)) > f.job.MaxBody || !strings.Contains(string(r.body), `"resultOmitted":"too-large"`) || rep.Result != ReasonTooLarge {
		t.Errorf("%d bytes: %s; %+v", len(r.body), r.body, rep)
	}
}

// TestAStagedFileThatNoLongerMatchesIsNotSent: the step shows each file to be
// the bytes of its digest again, and sends a changed one, a missing one, a link
// and a path outside reports/ as nothing but the reason.
func TestAStagedFileThatNoLongerMatchesIsNotSent(t *testing.T) {
	got := make(chan received, 1)
	srv := httptest.NewServer(reference(t, got, http.StatusOK))
	defer srv.Close()
	f := newFixture(t, srv.URL)
	changed := stage(t, f.staging, "f0", "reports/changed.txt", "original")
	if err := os.WriteFile(filepath.Join(f.staging, "f0"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(secret, []byte("OUTSIDE-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := stage(t, f.staging, "f1", "reports/link.txt", "linked")
	_ = os.Remove(filepath.Join(f.staging, "f1"))
	if err := os.Symlink(secret, filepath.Join(f.staging, "f1")); err != nil {
		t.Skip("no symlinks here")
	}
	missing := File{Path: "reports/gone.txt", SHA256: strings.Repeat("0", 64), Bytes: 3, Staged: "nope"}
	outside := stage(t, f.staging, "f3", "../etc/passwd", "x")
	escape := stage(t, f.staging, "f4", "reports/../../x", "x")
	f.job.Files = []File{changed, link, missing, outside, escape}
	rep, err := Run(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	r := <-got
	if strings.Contains(string(r.body), "tampered") || strings.Contains(string(r.body), "OUTSIDE-SECRET") || strings.Contains(string(r.body), base64.StdEncoding.EncodeToString([]byte("OUTSIDE-SECRET"))) {
		t.Errorf("the body carries a file it should not: %s", r.body)
	}
	want := []string{ReasonChanged, ReasonLink, ReasonMissing, ReasonOutside, ReasonOutside}
	for i, fr := range rep.Files {
		if fr.Status != StatusRefused || fr.Error != want[i] {
			t.Errorf("file %d: %+v, want refused %s", i, fr, want[i])
		}
	}
}

// TestAnAnswerThatIsNotSuccessFailsTheDelivery: a 500 is a failed delivery with
// the word "status", a 3xx one with "redirect" that is never followed, and what
// the receiver said is kept nowhere.
func TestAnAnswerThatIsNotSuccessFailsTheDelivery(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	for _, c := range []struct {
		name   string
		h      http.HandlerFunc
		reason string
	}{
		{"500", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom SECRET-RECEIVER-TEXT", 500) }, ReasonReceiverSaid},
		{"404", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, ReasonReceiverSaid},
		{"302", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }, ReasonRedirect},
		{"307", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
		}, ReasonRedirect},
	} {
		srv := httptest.NewServer(c.h)
		f := newFixture(t, srv.URL)
		f.job.Files = []File{stage(t, f.staging, "f0", "reports/a.md", "a")}
		rep, err := Run(context.Background(), f.options())
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if rep.Status != StatusFailed || rep.Error != c.reason || rep.Files[0].Status != StatusFailed || rep.Files[0].Error != c.reason {
			t.Errorf("%s: %+v", c.name, rep)
		}
		raw, _ := os.ReadFile(filepath.Join(f.work, ReportFile))
		if strings.Contains(string(raw), "SECRET-RECEIVER-TEXT") {
			t.Errorf("%s: the report holds the receiver's words", c.name)
		}
	}
	if elsewhere.Load() != 0 {
		t.Error("a redirect was followed to another host")
	}
}

// TestASlowReceiverEndsTheDelivery: a receiver that never answers costs the
// step its deadline and no more.
func TestASlowReceiverEndsTheDelivery(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	f := newFixture(t, srv.URL)
	f.job.Files = []File{stage(t, f.staging, "f0", "reports/a.md", "a")}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	rep, err := Run(ctx, f.options())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != StatusFailed || rep.Error != ReasonTimeout || time.Since(start) > 5*time.Second {
		t.Errorf("%+v after %s", rep, time.Since(start))
	}
}

// TestAnUnreachableReceiverIsOneWord: a refused connection and a certificate the
// system does not trust are told apart by word, and neither carries the
// system's own text.
func TestAnUnreachableReceiverIsOneWord(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	f := newFixture(t, "http://"+addr+"/x")
	rep, _ := Run(context.Background(), f.options())
	if rep.Error != ReasonUnreachable {
		t.Errorf("a refused connection: %+v", rep)
	}
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer tls.Close()
	f = newFixture(t, tls.URL)
	rep, _ = Run(context.Background(), f.options())
	if rep.Error != ReasonTLS {
		t.Errorf("an untrusted certificate: %+v", rep)
	}
	// With the server's own certificate trusted, it is delivered.
	pool := x509.NewCertPool()
	pool.AddCert(tls.Certificate())
	o := f.options()
	o.RootCAs = pool
	rep, _ = Run(context.Background(), o)
	if rep.Status != StatusDelivered {
		t.Errorf("a trusted certificate: %+v", rep)
	}
}

// TestADestinationThatFailsTheAddressRuleIsNotPosted: the step holds the address
// to the rule itself, whatever the caller checked.
func TestADestinationThatFailsTheAddressRuleIsNotPosted(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	f := newFixture(t, srv.URL)
	o := f.options()
	o.unchecked = false
	rep, _ := Run(context.Background(), o)
	if rep.Error != ReasonDestination || hits.Load() != 0 {
		t.Errorf("%+v hits %d", rep, hits.Load())
	}
	// No secret, no signature: nothing is sent unsigned.
	o = f.options()
	o.Cred.Secret = ""
	rep, _ = Run(context.Background(), o)
	if rep.Error != ReasonCredential || hits.Load() != 0 {
		t.Errorf("%+v hits %d", rep, hits.Load())
	}
	// A header value that could split the request is refused.
	o = f.options()
	o.Cred.Authorization = "Bearer x\r\nX-Evil: 1"
	rep, _ = Run(context.Background(), o)
	if rep.Error != ReasonCredential || hits.Load() != 0 {
		t.Errorf("%+v hits %d", rep, hits.Load())
	}
}

// TestAProxyRefusalIsTheAddressReason: a proxy that refuses the tunnel with a
// 403 (the host is not one the delivery may reach) is the word "address".
func TestAProxyRefusalIsTheAddressReason(t *testing.T) {
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "forbidden", http.StatusForbidden) }))
	defer px.Close()
	pu, _ := url.Parse(px.URL)
	f := newFixture(t, "https://hook.example/x")
	o := f.options()
	o.Proxy = pu
	rep, _ := Run(context.Background(), o)
	if rep.Status != StatusFailed || rep.Error != ReasonAddress {
		t.Errorf("%+v", rep)
	}
}

// TestADeliveryThroughTheRealProxy: the step's own tunnel works against the
// delivery proxy, for a plain-http destination on the person's network, and the
// proxy's rule refuses a private answer for an ordinary destination.
func TestADeliveryThroughTheRealProxy(t *testing.T) {
	got := make(chan received, 1)
	srv := httptest.NewServer(reference(t, got, http.StatusOK))
	defer srv.Close()
	hostport := strings.TrimPrefix(srv.URL, "http://")
	px := &netproxy.Proxy{Allow: []string{hostport}, PlainHTTP: []string{hostport}, NetworkOK: true}
	ps := httptest.NewServer(px.Handler())
	defer ps.Close()
	pu, _ := url.Parse(ps.URL)
	f := newFixture(t, srv.URL+"/hook")
	f.job.Files = []File{stage(t, f.staging, "f0", "reports/a.md", "a")}
	o := f.options()
	o.Proxy = pu
	rep, _ := Run(context.Background(), o)
	if rep.Status != StatusDelivered {
		t.Fatalf("%+v", rep)
	}
	<-got
	// The same address behind a public-only proxy is refused.
	px2 := &netproxy.Proxy{Allow: []string{hostport}, PublicOnly: true}
	ps2 := httptest.NewServer(px2.Handler())
	defer ps2.Close()
	pu2, _ := url.Parse(ps2.URL)
	o.Proxy = pu2
	rep, _ = Run(context.Background(), o)
	if rep.Status != StatusFailed || rep.Error != ReasonAddress {
		t.Errorf("a loopback address behind a public-only proxy: %+v", rep)
	}
}

// TestCheckURL holds the address rule: what a destination may name, and what is
// refused however it is written.
func TestCheckURL(t *testing.T) {
	ok := []struct {
		raw  string
		net  bool
		want string
	}{
		{"https://hooks.example.com/in/abc?token=1", false, "hooks.example.com:443"},
		{"HTTPS://Hooks.Example.COM:443/x", false, "hooks.example.com:443"},
		{"https://8.8.8.8/x", false, "8.8.8.8:443"},
		{"https://n8n.lan:5678/webhook/x", true, "n8n.lan:5678"},
		{"http://192.168.1.20:5678/x", true, "192.168.1.20:5678"},
		{"http://10.0.0.5/x", true, "10.0.0.5:80"},
		{"https://100.101.102.103/x", true, "100.101.102.103:443"},
	}
	for _, c := range ok {
		got, err := CheckURL(c.raw, c.net)
		if err != nil || got.HostPort != c.want {
			t.Errorf("%s: %v, %+v", c.raw, err, got)
		}
	}
	bad := []struct {
		raw string
		net bool
	}{
		{"", false},
		{"http://hooks.example.com/x", false},
		{"ftp://hooks.example.com/x", true},
		{"https://user:pass@hooks.example.com/x", false},
		{"https://user@hooks.example.com/x", true},
		{"https://hooks.example.com:8443/x", false},
		{"https://hooks.example.com:0/x", true},
		{"https://hooks.example.com:65536/x", true},
		{"https://hooks.example.com/x#frag", false},
		{"https://hooks.example.com/ x", false},
		{"https://hooks.example.com/x\r\nX: y", false},
		{"https://hooks.exämple.com/x", false},
		{"https://[2001:4860:4860::8888]/x", false},
		{"https://[::1]:8443/x", true},
		{"https://127.0.0.1/x", false},
		{"https://127.0.0.1:8443/x", true},
		{"http://localhost:8080/x", true},
		{"http://app.localhost/x", true},
		{"https://0.0.0.0/x", true},
		{"https://10.0.0.5/x", false},
		{"https://192.168.1.1/x", false},
		{"https://169.254.169.254/latest/meta-data", false},
		{"https://169.254.169.254/latest/meta-data", true},
		{"https://100.100.100.200/x", false},
		{"https://2130706433/x", true},
		{"https://0x7f.1/x", true},
		{"https://0x7f000001/x", true},
		{"https://127.1/x", true},
		{"https://127.0.0.0x1/x", true},
		{"https://0177.0.0.1/x", true},
		{"https://1.2.3/x", false},
		{"https://hooks.example.com./x", false},
		{"https://-bad.example.com/x", false},
		{"https://a..b/x", false},
		{"https://under_score.example.com/x", false},
		{"https:hooks.example.com/x", false},
		{"https:///x", false},
		{"//hooks.example.com/x", false},
		{"https://hooks.example.com%2eevil.com/x", false},
		{"https://" + strings.Repeat("a", 64) + ".com/x", false},
		{"https://hooks.example.com/" + strings.Repeat("a", 3000), false},
	}
	for _, c := range bad {
		if got, err := CheckURL(c.raw, c.net); err == nil {
			t.Errorf("%q (on my network %v) was taken: %+v", c.raw, c.net, got)
		}
	}
}

// TestSign pins the signature recipe a receiver follows.
func TestSign(t *testing.T) {
	mac := hmac.New(sha256.New, []byte("k"))
	mac.Write([]byte("1700000000.{\"a\":1}"))
	if got, want := Sign([]byte("k"), "1700000000", []byte(`{"a":1}`)), hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Errorf("%s != %s", got, want)
	}
}

// TestAResultTheBodyCannotCarryIsLeftOutAndSaid: a result that would pass the cap
// once escaped is judged by the bytes it is sent as, and one that is not valid
// UTF-8 or is nested deeper than a receiver reads is left out with its word, so
// the body is always valid text within its cap.
func TestAResultTheBodyCannotCarryIsLeftOutAndSaid(t *testing.T) {
	for name, c := range map[string]struct {
		result json.RawMessage
		word   string
	}{
		"markup":       {json.RawMessage(`"` + strings.Repeat("<&>", int(DefaultMaxBody/6)-10) + `"`), StatusDelivered},
		"line breaks":  {json.RawMessage(`"` + strings.Repeat(" ", int(DefaultMaxBody/6)) + `"`), ReasonTooLarge},
		"not utf-8":    {json.RawMessage("\"a\xff\xfeb\""), ReasonUnreadable},
		"deep":         {json.RawMessage(strings.Repeat("[", 5000) + strings.Repeat("]", 5000)), ReasonUnreadable},
		"not json":     {json.RawMessage("{"), ReasonUnreadable},
		"nesting fine": {json.RawMessage(strings.Repeat("[", 60) + strings.Repeat("]", 60)), StatusDelivered},
	} {
		f := newFixture(t, "https://x.example/")
		f.job.Result = c.result
		body, _, word := buildBody(f.options())
		if int64(len(body)) > DefaultMaxBody || !utf8.Valid(body) || !json.Valid(body) {
			t.Errorf("%s: a body of %d bytes (valid utf-8 %v, json %v)", name, len(body), utf8.Valid(body), json.Valid(body))
		}
		if word != c.word {
			t.Errorf("%s: the result's word is %q, want %q", name, word, c.word)
		}
		if c.word != StatusDelivered && !strings.Contains(string(body), `"resultOmitted":"`+c.word+`"`) {
			t.Errorf("%s: the body does not say why the result is out", name)
		}
	}
}

// TestEntriesAfterABigFileStayWithinTheCap: room is kept for the listing of every
// later file, so a large file early in the job cannot push the body past its cap.
func TestEntriesAfterABigFileStayWithinTheCap(t *testing.T) {
	f := newFixture(t, "https://x.example/")
	f.job.Files = append(f.job.Files, stage(t, f.staging, "f0000", "reports/big.bin", strings.Repeat("x", 7_800_000)))
	for i := 1; i < 200; i++ {
		file := stage(t, f.staging, fmt.Sprintf("f%04d", i), "reports/"+strings.Repeat("p", 100)+fmt.Sprintf("%d.md", i), "later")
		file.Title = strings.Repeat("t", 120)
		file.Omitted = ReasonTooLarge
		f.job.Files = append(f.job.Files, file)
	}
	body, _, _ := buildBody(f.options())
	if int64(len(body)) > DefaultMaxBody {
		t.Errorf("a body of %d bytes, %d over its cap", len(body), int64(len(body))-DefaultMaxBody)
	}
}

// TestTheBodyIsNeverOverItsCap: for many jobs of odd sizes, titles and paths
// (markup, line separators, astral characters, controls), a body that could fit
// is within its cap, every file that is not left out carries its content (an empty
// file's too), and every file left out says why.
func TestTheBodyIsNeverOverItsCap(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	alphabets := []string{"abc", "<>&", "  ", "é€𝄞", "\x00\x01\"\\"}
	for it := 0; it < 200; it++ {
		f := newFixture(t, "https://x.example/")
		maxBody := int64(2000 + rng.Intn(60000))
		f.job.MaxBody = maxBody
		f.job.Result = nil
		if rng.Intn(2) == 0 {
			f.job.Result = json.RawMessage(fmt.Sprintf("%q", strings.Repeat("r", rng.Intn(int(maxBody)/2))))
		}
		for i := 0; i < rng.Intn(30); i++ {
			size := rng.Intn(int(maxBody) / 2)
			if rng.Intn(5) == 0 {
				size = 0
			}
			alpha := []rune(alphabets[rng.Intn(len(alphabets))])
			var title strings.Builder
			for j := 0; j < rng.Intn(200); j++ {
				title.WriteRune(alpha[rng.Intn(len(alpha))])
			}
			file := stage(t, f.staging, fmt.Sprintf("f%04d", i), fmt.Sprintf("reports/%s%d.md", strings.Repeat("<", rng.Intn(50)), i), strings.Repeat("x", size))
			file.Title = title.String()
			f.job.Files = append(f.job.Files, file)
		}
		body, files, _ := buildBody(f.options())
		if body == nil {
			continue
		}
		if int64(len(body)) > maxBody {
			t.Fatalf("iteration %d: a body of %d bytes over a cap of %d", it, len(body), maxBody)
		}
		var got struct {
			Deliverables []struct {
				Content *string `json:"content"`
				Omitted string  `json:"omitted"`
			} `json:"deliverables"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		for i, d := range got.Deliverables {
			if (d.Omitted == "") == (d.Content == nil) {
				t.Errorf("iteration %d file %d: content %v and omitted %q", it, i, d.Content != nil, d.Omitted)
			}
			if (d.Omitted == "") != (files[i].Status == "") {
				t.Errorf("iteration %d file %d: the body and the outcome disagree", it, i)
			}
		}
	}
}

// TestAPostThatFailedIsNotADeliveredResult: whichever way the POST fails, the
// result is not recorded as sent.
func TestAPostThatFailedIsNotADeliveredResult(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	f := newFixture(t, "http://"+addr+"/x")
	rep, err := Run(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != StatusFailed || rep.Result != ReasonUnreachable {
		t.Errorf("an unreachable receiver: status %q, result %q", rep.Status, rep.Result)
	}
}

// TestACredentialIsOneTheStepCanSend: a secret that is not valid text, and a
// header value with a control character, are refused before any request.
func TestACredentialIsOneTheStepCanSend(t *testing.T) {
	for _, auth := range []string{"Bearer x\r\nX: y", "Bearer x\x7f", "Bearer x\vy", "Bearer x\x00"} {
		if ValidHeaderValue(auth) {
			t.Errorf("%q was taken as a header value", auth)
		}
	}
	if !ValidHeaderValue("Bearer a\tb") || !ValidHeaderValue("") {
		t.Error("a tab, or no value, was refused")
	}
	for _, s := range []string{"", "a\xffb", "a\nb"} {
		if ValidSecret(s) {
			t.Errorf("%q was taken as a secret", s)
		}
	}
}
