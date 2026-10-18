package remote_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	"github.com/blaktron/agentry-cli/pkg/knowledge/remote"
	"github.com/blaktron/agentry-cli/pkg/knowledge/store"
)

// A refusal reaches the person as the registry's own sentence, with the
// store's sentinel where the store acts on it.
func TestRefusalsCarryTheRegistrysSentence(t *testing.T) {
	ctx := context.Background()
	reg := knowledgetest.NewRegistry(t)
	alice, err := remote.New(reg.URL, reg.Token("alice"), nil)
	if err != nil {
		t.Fatal(err)
	}
	dk := bytes.Repeat([]byte{9}, 32)
	wk, err := alice.Wrap(ctx, dk)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := alice.Unwrap(ctx, wk); err != nil || !bytes.Equal(got, dk) {
		t.Fatalf("unwrap for its owner: %v", err)
	}

	bob, _ := remote.New(reg.URL, reg.Token("bob"), nil)
	_, err = bob.Unwrap(ctx, wk)
	if !errors.Is(err, store.ErrKeyNotYours) || !strings.Contains(err.Error(), "does not open for this account") {
		t.Fatalf("another account's unwrap: %v", err)
	}
	if _, err := alice.Create(ctx, "notes", wk); err != nil {
		t.Fatal(err)
	}
	_, err = alice.Create(ctx, "notes", wk)
	if !errors.Is(err, store.ErrRemoteNameTaken) || !strings.Contains(err.Error(), "already keeps a knowledge pack of this name") {
		t.Fatalf("a name taken: %v", err)
	}
	stranger, _ := remote.New(reg.URL, "agyp_not_a_token", nil)
	if _, err := stranger.List(ctx); !errors.Is(err, store.ErrSignedOut) || !strings.Contains(err.Error(), "agentry login") {
		t.Fatalf("an unknown token: %v", err)
	}
	reg.Forbid = true
	if _, err := alice.List(ctx); err == nil || !strings.Contains(err.Error(), "knowledge:read, knowledge:write") {
		t.Fatalf("a token without the scopes: %v", err)
	}
}

func TestNoTokenIsSignedOut(t *testing.T) {
	if _, err := remote.New("https://registry.example", "", nil); !errors.Is(err, store.ErrSignedOut) {
		t.Fatalf("err = %v", err)
	}
}

// roundTripFunc answers requests without a server.
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
		_, _ = c.List(context.Background())
		if len(urls) != 2 || urls[1] != location || auths[0] != "Bearer agyp_tok" || auths[1] != "" {
			t.Fatalf("requests %v carried Authorization %q, want the redirect to %s followed without the token", urls, auths, location)
		}
	}
}
