package send

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/blaktron/agentry-cli/runnerimage/netproxy"
)

const (
	testKeyID     = "AKIAPLANTEDKEYID0123"
	testAccessKey = "PLANTEDaccessSECRET/0123456789+abcdefghij"
)

// TestSigV4MatchesTheServicesPublishedExamples holds the signer to the two
// worked examples of the service's own documentation (a GET of a byte range and
// a PUT with a '$' in its key), so it is checked against an author other than
// this package.
func TestSigV4MatchesTheServicesPublishedExamples(t *testing.T) {
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	const id, secret = "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	get := signV4(id, secret, "us-east-1", at, "GET", "/test.txt", "", map[string]string{
		"host":                 "examplebucket.s3.amazonaws.com",
		"range":                "bytes=0-9",
		"x-amz-content-sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"x-amz-date":           "20130524T000000Z",
	}, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	if want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"; get != want {
		t.Errorf("GET example:\n got %s\nwant %s", get, want)
	}
	put := signV4(id, secret, "us-east-1", at, "PUT", escapeKey("/test$file.text"), "", map[string]string{
		"date":                 "Fri, 24 May 2013 00:00:00 GMT",
		"host":                 "examplebucket.s3.amazonaws.com",
		"x-amz-content-sha256": "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
		"x-amz-date":           "20130524T000000Z",
		"x-amz-storage-class":  "REDUCED_REDUNDANCY",
	}, "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072")
	if want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class, Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"; put != want {
		t.Errorf("PUT example:\n got %s\nwant %s", put, want)
	}
}

// bucketServer is a reference service: it checks each request's signature the
// way a service does, from the headers and path it received, and keeps what it
// was given.
type bucketServer struct {
	t       *testing.T
	mu      sync.Mutex
	objects map[string][]byte
	reqs    []string
	dump    bytes.Buffer
	status  func(n int, path string) int
	n       atomic.Int32
}

func newBucketServer(t *testing.T) (*bucketServer, *httptest.Server) {
	b := &bucketServer{t: t, objects: map[string][]byte{}}
	return b, httptest.NewServer(b)
}

var authRE = regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=([^/]+)/(\d{8})/([^/]+)/s3/aws4_request, SignedHeaders=([a-z0-9;-]+), Signature=([0-9a-f]{64})$`)

func (b *bucketServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	n := int(b.n.Add(1))
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, v := range r.Header {
		b.dump.WriteString(k + ": " + strings.Join(v, ",") + "\n")
	}
	b.dump.WriteString(r.URL.String() + "\n")
	m := authRE.FindStringSubmatch(r.Header.Get("Authorization"))
	if m == nil {
		b.t.Errorf("the Authorization header %q is not a signature version 4 one", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if m[1] != testKeyID {
		b.t.Errorf("the credential names key %q", m[1])
	}
	amz := r.Header.Get("X-Amz-Date")
	at, err := time.Parse("20060102T150405Z", amz)
	if err != nil || time.Since(at) > 5*time.Minute || m[2] != amz[:8] {
		b.t.Errorf("the date %q is not fresh or does not match the scope %q", amz, m[2])
	}
	sum := sha256.Sum256(body)
	if got := r.Header.Get("X-Amz-Content-Sha256"); got != hex.EncodeToString(sum[:]) {
		b.t.Errorf("the declared payload hash %q is not the body's", got)
	}
	if int64(len(body)) != r.ContentLength {
		b.t.Errorf("the body is %d bytes and the length %d", len(body), r.ContentLength)
	}
	hdr := map[string]string{}
	for _, name := range strings.Split(m[4], ";") {
		if name == "host" {
			hdr[name] = r.Host
		} else {
			hdr[name] = r.Header.Get(name)
		}
	}
	want := signV4(testKeyID, testAccessKey, m[3], at, r.Method, r.URL.EscapedPath(), r.URL.RawQuery, hdr, r.Header.Get("X-Amz-Content-Sha256"))
	if !strings.HasSuffix(want, "Signature="+m[5]) {
		b.t.Errorf("the signature does not verify for %s %s", r.Method, r.URL.EscapedPath())
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if !strings.Contains(m[4], "host") || !strings.Contains(m[4], "x-amz-content-sha256") || !strings.Contains(m[4], "x-amz-date") {
		b.t.Errorf("signed headers %q miss one a service needs", m[4])
	}
	if r.Method != http.MethodPut {
		b.t.Errorf("method %s", r.Method)
	}
	b.reqs = append(b.reqs, r.URL.Path)
	st := http.StatusOK
	if b.status != nil {
		st = b.status(n, r.URL.Path)
	}
	if st == http.StatusOK {
		b.objects[r.URL.Path] = body
	}
	w.WriteHeader(st)
	_, _ = w.Write([]byte("service-text-never-kept " + testAccessKey))
}

func s3Fixture(t *testing.T, endpoint string) *fixture {
	t.Helper()
	f := newFixture(t, "")
	f.job = Job{Kind: KindS3, Bucket: "reports-bucket", Prefix: "agentry/out", Region: "eu-west-1", Endpoint: endpoint, RunID: "cafe0000", Ref: "acme/weekly@1.0.0", Digest: "sha256:abc"}
	return f
}

func (f *fixture) s3Options() Options {
	o := f.options()
	o.Cred = Credential{AccessKeyID: testKeyID, SecretAccessKey: testAccessKey}
	return o
}

// TestEachFileIsPutToItsKeyWithASignatureTheServiceVerifies: two files go to
// <bucket>/<prefix>/<run id>/<file> on a path-style endpoint, each signed over
// the digest the envelope named, with the right type; the report says delivered
// and holds nothing the service said, and the secret is in no header, address,
// body or report.
func TestEachFileIsPutToItsKeyWithASignatureTheServiceVerifies(t *testing.T) {
	b, srv := newBucketServer(t)
	defer srv.Close()
	f := s3Fixture(t, srv.URL)
	f.job.Files = []File{
		stage(t, f.staging, "f0", "reports/weekly.md", "# hello\n"),
		stage(t, f.staging, "f1", "reports/data/table.csv", "x,y\n1,2\n"),
		stage(t, f.staging, "f2", "reports/page.html", "<script>alert(1)</script>"),
	}
	rep, err := Run(context.Background(), f.s3Options())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != StatusDelivered || rep.Error != "" {
		t.Fatalf("report: %+v", rep)
	}
	for _, fr := range rep.Files {
		if fr.Status != StatusDelivered || fr.Error != "" {
			t.Errorf("file %+v", fr)
		}
	}
	for path, want := range map[string]string{
		"/reports-bucket/agentry/out/cafe0000/weekly.md":      "# hello\n",
		"/reports-bucket/agentry/out/cafe0000/data/table.csv": "x,y\n1,2\n",
		"/reports-bucket/agentry/out/cafe0000/page.html":      "<script>alert(1)</script>",
	} {
		if string(b.objects[path]) != want {
			t.Errorf("%s holds %q, want %q", path, b.objects[path], want)
		}
	}
	if len(b.objects) != 3 {
		t.Errorf("objects: %v", b.reqs)
	}
	raw, _ := os.ReadFile(filepath.Join(f.work, ReportFile))
	for name, haystack := range map[string]string{"request": b.dump.String(), "report": string(raw)} {
		if strings.Contains(haystack, testAccessKey) && name == "request" {
			// The service's own answer echoes it only in the reply, never the request.
			for _, line := range strings.Split(haystack, "\n") {
				if strings.Contains(line, testAccessKey) {
					t.Errorf("the secret is in a request line: %q", line)
				}
			}
		}
		if name == "report" && (strings.Contains(haystack, testAccessKey) || strings.Contains(haystack, "service-text-never-kept")) {
			t.Errorf("the report holds the secret or the service's text: %s", haystack)
		}
	}
}

// TestAnObjectIsStoredAsItsTypeOrAsADownload: only the types a person's tools
// open as themselves are given their own content type.
func TestAnObjectIsStoredAsItsTypeOrAsADownload(t *testing.T) {
	for p, want := range map[string]string{
		"reports/a.pdf": "application/pdf", "reports/a.md": "text/markdown; charset=utf-8", "reports/a.HTML": "application/octet-stream",
		"reports/a.svg": "application/octet-stream", "reports/a": "application/octet-stream", "reports/a.png": "image/png",
	} {
		if got := contentTypeOf(p); got != want {
			t.Errorf("%s: %s, want %s", p, got, want)
		}
	}
}

// TestAKeyBuiltFromANameTheAgentChoseNeverLeavesItsFolder: whatever the path
// says (dot segments, separators of either kind, a percent-encoded dot, controls
// and bidirectional marks, an enormous name), the key is under
// <prefix>/<run id>/ with every segment plain; a path outside reports/ and a bad
// run id or prefix get no key; two paths that become one key keep both.
func TestAKeyBuiltFromANameTheAgentChoseNeverLeavesItsFolder(t *testing.T) {
	hostile := []string{
		"reports/../../other/secret.txt",
		"reports/a/../../b.md",
		"reports//x.md",
		"reports/..%2f..%2fetc/passwd",
		"reports/%2e%2e/x.md",
		"reports/a\\..\\b.md",
		"reports/a\x00b.md",
		"reports/a\nb.md",
		"reports/txt\u202efdp.exe",
		"reports/" + strings.Repeat("é", 300) + ".pdf",
		"reports/.hidden",
		"reports/...",
		"reports/ ",
		"reports/a:b*c?.md",
		"reports/a/" + strings.Repeat("d/", 600) + "x.md",
		"other/x.md",
		"/reports/x.md",
		"reports",
		"",
	}
	keys := ObjectKeys("agentry/out", "cafe0000", hostile)
	for i, p := range hostile {
		k := keys[i]
		if k == "" {
			continue
		}
		if !strings.HasPrefix(k, "agentry/out/cafe0000/") || len(k) > maxKeyBytes {
			t.Errorf("%q gave key %q outside its folder or too long", p, k)
		}
		for _, seg := range strings.Split(k, "/") {
			if seg == "" || seg == "." || seg == ".." || len(seg) > maxSegmentBytes {
				t.Errorf("%q gave key %q with segment %q", p, k, seg)
			}
		}
		for _, r := range k {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || strings.ContainsRune(`\%:*?"<>|`, r) {
				t.Errorf("%q gave key %q holding %U", p, k, r)
			}
		}
	}
	for i, h := range hostile {
		switch h {
		case "other/x.md", "/reports/x.md", "reports", "", "reports//x.md", "reports/../../other/secret.txt", "reports/a/../../b.md":
			if keys[i] != "" {
				t.Errorf("%q was given key %q", h, keys[i])
			}
		}
	}
	// A key that cannot be made: too deep.
	for i, h := range hostile {
		if strings.Contains(h, "d/d/d/") && keys[i] != "" {
			t.Errorf("a key past %d bytes was made: %d", maxKeyBytes, len(keys[i]))
		}
	}
	for _, bad := range [][2]string{{"../x", "cafe0000"}, {"a//b", "cafe0000"}, {"/a", "cafe0000"}, {"a", "../x"}, {"a", "a/b"}, {"a", ""}, {"a b", "cafe0000"}} {
		if k := ObjectKeys(bad[0], bad[1], []string{"reports/x.md"})[0]; k != "" {
			t.Errorf("prefix %q run %q gave key %q", bad[0], bad[1], k)
		}
	}
	same := ObjectKeys("", "cafe0000", []string{"reports/a:b.md", "reports/a_b.md", "reports/a?b.md"})
	if same[0] != "cafe0000/a_b.md" || same[1] != "cafe0000/a_b (2).md" || same[2] != "cafe0000/a_b (3).md" {
		t.Errorf("collisions: %v", same)
	}
}

// TestAPathTheStepIsGivenCannotEscapeInTheRequest: the request path the service
// sees for a hostile file name is the plain key, percent-encoded once, never a
// dot segment.
func TestAPathTheStepIsGivenCannotEscapeInTheRequest(t *testing.T) {
	b, srv := newBucketServer(t)
	defer srv.Close()
	f := s3Fixture(t, srv.URL)
	f.job.Files = []File{
		stage(t, f.staging, "f0", "reports/../x.md", "a"),
		stage(t, f.staging, "f1", "reports/a b#c?d.md", "b"),
		stage(t, f.staging, "f2", "reports/é.md", "c"),
	}
	rep, _ := Run(context.Background(), f.s3Options())
	if rep.Files[0].Status != StatusRefused || rep.Files[0].Error != ReasonOutside {
		t.Errorf("a dot-dot path: %+v", rep.Files[0])
	}
	for _, p := range b.reqs {
		if !strings.HasPrefix(p, "/reports-bucket/agentry/out/cafe0000/") || strings.Contains(p, "..") {
			t.Errorf("request path %q", p)
		}
	}
	if len(b.reqs) != 2 {
		t.Errorf("requests: %v", b.reqs)
	}
}

// TestAFilePastTheCapIsNeverRead: a file larger than the object cap is refused
// with the closed reason and no request is made for it, and the others still go.
func TestAFilePastTheCapIsNeverRead(t *testing.T) {
	b, srv := newBucketServer(t)
	defer srv.Close()
	f := s3Fixture(t, srv.URL)
	f.job.MaxObject = 1000
	f.job.Files = []File{
		stage(t, f.staging, "f0", "reports/big.pdf", strings.Repeat("x", 1001)),
		stage(t, f.staging, "f1", "reports/ok.md", strings.Repeat("y", 1000)),
	}
	rep, _ := Run(context.Background(), f.s3Options())
	if rep.Files[0].Status != StatusRefused || rep.Files[0].Error != ReasonTooLarge {
		t.Errorf("the big file: %+v", rep.Files[0])
	}
	if rep.Files[1].Status != StatusDelivered {
		t.Errorf("the file within the cap: %+v", rep.Files[1])
	}
	if len(b.reqs) != 1 || rep.Status != StatusFailed || rep.Error != ReasonTooLarge {
		t.Errorf("requests %v, report %+v", b.reqs, rep)
	}
	// A file that claims to be small and is larger on disk is changed, not sent.
	f2 := s3Fixture(t, srv.URL)
	f2.job.Files = []File{stage(t, f2.staging, "f0", "reports/a.md", "abc")}
	f2.job.Files[0].Bytes = 2
	rep, _ = Run(context.Background(), f2.s3Options())
	if rep.Files[0].Error != ReasonChanged || len(b.reqs) != 1 {
		t.Errorf("a file longer than listed: %+v, requests %v", rep.Files[0], b.reqs)
	}
	// And the default cap is the largest file a run can write.
	if DefaultMaxObject != 64<<20 {
		t.Errorf("DefaultMaxObject %d", DefaultMaxObject)
	}
}

// TestAChangedStagedFileIsRefusedAndNeverSent: bytes that are not the digest the
// envelope names, a link in the staging folder, and a file that is missing are
// each refused with a closed word and make no request.
func TestAChangedStagedFileIsRefusedAndNeverSent(t *testing.T) {
	b, srv := newBucketServer(t)
	defer srv.Close()
	f := s3Fixture(t, srv.URL)
	a := stage(t, f.staging, "f0", "reports/a.md", "original")
	if err := os.WriteFile(filepath.Join(f.staging, "f0"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.staging, "f1")); err != nil {
		t.Fatal(err)
	}
	l := File{Path: "reports/l.md", SHA256: a.SHA256, Bytes: 6, Staged: "f1"}
	m := File{Path: "reports/m.md", SHA256: a.SHA256, Bytes: 1, Staged: "nope"}
	f.job.Files = []File{a, l, m}
	rep, _ := Run(context.Background(), f.s3Options())
	want := []string{ReasonChanged, ReasonLink, ReasonMissing}
	for i, fr := range rep.Files {
		if fr.Status != StatusRefused || fr.Error != want[i] {
			t.Errorf("file %d: %+v, want %s", i, fr, want[i])
		}
	}
	if len(b.reqs) != 0 {
		t.Errorf("requests: %v", b.reqs)
	}
}

// TestARefusalOfTheSenderEndsTheDelivery: a 403 fails the file and the ones after
// it with one closed word, and no more is asked of the service; a 500 fails only
// its own file.
func TestARefusalOfTheSenderEndsTheDelivery(t *testing.T) {
	b, srv := newBucketServer(t)
	defer srv.Close()
	b.status = func(n int, _ string) int {
		if n == 1 {
			return http.StatusForbidden
		}
		return http.StatusOK
	}
	f := s3Fixture(t, srv.URL)
	f.job.Files = []File{stage(t, f.staging, "f0", "reports/a.md", "a"), stage(t, f.staging, "f1", "reports/b.md", "b")}
	rep, _ := Run(context.Background(), f.s3Options())
	if rep.Status != StatusFailed || rep.Error != ReasonReceiverSaid || len(b.objects) != 0 {
		t.Errorf("report %+v objects %v", rep, b.objects)
	}
	for i, fr := range rep.Files {
		if fr.Status != StatusFailed || fr.Error != ReasonReceiverSaid {
			t.Errorf("file %d: %+v", i, fr)
		}
	}
	if b.n.Load() != 1 {
		t.Errorf("the service was asked %d times after a 403", b.n.Load())
	}
	b2, srv2 := newBucketServer(t)
	defer srv2.Close()
	b2.status = func(n int, _ string) int {
		if n == 1 {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	}
	f2 := s3Fixture(t, srv2.URL)
	f2.job.Files = []File{stage(t, f2.staging, "f0", "reports/a.md", "a"), stage(t, f2.staging, "f1", "reports/b.md", "b")}
	rep, _ = Run(context.Background(), f2.s3Options())
	if rep.Files[0].Status != StatusFailed || rep.Files[1].Status != StatusDelivered || rep.Status != StatusFailed {
		t.Errorf("a 500 on the first file: %+v", rep)
	}
}

// TestARedirectIsNeverFollowed: a service that answers with a redirect to another
// host is reported as one and the other host is never asked.
func TestARedirectIsNeverFollowed(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	f := s3Fixture(t, srv.URL)
	f.job.Files = []File{stage(t, f.staging, "f0", "reports/a.md", "a"), stage(t, f.staging, "f1", "reports/b.md", "b")}
	rep, _ := Run(context.Background(), f.s3Options())
	if rep.Status != StatusFailed || rep.Error != ReasonRedirect || hits.Load() != 0 {
		t.Errorf("report %+v, redirected-to host asked %d times", rep, hits.Load())
	}
}

// TestACredentialThatIsNotOneIsRefusedBeforeAnything: an access key id with a
// slash (which the signature's scope would misread), a secret with a line break
// and an empty pair each fail the delivery with "credential" and no request.
func TestACredentialThatIsNotOneIsRefusedBeforeAnything(t *testing.T) {
	b, srv := newBucketServer(t)
	defer srv.Close()
	for _, c := range []Credential{
		{AccessKeyID: "AK/IA", SecretAccessKey: testAccessKey},
		{AccessKeyID: testKeyID, SecretAccessKey: "a\nb"},
		{AccessKeyID: testKeyID},
		{SecretAccessKey: testAccessKey},
		{Secret: testSecret},
	} {
		f := s3Fixture(t, srv.URL)
		f.job.Files = []File{stage(t, f.staging, "f0", "reports/a.md", "a")}
		o := f.s3Options()
		o.Cred = c
		rep, _ := Run(context.Background(), o)
		if rep.Status != StatusFailed || rep.Error != ReasonCredential || rep.Files[0].Error != ReasonCredential {
			t.Errorf("%+v: %+v", c, rep)
		}
	}
	if len(b.reqs) != 0 {
		t.Errorf("requests: %v", b.reqs)
	}
}

// TestADestinationTheStepDoesNotAcceptIsRefused: the step applies the address
// rule again, whatever the job says: a private endpoint without the tick, a
// plain-http one, a bad bucket, a run id that reads as a path.
func TestADestinationTheStepDoesNotAcceptIsRefused(t *testing.T) {
	for name, mut := range map[string]func(*Job){
		"private endpoint": func(j *Job) { j.Endpoint = "https://10.0.0.5" },
		"loopback":         func(j *Job) { j.Endpoint = "https://127.0.0.1" },
		"metadata":         func(j *Job) { j.Endpoint = "http://169.254.169.254"; j.OnMyNetwork = true },
		"http":             func(j *Job) { j.Endpoint = "http://minio.example.com" },
		"path in endpoint": func(j *Job) { j.Endpoint = "https://minio.example.com/bucket" },
		"bad bucket":       func(j *Job) { j.Bucket = "../x" },
		"bad prefix":       func(j *Job) { j.Prefix = "a/../b" },
		"bad region":       func(j *Job) { j.Region = "x/y" },
		"run id a path":    func(j *Job) { j.RunID = "../../x" },
		"webhook url":      func(j *Job) { j.Kind = "ftp" },
	} {
		f := s3Fixture(t, "https://minio.example.com")
		mut(&f.job)
		f.job.Files = []File{stage(t, f.staging, "f0", "reports/a.md", "a")}
		o := f.s3Options()
		o.unchecked = false
		rep, _ := Run(context.Background(), o)
		if rep.Status != StatusFailed || rep.Error != ReasonDestination {
			t.Errorf("%s: %+v", name, rep)
		}
	}
}

// TestAnS3DeliveryThroughTheRealProxy: the tunnel works to an endpoint on the
// person's network, and a public-only proxy refuses the same address.
func TestAnS3DeliveryThroughTheRealProxy(t *testing.T) {
	b, srv := newBucketServer(t)
	defer srv.Close()
	hostport := strings.TrimPrefix(srv.URL, "http://")
	px := &netproxy.Proxy{Allow: []string{hostport}, PlainHTTP: []string{hostport}, NetworkOK: true}
	ps := httptest.NewServer(px.Handler())
	defer ps.Close()
	pu, _ := url.Parse(ps.URL)
	f := s3Fixture(t, srv.URL)
	f.job.Files = []File{stage(t, f.staging, "f0", "reports/a.md", "a")}
	o := f.s3Options()
	o.Proxy = pu
	rep, _ := Run(context.Background(), o)
	if rep.Status != StatusDelivered || len(b.objects) != 1 {
		t.Fatalf("%+v %v", rep, b.reqs)
	}
	px2 := &netproxy.Proxy{Allow: []string{hostport}, PublicOnly: true}
	ps2 := httptest.NewServer(px2.Handler())
	defer ps2.Close()
	pu2, _ := url.Parse(ps2.URL)
	o.Proxy = pu2
	rep, _ = Run(context.Background(), o)
	if rep.Status != StatusFailed || rep.Error != ReasonAddress || rep.Files[0].Error != ReasonAddress {
		t.Errorf("a loopback endpoint behind a public-only proxy: %+v", rep)
	}
	if len(b.objects) != 1 {
		t.Errorf("an object was written through a refused tunnel: %v", b.reqs)
	}
}

// TestCheckS3 holds the destination rule.
func TestCheckS3(t *testing.T) {
	ok := []struct {
		s     S3Spec
		host  string
		hp    string
		style bool
	}{
		{S3Spec{Bucket: "my-bucket", Region: "us-east-1"}, "my-bucket.s3.us-east-1.amazonaws.com", "my-bucket.s3.us-east-1.amazonaws.com:443", false},
		{S3Spec{Bucket: "my-bucket", Prefix: "a/b-c_d.e", Region: "eu-west-1"}, "my-bucket.s3.eu-west-1.amazonaws.com", "my-bucket.s3.eu-west-1.amazonaws.com:443", false},
		{S3Spec{Bucket: "my.bucket", Region: "auto", Endpoint: "https://acct.r2.cloudflarestorage.com"}, "acct.r2.cloudflarestorage.com", "acct.r2.cloudflarestorage.com:443", true},
		{S3Spec{Bucket: "b-1", Region: "us-east-1", Endpoint: "http://192.168.1.20:9000", OnMyNetwork: true}, "192.168.1.20:9000", "192.168.1.20:9000", true},
		{S3Spec{Bucket: "b-1", Region: "us-east-1", Endpoint: "https://minio.lan:9000/", OnMyNetwork: true}, "minio.lan:9000", "minio.lan:9000", true},
	}
	for _, c := range ok {
		got, err := CheckS3(c.s)
		if err != nil || got.Host != c.host || got.HostPort != c.hp || got.PathStyle != c.style {
			t.Errorf("%+v: %+v %v", c.s, got, err)
		}
	}
	bad := []S3Spec{
		{Bucket: "", Region: "us-east-1"},
		{Bucket: "ab", Region: "us-east-1"},
		{Bucket: "UPPER", Region: "us-east-1"},
		{Bucket: "a..b", Region: "us-east-1"},
		{Bucket: "-ab", Region: "us-east-1"},
		{Bucket: "192.168.1.1", Region: "us-east-1"},
		{Bucket: "my.bucket", Region: "us-east-1"},
		{Bucket: "my-bucket", Region: ""},
		{Bucket: "my-bucket", Region: "US-EAST-1"},
		{Bucket: "my-bucket", Region: "a/b"},
		{Bucket: "my-bucket", Region: "us-east-1", Prefix: "/a"},
		{Bucket: "my-bucket", Region: "us-east-1", Prefix: "a/"},
		{Bucket: "my-bucket", Region: "us-east-1", Prefix: "a//b"},
		{Bucket: "my-bucket", Region: "us-east-1", Prefix: "../a"},
		{Bucket: "my-bucket", Region: "us-east-1", Prefix: "a b"},
		{Bucket: "my-bucket", Region: "us-east-1", Prefix: "a%2fb"},
		{Bucket: "my-bucket", Region: "us-east-1", Prefix: strings.Repeat("a", 201)},
		{Bucket: "my-bucket", Region: "us-east-1", OnMyNetwork: true},
		{Bucket: "my-bucket", Region: "us-east-1", Endpoint: "https://10.0.0.5"},
		{Bucket: "my-bucket", Region: "us-east-1", Endpoint: "https://127.0.0.1"},
		{Bucket: "my-bucket", Region: "us-east-1", Endpoint: "https://localhost"},
		{Bucket: "my-bucket", Region: "us-east-1", Endpoint: "http://minio.example.com"},
		{Bucket: "my-bucket", Region: "us-east-1", Endpoint: "https://minio.example.com:9000"},
		{Bucket: "my-bucket", Region: "us-east-1", Endpoint: "https://minio.example.com/b"},
		{Bucket: "my-bucket", Region: "us-east-1", Endpoint: "https://minio.example.com?x=1"},
		{Bucket: "my-bucket", Region: "us-east-1", Endpoint: "https://u:p@minio.example.com"},
		{Bucket: "my-bucket", Region: "us-east-1", Endpoint: "http://169.254.169.254", OnMyNetwork: true},
	}
	for _, s := range bad {
		if got, err := CheckS3(s); err == nil {
			t.Errorf("%+v was accepted: %+v", s, got)
		}
	}
	a, _ := CheckS3(S3Spec{Bucket: "one-bucket", Region: "us-east-1"})
	b, _ := CheckS3(S3Spec{Bucket: "two-bucket", Region: "eu-west-1"})
	c, _ := CheckS3(S3Spec{Bucket: "x-bucket", Region: "auto", Endpoint: "https://acct.r2.cloudflarestorage.com"})
	d, _ := CheckS3(S3Spec{Bucket: "y-bucket", Region: "auto", Endpoint: "https://other.r2.cloudflarestorage.com"})
	if !SameService(a, b) || SameService(a, c) || SameService(c, d) || !SameService(c, c) {
		t.Error("SameService")
	}
}
