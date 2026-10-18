package remote_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/voice/remote"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A registry that answers a request with a redirect off its own origin
// (plain http on its host, another port) does not take the bearer token
// along.
func TestTokenDoesNotFollowARedirectOffTheRegistrysOrigin(t *testing.T) {
	for _, location := range []string{"http://registry.example/api/v1/x", "https://registry.example:8443/api/v1/x"} {
		var urls, auths []string
		hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			urls = append(urls, r.URL.String())
			auths = append(auths, r.Header.Get("Authorization"))
			resp := &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: http.NoBody, Request: r}
			if len(urls) == 1 {
				resp.StatusCode = http.StatusFound
				resp.Header.Set("Location", location)
			}
			return resp, nil
		})}
		c, err := remote.New("https://registry.example", "agyp_tok", hc)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Get(context.Background())
		if len(urls) != 2 || urls[1] != location || auths[0] != "Bearer agyp_tok" || auths[1] != "" {
			t.Fatalf("requests %v carried Authorization %q, want the redirect to %s followed without the token", urls, auths, location)
		}
	}
}
