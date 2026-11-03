package messenger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The HTTP side every client shares: one transport through the relay, and
// one JSON request whose errors never repeat the request's address or the
// credential.

// callTimeout bounds every request but a long poll.
const callTimeout = 30 * time.Second

// maxAnswer bounds what the role reads of one answer.
const maxAnswer = 8 << 20

// newHTTPClient is a client reaching its service through proxy (an http://
// CONNECT proxy, or "" for none), whose answers must begin within header.
func newHTTPClient(proxy string, header time.Duration) (*http.Client, error) {
	tr := &http.Transport{
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: header,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          4,
	}
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil || u.Scheme != "http" || u.Host == "" {
			return nil, fmt.Errorf("the relay's address %q is not an http:// proxy", proxy)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: tr}, nil
}

// answer is one request's answer.
type answer struct {
	status int
	header http.Header
	body   []byte
}

// doJSON sends method to address with body as JSON (none when nil) and the
// header auth, within timeout. what names the request in an error, and
// scrub takes the credential out of every error's text.
func doJSON(ctx context.Context, c *http.Client, method, address, auth string, body any, timeout time.Duration, what string, scrub func(string) string) (answer, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return answer{}, err
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, address, rd)
	if err != nil {
		return answer{}, fmt.Errorf("%s: the request could not be made", what)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("User-Agent", "agentry-messenger")
	resp, err := c.Do(req)
	if err != nil {
		return answer{}, fmt.Errorf("%s: %s", what, scrub(transportError(err)))
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return answer{}, fmt.Errorf("%s: reading the answer: %s", what, scrub(err.Error()))
	}
	return answer{status: resp.StatusCode, header: resp.Header, body: b}, nil
}

// retryAfterHeader is an answer's Retry-After in seconds, or 0.
func retryAfterHeader(h http.Header) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// scrubber takes each of secrets out of text.
func scrubber(what string, secrets ...string) func(string) string {
	return func(text string) string {
		for _, s := range secrets {
			if s != "" {
				text = strings.ReplaceAll(text, s, "["+what+"]")
			}
		}
		return text
	}
}
