package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/mcpauth/mcpauthtest"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// connectFixture points the CLI's three connects at fakes, with a browser
// that approves, the model key's keychain in memory, and Poe's and Hugging
// Face's client ids set to the fakes'.
type connectFixture struct {
	or   *mcpauthtest.OpenRouter
	poe  *mcpauthtest.Poe
	hf   *mcpauthtest.HuggingFace
	mu   sync.Mutex
	keys map[string]string
}

func (f *connectFixture) key(p string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keys[p]
}

func newConnectFixture(t *testing.T) *connectFixture {
	t.Helper()
	isolateEnv(t)
	t.Setenv("AGENTRY_RUNNER_HOME", t.TempDir())
	t.Setenv("AGENTRY_NO_BROWSER", "")
	t.Setenv(modelkey.EnvNoKeyring, "")
	f := &connectFixture{or: mcpauthtest.NewOpenRouter(t), poe: mcpauthtest.NewPoe(t), hf: mcpauthtest.NewHuggingFace(t), keys: map[string]string{}}
	f.poe.ExpiresIn = 30 * 24 * 3600
	t.Setenv(mcpauth.PoeClientIDEnv, f.poe.ClientID)
	t.Setenv(mcpauth.HuggingFaceClientIDEnv, f.hf.AppID)
	pool := x509.NewCertPool()
	for _, c := range []*x509.Certificate{f.or.Certificate(), f.poe.Certificate(), f.hf.Certificate()} {
		pool.AddCert(c)
	}
	tlsc := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	guard := mcpauth.Guard{AllowLoopback: true, TLS: tlsc}
	browser := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsc}}
	prev := []any{vendorHTTP, connectionKeys, openBrowser, openRouterEndpoints, poeEndpoints, huggingFaceEndpoints, connectStoreKey, connectDeleteKey}
	vendorHTTP = guard.Client
	connectionKeys = func() connection.Keychain { return nil }
	openBrowser = func(u string) error {
		go func() {
			if res, err := browser.Get(u); err == nil {
				_ = res.Body.Close()
			}
		}()
		return nil
	}
	openRouterEndpoints = func(o *mcpauth.OpenRouter) { o.AuthURL, o.KeysURL = f.or.URL+"/auth", f.or.URL+"/api/v1/auth/keys" }
	poeEndpoints = func(p *mcpauth.Poe) {
		p.AuthorizeURL, p.TokenURL, p.BalanceURL = f.poe.URL+"/oauth/authorize", f.poe.URL+"/token", f.poe.URL+"/usage/current_balance"
	}
	huggingFaceEndpoints = func(h *mcpauth.HuggingFace) { h.Issuer = f.hf.URL }
	connectStoreKey = func(p, k string) error { f.mu.Lock(); defer f.mu.Unlock(); f.keys[p] = k; return nil }
	connectDeleteKey = func(p string) error { f.mu.Lock(); defer f.mu.Unlock(); delete(f.keys, p); return nil }
	t.Cleanup(func() {
		vendorHTTP = prev[0].(func() *http.Client)
		connectionKeys = prev[1].(func() connection.Keychain)
		openBrowser = prev[2].(func(string) error)
		openRouterEndpoints = prev[3].(func(*mcpauth.OpenRouter))
		poeEndpoints = prev[4].(func(*mcpauth.Poe))
		huggingFaceEndpoints = prev[5].(func(*mcpauth.HuggingFace))
		connectStoreKey = prev[6].(func(string, string) error)
		connectDeleteKey = prev[7].(func(string) error)
	})
	t.Cleanup(modelsignin.RegisterVendor(modelsignin.HuggingFace, modelsignin.HuggingFaceVendor{Flow: mcpauth.HuggingFace{KeyConnect: mcpauth.KeyConnect{HTTP: guard.Client()}, Issuer: f.hf.URL}}))
	return f
}

// Each connect keeps the key it minted in the model key's keychain entry,
// says where, and prints it nowhere (I6); list shows it connected; remove
// forgets it and names the vendor's page (OpenRouter, Poe), or says Hugging
// Face offers no revocation.
func TestModelSignInsConnectEachAccountThenDisconnect(t *testing.T) {
	f := newConnectFixture(t)
	for _, c := range []struct{ vendor, page string }{
		{"openrouter", mcpauth.OpenRouterKeysPage},
		{"poe", mcpauth.PoeKeysPage},
		{"huggingface", "not offered by Hugging Face"},
	} {
		out, errOut, err := runCLI(t, strings.NewReader(""), "model-signins", "add", c.vendor, "--json")
		if err != nil {
			t.Fatalf("%s: add: %v\n%s", c.vendor, err, errOut)
		}
		var got signedInJSON
		if err := json.Unmarshal([]byte(out), &got); err != nil || got.Vendor != c.vendor || got.Kind != policy.KeyKindAPI || !got.Connected {
			t.Fatalf("%s: add = %s (%v)", c.vendor, out, err)
		}
		key := f.key(c.vendor)
		if key == "" || strings.Contains(out+errOut, key) || strings.Contains(out+errOut, "PROBE-") {
			t.Fatalf("%s: the key is not kept, or was printed:\n%s\n%s", c.vendor, out, errOut)
		}
		if !strings.Contains(errOut, "in your browser:") {
			t.Errorf("%s: the address was not shown:\n%s", c.vendor, errOut)
		}
		if c.vendor == "poe" && got.KeyExpiresAt == "" {
			t.Errorf("poe: the key's expiry was not said")
		}
	}
	out, _, err := runCLI(t, nil, "model-signins", "list")
	if err != nil || strings.Count(out, "· connected") != 3 || !strings.Contains(out, "poe as person@example.com") || !strings.Contains(out, "key expires") {
		t.Fatalf("list = %q (%v)", out, err)
	}
	for _, c := range []struct{ vendor, want string }{
		{"openrouter", mcpauth.OpenRouterKeysPage},
		{"poe", mcpauth.PoeKeysPage},
		{"huggingface", "not offered by Hugging Face"},
	} {
		out, _, err := runCLI(t, nil, "model-signins", "remove", c.vendor)
		if err != nil || !strings.Contains(out, "disconnected your") || !strings.Contains(out, c.want) {
			t.Fatalf("%s: remove = %q (%v)", c.vendor, out, err)
		}
		if f.key(c.vendor) != "" {
			t.Fatalf("%s: the key stayed in the keychain", c.vendor)
		}
	}
}

// A Hugging Face run renews its connected token at start into the entry the
// run reads; a run whose variable is set, or a hosted run, is left alone.
func TestARunRenewsAConnectedHuggingFaceToken(t *testing.T) {
	f := newConnectFixture(t)
	if _, errOut, err := runCLI(t, strings.NewReader(""), "model-signins", "add", "huggingface"); err != nil {
		t.Fatalf("add: %v\n%s", err, errOut)
	}
	first := f.key("huggingface")
	plan := &spec.Plan{Harness: spec.HarnessSpec{Agent: spec.ContainerSpec{Env: []spec.EnvVar{
		{Name: "HF_TOKEN", FromHostEnv: "HF_TOKEN", FromKeyring: modelkey.Account("huggingface")},
	}}}}
	home := os.Getenv("AGENTRY_RUNNER_HOME")
	if err := refreshConnectedKey(context.Background(), home, plan, true); err != nil || f.key("huggingface") != first {
		t.Fatalf("a hosted run: %v", err)
	}
	t.Setenv("HF_TOKEN", "hf_from_the_environment")
	if err := refreshConnectedKey(context.Background(), home, plan, false); err != nil || f.key("huggingface") != first {
		t.Fatalf("a set variable wins: %v", err)
	}
	t.Setenv("HF_TOKEN", "")
	if err := refreshConnectedKey(context.Background(), home, plan, false); err != nil {
		t.Fatal(err)
	}
	if now := f.key("huggingface"); now == first || !f.hf.Handed(now) {
		t.Fatalf("the entry holds %q: want a renewed token", now)
	}
	f.hf.Refused = true
	if err := refreshConnectedKey(context.Background(), home, plan, false); err == nil || !strings.Contains(err.Error(), "agentry model-signins add huggingface") {
		t.Fatalf("a refused refresh: %v", err)
	}
}

// setup --connect connects and writes a policy whose key is the keychain
// entry the connect filled; a disabled keychain refuses before any browser.
func TestSetupConnectWritesThePolicy(t *testing.T) {
	f := newConnectFixture(t)
	policyPath := t.TempDir() + "/policy.yaml"
	_, errOut, err := runCLI(t, strings.NewReader(""), "setup", "--provider", "openrouter", "--connect", "--model", "anthropic/claude-sonnet-4.5", "--yes", "--skip-images", "--policy", policyPath)
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, errOut)
	}
	pol, _, err := policy.Load(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	m := pol.Model
	if m.Provider != "openrouter" || m.APIKeySource != modelkey.SourceKeyring || m.APIKeyEnv != "OPENROUTER_API_KEY" || m.Name != "anthropic/claude-sonnet-4.5" {
		t.Fatalf("model %+v", m)
	}
	if !strings.HasPrefix(f.key("openrouter"), "sk-or-v1-PROBE-") || strings.Contains(errOut, f.key("openrouter")) {
		t.Fatal("the key is not kept, or was printed")
	}
	t.Setenv(modelkey.EnvNoKeyring, "1")
	n := len(f.or.Authorizations)
	if _, _, err := runCLI(t, strings.NewReader(""), "model-signins", "add", "openrouter"); err == nil || !strings.Contains(err.Error(), modelkey.EnvNoKeyring) {
		t.Fatalf("no keychain: %v", err)
	}
	if len(f.or.Authorizations) != n {
		t.Fatal("the browser was sent to a connect whose key could not be kept")
	}
}

// Poe needs Agentry's client id, which this build carries none of until M8.
func TestModelSignInsConnectPoeNamesTheMissingClient(t *testing.T) {
	newConnectFixture(t)
	t.Setenv(mcpauth.PoeClientIDEnv, "")
	if _, _, err := runCLI(t, strings.NewReader(""), "model-signins", "add", "poe"); err == nil || !strings.Contains(err.Error(), "carries no client id") {
		t.Fatalf("err %v", err)
	}
}
