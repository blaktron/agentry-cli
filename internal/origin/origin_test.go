package origin

import (
	"net/http"
	"testing"
)

func TestOf(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://reg.example/api/v1", "https://reg.example"},
		{"HTTPS://Reg.Example.:443/", "https://reg.example"},
		{"http://reg.example:80", "http://reg.example"},
		{"https://reg.example:8443", "https://reg.example:8443"},
		{"http://[::1]:8080/x", "http://[::1]:8080"},
		{"https://bücher.example", "https://xn--bcher-kva.example"},
		{"/relative/path", ""},
		{"::bad url", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := Of(tt.in); got != tt.want {
			t.Errorf("Of(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSame(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"same origin, other path", "https://reg.example/att/1", "https://reg.example", true},
		{"default port spelled out", "https://reg.example:443/x", "https://reg.example", true},
		{"case and trailing dot", "https://REG.example./x", "https://reg.example", true},
		{"punycode and unicode spellings", "https://xn--bcher-kva.example/x", "https://bücher.example", true},
		{"http spelling of an https host", "http://reg.example/att/1", "https://reg.example", false},
		{"http on the https port", "http://reg.example:443/att/1", "https://reg.example", false},
		{"other port", "https://reg.example:8443/x", "https://reg.example", false},
		{"other host", "https://cdn.example/x", "https://reg.example", false},
		{"relative URL", "/att/1", "https://reg.example", false},
		{"both without a host", "/a", "/a", false},
		{"unparsable", "::bad url", "https://reg.example", false},
	}
	for _, tt := range tests {
		if got := Same(tt.a, tt.b); got != tt.want {
			t.Errorf("%s: Same(%q, %q) = %v, want %v", tt.name, tt.a, tt.b, got, tt.want)
		}
	}
}

// hopTransport answers a request with the redirect its table names for the
// request's URL (a 302 to the Location), or 200 when the table has none,
// and records the URL and the Authorization header of every request it
// sees, so a test follows a token across hops without a server on every
// origin.
type hopTransport struct {
	redirects map[string]string
	urls      []string
	auths     []string
}

func (h *hopTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h.urls = append(h.urls, req.URL.String())
	h.auths = append(h.auths, req.Header.Get("Authorization"))
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: req}
	if loc, ok := h.redirects[req.URL.String()]; ok {
		resp.StatusCode = http.StatusFound
		resp.Header.Set("Location", loc)
	}
	return resp, nil
}

func TestGuardKeepsTheTokenOnlyOnTheOriginItWasSentTo(t *testing.T) {
	const start = "https://reg.example/blob/1"
	tests := []struct {
		name      string
		redirects map[string]string
		wantFinal string
		wantToken bool
	}{
		{"same origin, other path", map[string]string{start: "https://reg.example/blob/2"}, "https://reg.example/blob/2", true},
		{"same origin, default port spelled out", map[string]string{start: "https://reg.example:443/blob/2"}, "https://reg.example:443/blob/2", true},
		{"same origin, host in other case", map[string]string{start: "https://REG.example/blob/2"}, "https://REG.example/blob/2", true},
		{"scheme downgrade on the same host", map[string]string{start: "http://reg.example/blob/1"}, "http://reg.example/blob/1", false},
		{"other port on the same host", map[string]string{start: "https://reg.example:8443/blob/1"}, "https://reg.example:8443/blob/1", false},
		{"http on the https port", map[string]string{start: "http://reg.example:443/blob/1"}, "http://reg.example:443/blob/1", false},
		{"other host", map[string]string{start: "https://cdn.example/blob/1"}, "https://cdn.example/blob/1", false},
		{"subdomain of the registry's host", map[string]string{start: "https://cdn.reg.example/blob/1"}, "https://cdn.reg.example/blob/1", false},
		{"left and back on the second hop", map[string]string{start: "http://reg.example/x", "http://reg.example/x": "https://reg.example/y"}, "https://reg.example/y", true},
		{"same origin first, away second", map[string]string{start: "https://reg.example/x", "https://reg.example/x": "http://reg.example/y"}, "http://reg.example/y", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ht := &hopTransport{redirects: tt.redirects}
			req, err := http.NewRequest(http.MethodGet, start, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer tok-123")
			resp, err := Guard(&http.Client{Transport: ht}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if got := ht.urls[len(ht.urls)-1]; got != tt.wantFinal {
				t.Fatalf("last request went to %s, want %s (all: %v)", got, tt.wantFinal, ht.urls)
			}
			want := ""
			if tt.wantToken {
				want = "Bearer tok-123"
			}
			if got := ht.auths[len(ht.auths)-1]; got != want {
				t.Fatalf("last request carried Authorization %q, want %q (all: %q)", got, want, ht.auths)
			}
			if ht.auths[0] != "Bearer tok-123" {
				t.Fatalf("first request carried Authorization %q, want the token", ht.auths[0])
			}
		})
	}
}

func TestGuardLeavesTheCallersClientAndPolicyAlone(t *testing.T) {
	ht := &hopTransport{redirects: map[string]string{"https://reg.example/a": "https://reg.example/b"}}
	var refused bool
	base := &http.Client{Transport: ht, CheckRedirect: func(*http.Request, []*http.Request) error {
		refused = true
		return http.ErrUseLastResponse
	}}
	resp, err := Guard(base).Get("https://reg.example/a")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !refused || resp.StatusCode != http.StatusFound || len(ht.urls) != 1 {
		t.Fatalf("the client's own redirect policy did not decide: refused=%v status=%d urls=%v", refused, resp.StatusCode, ht.urls)
	}
	if base.CheckRedirect == nil {
		t.Fatal("Guard changed the client it was given")
	}
}

func TestGuardStopsAfterTenRedirects(t *testing.T) {
	ht := &hopTransport{redirects: map[string]string{"https://reg.example/loop": "https://reg.example/loop"}}
	if _, err := Guard(&http.Client{Transport: ht}).Get("https://reg.example/loop"); err == nil {
		t.Fatal("a redirect loop was followed without end")
	}
	if len(ht.urls) != 10 {
		t.Fatalf("followed %d requests, want the default 10", len(ht.urls))
	}
}

func TestGuardOfNilIsTheDefaultClient(t *testing.T) {
	if Guard(nil) == nil || Guard(nil) == http.DefaultClient {
		t.Fatal("Guard(nil) must be a guarded copy of the default client")
	}
}
