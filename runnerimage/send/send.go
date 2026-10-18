// Package send is the runner image's `deliver` role: the delivery step that
// hands a finished run's deliverables to a destination the person chose. The
// webhook kind is one HTTPS POST of a JSON body that carries the run's id,
// release reference and digest, its result, and each deliverable's path, title,
// digest and content, signed with an HMAC the receiver can check.
//
// The caller runs this in a container of its own, on a network whose only route
// is a CONNECT proxy to the destination's host and port, with the files it may
// send staged read-only (each already shown to match the digest the run's
// envelope names) and the credential on stdin: never in the environment, an
// argument, a file or the report. The step follows no redirect, reads at most a
// small part of the answer and never records it, and reports each file's outcome
// as one word from a closed list.
//
// This package is part of the embedded runner-image build context and so
// imports nothing outside the standard library and the runner image's own
// packages.
package send

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// The names this package and its caller share.
const (
	// ReportSchema is the report's format; ReportFile is where Run writes it,
	// in the work directory, and JobFile where the caller puts the job it reads.
	ReportSchema = "deliver/v1"
	ReportFile   = "delivery.json"
	JobFile      = "job.json"
	// BodySchema names the format of the body a webhook receives.
	BodySchema = "agentry-delivery/v1"
	// KindWebhook is the one kind this step sends.
	KindWebhook = "webhook"
	// DefaultMaxBody is the most a webhook body is: 10 MiB.
	DefaultMaxBody int64 = 10 << 20
)

// The headers a webhook carries, beside Content-Type and Authorization.
const (
	HeaderSignature = "X-Agentry-Signature"
	HeaderTimestamp = "X-Agentry-Timestamp"
	HeaderRun       = "X-Agentry-Run"
)

// The words a file's outcome and a delivery's reason come in. They are the
// receipt's closed list (the engine's tests hold the two equal): never a
// receiver's text, a path or a system's message.
const (
	StatusDelivered = "delivered"
	StatusRefused   = "refused"
	StatusFailed    = "failed"

	// Why a file's content was not sent.
	ReasonTooLarge     = "too-large"
	ReasonChanged      = "changed"
	ReasonMissing      = "missing"
	ReasonLink         = "link"
	ReasonUnreadable   = "unreadable"
	ReasonOutside      = "outside-reports"
	ReasonTypeRefused  = "type"
	ReasonDestination  = "destination"
	ReasonCredential   = "credential"
	ReasonAddress      = "address"
	ReasonUnreachable  = "unreachable"
	ReasonTLS          = "tls"
	ReasonTimeout      = "timeout"
	ReasonRedirect     = "redirect"
	ReasonReceiverSaid = "status"
	ReasonStep         = "step"
)

// Timings of one POST.
const (
	dialTimeout     = 30 * time.Second
	headerTimeout   = 60 * time.Second
	requestTimeout  = 3 * time.Minute
	maxAnswerBytes  = 64 << 10
	maxCredentialIn = 64 << 10
	userAgent       = "agentry-runner (delivery)"
)

// Job is what the caller asks of the step, written to JobFile.
type Job struct {
	Kind string `json:"kind"`
	// URL is the destination's address, OnMyNetwork its saved "On my network".
	URL         string `json:"url"`
	OnMyNetwork bool   `json:"onMyNetwork,omitempty"`
	// RunID, Ref and Digest identify the run and the exact release that ran.
	RunID  string `json:"runId"`
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
	// Result is the run's final result as its envelope holds it: the parsed
	// JSON a release with an output schema gave, else its text as a JSON string.
	Result json.RawMessage `json:"result,omitempty"`
	// MaxBody is the body's cap in bytes; zero is DefaultMaxBody.
	MaxBody int64  `json:"maxBody,omitempty"`
	Files   []File `json:"files"`
}

// File is one deliverable of the run.
type File struct {
	Path   string `json:"path"`
	Title  string `json:"title"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Type   string `json:"type"`
	Marked bool   `json:"marked"`
	// Staged names the file in the staging folder, holding the bytes the
	// caller found equal to SHA256. Empty when the caller sends none.
	Staged string `json:"staged,omitempty"`
	// Omitted says why the caller sends no content for the file (one of the
	// Reason words), and the body carries the word.
	Omitted string `json:"omitted,omitempty"`
}

// Credential is what arrives on stdin: the HMAC secret signing the body, and the
// value of an Authorization header the receiver wants, when it does.
type Credential struct {
	Secret        string `json:"secret"`
	Authorization string `json:"authorization,omitempty"`
}

// Report is what one delivery did, written to ReportFile.
type Report struct {
	Schema string `json:"schema"`
	Kind   string `json:"kind"`
	// Host is the host and port reached.
	Host string `json:"host"`
	// Status is StatusDelivered or StatusFailed for the POST as a whole, Error
	// the closed reason when it failed.
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// Files is each of the job's files in its order, with what became of it.
	Files []FileResult `json:"files"`
	// Result is the outcome of the run's result: sent in the body, or why not.
	Result string `json:"result,omitempty"`
}

// FileResult is one file's outcome.
type FileResult struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Options is one delivery.
type Options struct {
	Job Job
	// Staging is the read-only folder the job's staged files are in.
	Staging string
	// Work is the folder the report is written to.
	Work string
	Cred Credential
	// Proxy is the CONNECT proxy the POST goes through; nil is a direct
	// connection (tests only).
	Proxy *url.URL
	// Now is the clock the timestamp reads, nil is time.Now.
	Now func() time.Time
	// Transport replaces the HTTP transport (tests only).
	Transport http.RoundTripper
	// RootCAs replaces the system's roots (tests only).
	RootCAs *x509.CertPool
	// unchecked skips the address rule, so a test can post to a server on
	// its own loopback.
	unchecked bool
}

// Run delivers one job and writes its report to Work. The error is for a
// delivery that could not be reported; a delivery that failed is a report.
func Run(ctx context.Context, o Options) (Report, error) {
	rep := Report{Schema: ReportSchema, Kind: o.Job.Kind}
	rep.Files = make([]FileResult, len(o.Job.Files))
	for i, f := range o.Job.Files {
		rep.Files[i] = FileResult{Path: f.Path}
	}
	deliver(ctx, o, &rep)
	if err := writeReport(o.Work, rep); err != nil {
		return rep, err
	}
	return rep, nil
}

func deliver(ctx context.Context, o Options, rep *Report) {
	fail := func(reason string) {
		rep.Status, rep.Error = StatusFailed, reason
		for i := range rep.Files {
			if rep.Files[i].Status == "" {
				rep.Files[i].Status, rep.Files[i].Error = StatusFailed, reason
			}
		}
		if rep.Result == "" {
			rep.Result = reason
		}
	}
	if o.Job.Kind != KindWebhook {
		fail(ReasonDestination)
		return
	}
	target, err := CheckURL(o.Job.URL, o.Job.OnMyNetwork)
	if o.unchecked {
		u, perr := url.Parse(o.Job.URL)
		if perr == nil {
			target, err = Target{URL: u, HostPort: u.Host}, nil
		}
	}
	if err != nil {
		fail(ReasonDestination)
		return
	}
	rep.Host = target.HostPort
	if o.Cred.Secret == "" || strings.ContainsAny(o.Cred.Authorization, "\r\n\x00") {
		fail(ReasonCredential)
		return
	}
	body, files, resultSent := buildBody(o)
	for i := range rep.Files {
		rep.Files[i] = files[i]
	}
	if resultSent {
		rep.Result = StatusDelivered
	} else if len(o.Job.Result) > 0 {
		rep.Result = ReasonTooLarge
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	ts := strconv.FormatInt(now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL.String(), bytes.NewReader(body))
	if err != nil {
		fail(ReasonDestination)
		return
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderSignature, "sha256="+Sign([]byte(o.Cred.Secret), ts, body))
	if o.Job.RunID != "" {
		req.Header.Set(HeaderRun, o.Job.RunID)
	}
	if o.Cred.Authorization != "" {
		req.Header.Set("Authorization", o.Cred.Authorization)
	}
	resp, err := client(o).Do(req)
	if err != nil {
		rep.Status, rep.Error = StatusFailed, classify(err)
		for i := range rep.Files {
			if rep.Files[i].Status == "" {
				rep.Files[i].Status, rep.Files[i].Error = StatusFailed, rep.Error
			}
		}
		return
	}
	defer func() { _ = resp.Body.Close() }()
	// What the receiver answers is read to a bound and never kept.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAnswerBytes))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		rep.Status = StatusDelivered
		for i := range rep.Files {
			if rep.Files[i].Status == "" {
				rep.Files[i].Status = StatusDelivered
			}
		}
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		failSent(rep, ReasonRedirect)
	default:
		failSent(rep, ReasonReceiverSaid)
	}
}

// failSent fails the POST and every file whose content rode in it.
func failSent(rep *Report, reason string) {
	rep.Status, rep.Error = StatusFailed, reason
	for i := range rep.Files {
		if rep.Files[i].Status == "" {
			rep.Files[i].Status, rep.Files[i].Error = StatusFailed, reason
		}
	}
	if rep.Result == StatusDelivered {
		rep.Result = reason
	}
}

// Sign is the signature over a body: the hex HMAC-SHA256, keyed by the
// secret, of the timestamp, a full stop and the body. A receiver recomputes it
// from the X-Agentry-Timestamp header and the raw body, compares in constant
// time, and refuses a timestamp more than a few minutes from its own clock.
func Sign(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// bodyFile is a deliverable in the body.
type bodyFile struct {
	Path    string  `json:"path"`
	Title   string  `json:"title"`
	SHA256  string  `json:"sha256"`
	Bytes   int64   `json:"bytes"`
	Type    string  `json:"type"`
	Marked  bool    `json:"marked"`
	Content *string `json:"content,omitempty"`
	Omitted string  `json:"omitted,omitempty"`
}

// body is the JSON a webhook receives.
type body struct {
	Schema        string          `json:"schema"`
	RunID         string          `json:"runId"`
	Ref           string          `json:"ref"`
	Digest        string          `json:"digest"`
	Result        json.RawMessage `json:"result"`
	ResultOmitted string          `json:"resultOmitted,omitempty"`
	Deliverables  []bodyFile      `json:"deliverables"`
}

// slack is what the size prediction leaves free, so that the body is never
// over its cap by a miscount of JSON's framing.
const slack = 1024

// buildBody assembles the body within the cap and reports each file's outcome
// so far: a file whose content rides in it has none yet (its outcome is the
// POST's), and one left out has the reason. resultSent says the result is in
// the body.
func buildBody(o Options) (out []byte, files []FileResult, resultSent bool) {
	maxBody := o.Job.MaxBody
	if maxBody <= 0 {
		maxBody = DefaultMaxBody
	}
	b := body{Schema: BodySchema, RunID: o.Job.RunID, Ref: o.Job.Ref, Digest: o.Job.Digest, Result: json.RawMessage("null"), Deliverables: []bodyFile{}}
	files = make([]FileResult, len(o.Job.Files))
	// A result is sent when it is JSON and leaves room for the files.
	if r := bytes.TrimSpace(o.Job.Result); len(r) > 0 && json.Valid(r) {
		if int64(len(r)) <= maxBody/2 {
			b.Result, resultSent = json.RawMessage(r), true
		} else {
			b.ResultOmitted = ReasonTooLarge
		}
	}
	framing, _ := json.Marshal(b)
	used := int64(len(framing)) + slack
	root, rerr := os.OpenRoot(o.Staging)
	if rerr == nil {
		defer func() { _ = root.Close() }()
	}
	for i, f := range o.Job.Files {
		e := bodyFile{Path: f.Path, Title: f.Title, SHA256: f.SHA256, Bytes: f.Bytes, Type: f.Type, Marked: f.Marked}
		files[i].Path = f.Path
		omit := func(reason string) {
			e.Omitted = reason
			files[i].Status, files[i].Error = StatusRefused, reason
		}
		meta, _ := json.Marshal(e)
		switch {
		case !plainReportPath(f.Path):
			omit(ReasonOutside)
		case f.Omitted != "":
			omit(f.Omitted)
		case f.Bytes < 0:
			omit(ReasonUnreadable)
		case f.Staged == "" || rerr != nil:
			omit(ReasonMissing)
		case used+int64(len(meta))+int64(len(`,"content":""`))+int64(base64.StdEncoding.EncodedLen(int(min(f.Bytes, 1<<40))))+1 > maxBody:
			omit(ReasonTooLarge)
		default:
			content, reason := readStaged(root, f)
			if reason != "" {
				omit(reason)
			} else {
				e.Content = &content
			}
		}
		m, _ := json.Marshal(e)
		used += int64(len(m)) + 1
		b.Deliverables = append(b.Deliverables, e)
	}
	out, _ = json.Marshal(b)
	return out, files, resultSent
}

// plainReportPath is true for a path beneath reports/ with no way out of it.
func plainReportPath(p string) bool {
	if !strings.HasPrefix(p, "reports/") || path.Clean(p) != p || strings.ContainsAny(p, "\\\x00") {
		return false
	}
	return !strings.Contains(p, "..")
}

// readStaged reads a staged file and answers it base64 encoded, or the reason
// it cannot be sent: it is read as a regular file reached by no link and shown
// again to be the bytes of the file's size and SHA-256.
func readStaged(root *os.Root, f File) (string, string) {
	if f.Staged == "" || strings.ContainsAny(f.Staged, `/\`) || f.Staged == "." || f.Staged == ".." {
		return "", ReasonMissing
	}
	fi, err := root.Lstat(f.Staged)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", ReasonMissing
	case err != nil:
		return "", ReasonUnreadable
	case fi.Mode()&fs.ModeSymlink != 0 || !fi.Mode().IsRegular():
		return "", ReasonLink
	case fi.Size() != f.Bytes:
		return "", ReasonChanged
	}
	h, err := root.Open(f.Staged)
	if err != nil {
		return "", ReasonUnreadable
	}
	defer func() { _ = h.Close() }()
	data, err := io.ReadAll(io.LimitReader(h, f.Bytes+1))
	if err != nil {
		return "", ReasonUnreadable
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != f.Bytes || hex.EncodeToString(sum[:]) != strings.ToLower(f.SHA256) {
		return "", ReasonChanged
	}
	return base64.StdEncoding.EncodeToString(data), ""
}

// client is the POST's HTTP client: through the proxy by a CONNECT of its own
// (so a plain-http destination on the person's network is tunnelled too),
// following no redirect, bounded in every wait.
func client(o Options) *http.Client {
	tr := o.Transport
	if tr == nil {
		t := &http.Transport{
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: o.RootCAs},
			TLSHandshakeTimeout:   dialTimeout,
			ResponseHeaderTimeout: headerTimeout,
			DisableKeepAlives:     true,
		}
		if o.Proxy != nil {
			t.DialContext = dialVia(o.Proxy)
		} else {
			t.DialContext = (&net.Dialer{Timeout: dialTimeout}).DialContext
		}
		tr = t
	}
	return &http.Client{
		Transport: tr,
		Timeout:   requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// A redirect is a failure to report, never a second address to
			// send a signed body and a credential to.
			return http.ErrUseLastResponse
		},
	}
}

// errAddress is a proxy's refusal of the destination: the host or the address
// it resolves to is not one the delivery may reach.
var errAddress = errors.New("the proxy refused the destination's address")

// dialVia opens a tunnel to the address through the proxy.
func dialVia(proxy *url.URL) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: dialTimeout}
	return func(ctx context.Context, _, addr string) (net.Conn, error) {
		conn, err := d.DialContext(ctx, "tcp", proxy.Host)
		if err != nil {
			return nil, err
		}
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stop()
		if dl, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(dl)
		} else {
			_ = conn.SetDeadline(time.Now().Add(dialTimeout))
		}
		if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr); err != nil {
			_ = conn.Close()
			return nil, err
		}
		// One byte at a time would be slow and a buffer could swallow the
		// start of the tunnel: the proxy sends nothing after its answer until
		// the client does, so a buffered reader holds nothing of it.
		br := newLineReader(conn)
		status, err := br.status()
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		_ = conn.SetDeadline(time.Time{})
		switch status {
		case http.StatusOK:
			return conn, nil
		case http.StatusForbidden:
			_ = conn.Close()
			return nil, errAddress
		}
		_ = conn.Close()
		return nil, fmt.Errorf("the proxy answered %d", status)
	}
}

// lineReader reads a proxy's answer a byte at a time, so nothing beyond it is
// consumed.
type lineReader struct{ r io.Reader }

func newLineReader(r io.Reader) *lineReader { return &lineReader{r: r} }

// status reads the status line and the header block that follows it, and
// answers the status code.
func (l *lineReader) status() (int, error) {
	var head []byte
	buf := make([]byte, 1)
	for len(head) < 8192 {
		if _, err := io.ReadFull(l.r, buf); err != nil {
			return 0, err
		}
		head = append(head, buf[0])
		if bytes.HasSuffix(head, []byte("\r\n\r\n")) {
			break
		}
	}
	if !bytes.HasSuffix(head, []byte("\r\n\r\n")) {
		return 0, errors.New("the proxy's answer is not an HTTP answer")
	}
	line, _, _ := strings.Cut(string(head), "\r\n")
	f := strings.Fields(line)
	if len(f) < 2 || !strings.HasPrefix(f[0], "HTTP/1.") {
		return 0, errors.New("the proxy's answer is not an HTTP answer")
	}
	code, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, errors.New("the proxy's answer is not an HTTP answer")
	}
	return code, nil
}

// classify is a failed POST's reason, one closed word.
func classify(err error) string {
	var (
		unknown x509.UnknownAuthorityError
		invalid x509.CertificateInvalidError
		host    x509.HostnameError
		record  tls.RecordHeaderError
		ne      net.Error
	)
	switch {
	case errors.Is(err, errAddress):
		return ReasonAddress
	case errors.Is(err, context.DeadlineExceeded):
		return ReasonTimeout
	case errors.As(err, &unknown), errors.As(err, &invalid), errors.As(err, &host), errors.As(err, &record):
		return ReasonTLS
	case errors.As(err, &ne) && ne.Timeout():
		return ReasonTimeout
	case strings.Contains(err.Error(), "tls:"), strings.Contains(err.Error(), "x509:"):
		return ReasonTLS
	}
	return ReasonUnreachable
}

// writeReport writes the report to Work as a new file.
func writeReport(work string, rep Report) error {
	root, err := os.OpenRoot(work)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	f, err := root.OpenFile(ReportFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(b, '\n'))
	return errors.Join(werr, f.Close())
}

// ReadJob reads the job under dir, a regular file within a bound.
func ReadJob(dir string) (Job, error) {
	var j Job
	root, err := os.OpenRoot(dir)
	if err != nil {
		return j, err
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Lstat(JobFile)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 64<<20 {
		return j, errors.New("the job is not a readable file")
	}
	f, err := root.Open(JobFile)
	if err != nil {
		return j, err
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(io.LimitReader(f, 64<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		return j, errors.New("the job does not decode")
	}
	return j, nil
}

// ReadCredential reads the credential from r, which carries nothing else.
func ReadCredential(r io.Reader) (Credential, error) {
	var c Credential
	b, err := io.ReadAll(io.LimitReader(r, maxCredentialIn+1))
	if err != nil || len(b) > maxCredentialIn {
		return c, errors.New("the credential is unreadable or longer than any credential")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, errors.New("the credential does not decode")
	}
	return c, nil
}
