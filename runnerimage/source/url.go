package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/runnerimage/decode"
)

// The url kind (plan input-sources.md §5, D12; agentry-cli#834): one file
// at a web address, fetched with one https GET through the same proxy as a
// repository, its redirects followed only on the same host, and saved as
// one file under Out, named from the host's Content-Disposition or the
// address's last path segment with the name made safe. The host then
// decodes it as it decodes an upload, so it gets every malware check and
// every decoder, and an archive is unpacked under DR-39's rules.

// KindURL is the url kind.
const KindURL = "url"

const (
	// DefaultURLMaxBytes is a url source's byte cap locally: the most of
	// one document the host reads back from a decode
	// (pkg/runner/decode.MaxDocumentBytes, held equal by a test in
	// pkg/runner/sources).
	DefaultURLMaxBytes = 256 << 20
	// fallbackName names the file when neither the host nor the address
	// gives a usable one.
	fallbackName = "download"
	// maxFileName bounds the saved name, in bytes, below every file
	// system's 255 so the decode's own suffixes still fit.
	maxFileName = 200
	// fileUserAgent names the fetch to the host.
	fileUserAgent = "agentry-runner (input source)"
)

// CheckFileURL holds a url source's address to what a source may be:
// https, a plain host, no credentials, no port but 443, and no fragment
// (which is never sent; the caller drops it). A query is kept: a download
// address often needs one.
func CheckFileURL(raw string) (*url.URL, error) { return checkFileURL(raw, false) }

func checkFileURL(raw string, anyPort bool) (*url.URL, error) {
	u, err := checkAddress(raw, "the file's address", anyPort)
	if err != nil {
		return nil, err
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, errors.New("the file's address carries a fragment")
	}
	return u, nil
}

// fetchURL is one GET of the address, the body saved as one file under
// Out within MaxBytes, or refused whole.
func fetchURL(ctx context.Context, o Options, rep *Report) error {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultURLMaxBytes
	}
	u, err := checkFileURL(o.URL, o.anyPort)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return errors.New("the address is not a URL")
	}
	req.Header.Set("User-Agent", fileUserAgent)
	// The file as the host keeps it: no transfer encoding the transport
	// would undo out of sight.
	req.Header.Set("Accept-Encoding", "identity")
	if o.Token != "" {
		// The bearer goes to this host alone: a redirect is followed only
		// on the same host (newClient), and the proxy reaches no other.
		req.Header.Set("Authorization", "Bearer "+o.Token)
	}
	resp, err := newClient(o, u).Do(req)
	if err != nil {
		return fmt.Errorf("reach %s: %w", u.Host, scrubURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp.StatusCode, o.Token != "", "file")
	}
	if resp.ContentLength > o.MaxBytes {
		rep.Refusal = fmt.Sprintf("the file at the address is %s, past the %s cap", sizeWords(resp.ContentLength), sizeWords(o.MaxBytes))
		return nil
	}
	name := fileName(resp.Header.Get("Content-Disposition"), resp.Request.URL)
	root, err := os.OpenRoot(o.Out)
	if err != nil {
		return fmt.Errorf("the output directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	if entries, err := os.ReadDir(o.Out); err != nil || len(entries) != 0 {
		return errors.New("the output directory is not empty")
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, o.MaxBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		_ = root.Remove(name)
		return fmt.Errorf("read the file from %s: %w", u.Host, scrubURLError(err))
	case n > o.MaxBytes:
		// Never staged in part.
		_ = root.Remove(name)
		rep.Refusal = fmt.Sprintf("the file at the address is larger than the %s cap", sizeWords(o.MaxBytes))
		return nil
	}
	rep.Files, rep.Bytes, rep.Name = 1, n, name
	return nil
}

// newClient is the fetch step's HTTP client: through the proxy, its
// redirects followed only on the same host over https, the one host the
// proxy lets the step reach, so a credential is never sent anywhere else.
func newClient(o Options, u *url.URL) *http.Client {
	tr := o.Transport
	if tr == nil {
		tr = &http.Transport{
			Proxy:                 http.ProxyURL(o.Proxy),
			TLSHandshakeTimeout:   handshakeTimeout,
			ResponseHeaderTimeout: headerTimeout,
			ForceAttemptHTTP2:     true,
		}
	}
	return &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Host, u.Host) {
			return fmt.Errorf("the host redirected to another address (%s), which is not followed", printable(req.URL.Host, 100))
		}
		return nil
	}}
}

// fileName names the saved file: the host's Content-Disposition filename,
// else the last segment of the address the body came from, made safe;
// "download" when neither gives a name.
func fileName(disposition string, from *url.URL) string {
	if _, params, err := mime.ParseMediaType(disposition); err == nil {
		if n := SafeName(params["filename"]); n != "" {
			return n
		}
	}
	if from != nil {
		if n := SafeName(path.Base(from.Path)); n != "" {
			return n
		}
	}
	return fallbackName
}

// SafeName makes a name the host or the address gave a plain file name on
// Linux, macOS and Windows alike, or answers "" when nothing of it is
// left: its last path segment alone; controls, the marks that reorder or
// hide text and Windows' reserved characters as "_"; no trailing dot or
// space; a device name prefixed with "_"; not git's own folder; within
// maxFileName bytes, its extension kept. The result passes portableName
// and DR-39's member-name rule (decode.PlainMemberPath).
func SafeName(raw string) string {
	if i := strings.LastIndexAny(raw, `/\`); i >= 0 {
		raw = raw[i+1:]
	}
	if !utf8.ValidString(raw) {
		raw = strings.ToValidUTF8(raw, "_")
	}
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r < 0x20 || (r >= 0x7f && r <= 0x9f) || (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0xfeff:
			b.WriteRune('_')
		case strings.ContainsRune(`:*?"<>|`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	name := strings.TrimLeft(strings.TrimRight(b.String(), ". "), " ")
	if len(name) > maxFileName {
		ext := path.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		stem := name[:maxFileName-len(ext)]
		for !utf8.ValidString(stem) {
			stem = stem[:len(stem)-1]
		}
		name = strings.TrimRight(stem, ". ") + ext
	}
	if name == "" || name == "." || name == ".." || isGitName(name) {
		return ""
	}
	if !portableName(name) {
		name = "_" + name
	}
	if !portableName(name) || !decode.PlainMemberPath(name) {
		return ""
	}
	return name
}
