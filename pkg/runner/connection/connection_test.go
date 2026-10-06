package connection_test

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/mcpauth/mcpauthtest"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
)

// fakeKeys is a keychain in memory; broken refuses every call, as a
// machine with no Secret Service does.
type fakeKeys struct {
	m      map[string]string
	broken bool
}

func (k *fakeKeys) Get(a string) (string, error) {
	if k.broken {
		return "", errors.New("no keychain")
	}
	v, ok := k.m[a]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}

func (k *fakeKeys) Set(a, v string) error {
	if k.broken {
		return errors.New("no keychain")
	}
	k.m[a] = v
	return nil
}

func (k *fakeKeys) Delete(a string) error {
	if k.broken {
		return errors.New("no keychain")
	}
	delete(k.m, a)
	return nil
}

func grant(t *testing.T, v *mcpauthtest.Vendor) *mcpauth.Grant {
	t.Helper()
	g, err := mcpauth.Consent{HTTP: v.Guard().Client(), ServerURL: v.ServerURL(), Present: v.Browser(), Timeout: 10 * time.Second}.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

var release = connection.Key{Ref: "acme/mail@1.0.0", Digest: "sha256:aaaa", Server: "https://mcp.example/mcp"}

// D4/D10: a connection opens for the release that made it alone.
func TestAConnectionOpensForItsReleaseAlone(t *testing.T) {
	v := mcpauthtest.New(t)
	v.DCR = true
	s := connection.Open(t.TempDir(), &fakeKeys{m: map[string]string{}})
	k := release
	k.Server = v.ServerURL()
	if _, err := s.Put(k, grant(t, v), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(k); err != nil {
		t.Fatalf("its own release: %v", err)
	}
	for name, other := range map[string]connection.Key{
		"another QuickAgent":              {Ref: "acme/other@1.0.0", Digest: k.Digest, Server: k.Server},
		"another version":                 {Ref: "acme/mail@1.0.1", Digest: k.Digest, Server: k.Server},
		"the same version, another build": {Ref: k.Ref, Digest: "sha256:bbbb", Server: k.Server},
		"another server":                  {Ref: k.Ref, Digest: k.Digest, Server: "https://mcp.other.example/mcp"},
	} {
		if _, err := s.Get(other); !errors.Is(err, connection.ErrNone) {
			t.Errorf("%s: err = %v, want ErrNone", name, err)
		}
		if _, _, err := s.Use(context.Background(), other, v.Guard().Client(), nil); !errors.Is(err, connection.ErrNone) {
			t.Errorf("%s: Use err = %v, want ErrNone", name, err)
		}
	}
}

// A record renamed onto another release's file name is still not that
// release's connection.
func TestARecordCopiedToAnotherKeyDoesNotOpen(t *testing.T) {
	home := t.TempDir()
	v := mcpauthtest.New(t)
	v.DCR = true
	s := connection.Open(home, nil)
	k := release
	k.Server = v.ServerURL()
	if _, err := s.Put(k, grant(t, v), ""); err != nil {
		t.Fatal(err)
	}
	other := connection.Key{Ref: "acme/other@1.0.0", Digest: k.Digest, Server: k.Server}
	dir := connection.Dir(home)
	for _, ext := range []string{".json", ".secret"} {
		b, err := os.ReadFile(filepath.Join(dir, k.ID()+ext))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, other.ID()+ext), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Get(other); !errors.Is(err, connection.ErrNone) {
		t.Fatalf("a copied record opened for another release: %v", err)
	}
}

func TestUseRefreshesAndWritesTheRotatedTokenBack(t *testing.T) {
	for name, keys := range map[string]*fakeKeys{
		"keychain":    {m: map[string]string{}},
		"no keychain": {m: map[string]string{}, broken: true},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			v := mcpauthtest.New(t)
			v.DCR = true
			s := connection.Open(home, keys)
			k := release
			k.Server = v.ServerURL()
			g := grant(t, v)
			c, err := s.Put(k, g, "")
			if err != nil {
				t.Fatal(err)
			}
			wantIn := "keychain"
			if keys.broken {
				wantIn = "file"
			}
			if c.SecretIn != wantIn {
				t.Fatalf("kept in %q, want %q", c.SecretIn, wantIn)
			}
			a1, _, err := s.Use(context.Background(), k, v.Guard().Client(), nil)
			first := a1.Token
			if err != nil || first == "" {
				t.Fatalf("use: %q %v", first, err)
			}
			// The first refresh spent the consent's refresh token; a second
			// run works only if the rotated one was written back.
			if v.Live(g.Token.RefreshToken) {
				t.Fatal("the vendor did not rotate")
			}
			a2, c, err := s.Use(context.Background(), k, v.Guard().Client(), nil)
			second := a2.Token
			if err != nil || second == "" || second == first {
				t.Fatalf("second run: %q %v", second, err)
			}
			if c.LastUsedAt == "" || c.RefreshedAt == "" {
				t.Fatalf("not stamped: %+v", c)
			}
			// No token and no client secret in the record, raw or base64.
			raw, err := os.ReadFile(filepath.Join(connection.Dir(home), k.ID()+".json"))
			if err != nil {
				t.Fatal(err)
			}
			for _, tok := range append(v.Issued, first, second) {
				if strings.Contains(string(raw), tok) || strings.Contains(string(raw), base64.StdEncoding.EncodeToString([]byte(tok))) {
					t.Fatalf("the record holds a token:\n%s", raw)
				}
			}
			if !keys.broken {
				if _, err := os.Stat(filepath.Join(connection.Dir(home), k.ID()+".secret")); !os.IsNotExist(err) {
					t.Fatal("a keychain-kept secret also has a file")
				}
			} else if fi, err := os.Stat(filepath.Join(connection.Dir(home), k.ID()+".secret")); err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("the secret file: %v %v", fi, err)
			}
		})
	}
}

func TestARefusedRefreshBreaksTheConnection(t *testing.T) {
	v := mcpauthtest.New(t)
	v.DCR = true
	s := connection.Open(t.TempDir(), nil)
	k := release
	k.Server = v.ServerURL()
	if _, err := s.Put(k, grant(t, v), ""); err != nil {
		t.Fatal(err)
	}
	v.Refused = true
	_, _, err := s.Use(context.Background(), k, v.Guard().Client(), nil)
	var broken *connection.BrokenError
	if !errors.As(err, &broken) || !strings.Contains(broken.Reason, "invalid_grant") {
		t.Fatalf("err = %v, want a broken connection", err)
	}
	c, err := s.Get(k)
	if err != nil || c.Broken == "" {
		t.Fatalf("the record is not marked broken: %+v %v", c, err)
	}
	// It stays broken without asking the vendor again.
	v.Refused = false
	before := len(v.Requests)
	if _, _, err := s.Use(context.Background(), k, v.Guard().Client(), nil); !errors.As(err, &broken) || len(v.Requests) != before {
		t.Fatalf("a broken connection was refreshed: %v", err)
	}
}

func TestRemoveRevokesAndRecordsTheOutcome(t *testing.T) {
	home := t.TempDir()
	keys := &fakeKeys{m: map[string]string{}}
	v := mcpauthtest.New(t)
	v.DCR = true
	s := connection.Open(home, keys)
	k := release
	k.Server = v.ServerURL()
	g := grant(t, v)
	if _, err := s.Put(k, g, ""); err != nil {
		t.Fatal(err)
	}
	outcome, err := s.Remove(context.Background(), k, v.Guard().Client(), nil, "the version was uninstalled")
	if err != nil || outcome != connection.RevokeDone {
		t.Fatalf("outcome %q, err %v", outcome, err)
	}
	if v.Live(g.Token.RefreshToken) || len(v.Revoked) != 1 {
		t.Fatalf("not revoked at the vendor: %v", v.Revoked)
	}
	if _, err := s.Get(k); !errors.Is(err, connection.ErrNone) {
		t.Fatalf("still there: %v", err)
	}
	if len(keys.m) != 0 {
		t.Fatalf("the keychain still holds %v", keys.m)
	}
	audit, err := os.ReadFile(filepath.Join(connection.Dir(home), "audit.ndjson"))
	if err != nil || !strings.Contains(string(audit), `"revocation":"revoked"`) || !strings.Contains(string(audit), "uninstalled") {
		t.Fatalf("audit = %s (%v)", audit, err)
	}
	if strings.Contains(string(audit), "PROBE-REFRESH-") {
		t.Fatal("the audit log holds a token")
	}
}

func TestOwnClientsAreKeptOncePerIssuer(t *testing.T) {
	home := t.TempDir()
	keys := &fakeKeys{m: map[string]string{}}
	v := mcpauthtest.New(t)
	v.AddClient("mine", "s3cret-client")
	clients := connection.OpenClients(home, keys)
	if err := clients.Set(v.URL+"/", "mine", "s3cret-client"); err != nil {
		t.Fatal(err)
	}
	got, err := clients.Get(v.URL)
	if err != nil || got.ID != "mine" || got.Secret != "s3cret-client" {
		t.Fatalf("got %+v %v", got, err)
	}
	// A consent and refresh with it: the secret is the client store's,
	// never the connection's.
	g, err := mcpauth.Consent{HTTP: v.Guard().Client(), ServerURL: v.ServerURL(), Present: v.Browser(), OwnClient: clients.Get, Timeout: 10 * time.Second}.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := connection.Open(home, keys)
	k := release
	k.Server = v.ServerURL()
	if _, err := s.Put(k, g, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(keys.m["connection:"+k.ID()], "s3cret-client") {
		t.Fatal("the connection keeps the person's client secret beside its own")
	}
	if _, _, err := s.Use(context.Background(), k, v.Guard().Client(), clients); err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{".json"} {
		matches, _ := filepath.Glob(filepath.Join(connection.Dir(home), "clients", "*"+ext))
		for _, m := range matches {
			b, _ := os.ReadFile(m)
			if strings.Contains(string(b), "s3cret-client") {
				t.Fatalf("%s holds the client secret", m)
			}
		}
	}
	// Forgetting the client breaks the connections made with it.
	if ok, err := clients.Remove(v.URL); !ok || err != nil {
		t.Fatalf("remove: %v %v", ok, err)
	}
	var broken *connection.BrokenError
	if _, _, err := s.Use(context.Background(), k, v.Guard().Client(), clients); !errors.As(err, &broken) {
		t.Fatalf("err = %v, want broken", err)
	}
	if err := clients.Set("http://accounts.example", "x", ""); err == nil {
		t.Fatal("an http issuer was kept")
	}
}

func TestAConnectionWithNoRefreshTokenIsNotKept(t *testing.T) {
	s := connection.Open(t.TempDir(), nil)
	g := &mcpauth.Grant{Token: mcpauth.Token{AccessToken: "a"}}
	if _, err := s.Put(release, g, ""); err == nil || !strings.Contains(err.Error(), "no refresh token") {
		t.Fatalf("err = %v", err)
	}
}
