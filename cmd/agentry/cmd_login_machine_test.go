package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zalando/go-keyring"
)

func machineBrowser(t *testing.T) {
	t.Helper()
	prev := openBrowser
	openBrowser = func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		callback := fmt.Sprintf("http://127.0.0.1:%s/callback?state=%s&code=agd_browser-code", u.Query().Get("port"), u.Query().Get("state"))
		go func() {
			resp, err := http.Get(callback) //nolint:gosec // test loopback listener
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
	t.Cleanup(func() { openBrowser = prev })
}

func TestMachineLoginAndLogoutUseKeyring(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envNoKeyring, "")
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	machineBrowser(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	if err := keyring.Set(keyringService, "other.example", "agy_other"); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, strings.NewReader(""), "login", "--json", "--timeout", "1s", "--registry", fr.url())
	if err != nil || !strings.Contains(out, `"storage":"keyring"`) {
		t.Fatalf("keyring login: %s, %v", out, err)
	}
	if mustCredential(t, fr.url()) != "agy_browsertoken" {
		t.Fatal("keyring login was not stored")
	}
	path, _ := configPath()
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "agy_browsertoken") {
		t.Fatal("keyring credential also written to config")
	}
	for i := 0; i < 2; i++ {
		if _, _, err := runCLI(t, nil, "logout", "--json", "--registry", fr.url()); err != nil {
			t.Fatal(err)
		}
	}
	host, _ := registryHost(fr.url())
	if _, err := keyring.Get(keyringService, host); err != keyring.ErrNotFound || mustCredential(t, fr.url()) != "" {
		t.Fatal("selected keyring login still available")
	}
	if token, err := keyring.Get(keyringService, "other.example"); token != "agy_other" || err != nil {
		t.Fatal("another keyring login changed")
	}
	// A new login retains the pins established before sign-out.
	out, _, err = runCLI(t, strings.NewReader(""), "login", "--json", "--timeout", "1s", "--registry", fr.url())
	if err != nil || !strings.Contains(out, `"signingKeys":"unchanged"`) {
		t.Fatalf("re-login after sign-out: %s, %v", out, err)
	}
	t.Setenv(envToken, "agy_environment")
	out, logs, err := runCLI(t, nil, "logout", "--json", "--registry", fr.url())
	if err != nil || !strings.Contains(out, `"environmentCredential":true`) || strings.Contains(out+logs, "agy_environment") {
		t.Fatalf("environment override result: %s, %v", out, err)
	}
	if mustCredential(t, fr.url()) != "agy_environment" {
		t.Fatal("child process changed environment credential")
	}
}

func TestMachineLoginDoesNotFollowAccountRedirect(t *testing.T) {
	isolateEnv(t)
	machineBrowser(t)
	fr := newFakeRegistry(t)
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		fr.srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer other.Close()
	selected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/user" {
			http.Redirect(w, r, other.URL+"/api/v1/user", http.StatusFound)
			return
		}
		fr.srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer selected.Close()
	out, logs, err := runCLI(t, strings.NewReader(""), "login", "--json", "--timeout", "1s", "--registry", selected.URL)
	if err == nil || !strings.Contains(out, "verification_or_storage_failed") {
		t.Fatalf("redirect result = %s, %v", out, err)
	}
	if reached.Load() != 0 {
		t.Fatal("credential-bearing account request followed another registry's redirect")
	}
	if mustCredential(t, selected.URL) != "" {
		t.Fatal("unverified redirected identity stored")
	}
	if strings.Contains(out+logs, "agy_browsertoken") {
		t.Fatal("token exposed")
	}
}

func TestMachineLoginRedactsRegistryFailuresAndHandles(t *testing.T) {
	for _, reject := range []bool{true, false} {
		t.Run(fmt.Sprint(reject), func(t *testing.T) {
			isolateEnv(t)
			machineBrowser(t)
			fr := newFakeRegistry(t)
			fr.userHandle = "agy_browsertoken"
			selected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if reject && r.URL.Path == "/api/v1/user" {
					http.Error(w, "Bearer agy_browsertoken", http.StatusUnauthorized)
					return
				}
				fr.srv.Config.Handler.ServeHTTP(w, r)
			}))
			defer selected.Close()
			out, logs, err := runCLI(t, strings.NewReader(""), "login", "--json", "--timeout", "1s", "--registry", selected.URL)
			if reject != (err != nil) {
				t.Fatalf("unexpected status: %v", err)
			}
			printed := out + logs
			if err != nil {
				printed += err.Error()
			}
			if strings.Contains(printed, "agy_browsertoken") {
				t.Fatal("credential echoed by registry exposed")
			}
			if reject && mustCredential(t, selected.URL) != "" {
				t.Fatal("rejected token stored")
			}
		})
	}
}

func TestMachineLoginRefusesCredentialOverridesAndSecretURLs(t *testing.T) {
	isolateEnv(t)
	t.Setenv("AGENTRY_NO_BROWSER", "1")
	for _, reg := range []string{"https://agy_private@example.test", "https://example.test?token=agy_private"} {
		out, logs, err := runCLI(t, nil, "login", "--json", "--registry", reg)
		if err == nil || strings.Contains(out+logs+err.Error(), "agy_private") {
			t.Fatal("secret registry URL not refused with redacted output")
		}
	}
	t.Setenv(envToken, "agy_override")
	out, logs, err := runCLI(t, nil, "login", "--json", "--timeout", "10ms", "--registry", "https://example.test")
	if err == nil || !strings.Contains(out, "environment_credential") || strings.Contains(out+logs+err.Error(), "agy_override") {
		t.Fatal("credential override not refused")
	}
}

func TestLogoutRefusesSharedKeyringIdentityAndPreservesConfig(t *testing.T) {
	isolateEnv(t)
	const first, second = "https://example.test/first", "https://example.test/second"
	if err := saveCLIConfig(cliConfig{DefaultRegistry: first, Registries: []registryEntry{{URL: first, TokenRef: tokenRefKeyring}, {URL: second, TokenRef: tokenRefKeyring}}}); err != nil {
		t.Fatal(err)
	}
	path, _ := configPath()
	before, _ := os.ReadFile(path)
	out, _, err := runCLI(t, nil, "logout", "--json", "--registry", first)
	if err == nil || !strings.Contains(out, "shared_keyring_identity") {
		t.Fatalf("ambiguous sign-out = %s, %v", out, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("ambiguous sign-out changed identities")
	}
}

func TestMachineLoginDeclineCancelAndTimeoutStoreNothing(t *testing.T) {
	for _, tc := range []struct{ input, timeout, status string }{
		{"{\"action\":\"decline\"}\n", "1s", "declined"},
		{"{\"action\":\"cancel\"}\n", "1s", "cancelled"},
		{"agy_never-print-this\n", "1s", "failed"},
		{"", "10ms", "timed_out"},
	} {
		t.Run(tc.status+tc.input, func(t *testing.T) {
			isolateEnv(t)
			t.Setenv("AGENTRY_NO_BROWSER", "1")
			fr := newFakeRegistry(t)
			out, logs, err := runCLI(t, strings.NewReader(tc.input), "login", "--json", "--timeout", tc.timeout, "--registry", fr.url())
			if err == nil {
				t.Fatal("unsuccessful login returned success")
			}
			d := json.NewDecoder(strings.NewReader(out))
			var first, final map[string]any
			if d.Decode(&first) != nil || d.Decode(&final) != nil || first["status"] != "pending" || final["status"] != tc.status {
				t.Fatalf("lifecycle = %s", out)
			}
			if strings.Contains(out+logs+err.Error(), "agy_never-print-this") {
				t.Fatal("stdin value exposed")
			}
			if mustCredential(t, fr.url()) != "" {
				t.Fatal("unsuccessful login stored a credential")
			}
		})
	}
}

func TestLogoutClearsOnlySelectedRegistry(t *testing.T) {
	isolateEnv(t)
	first, second := newFakeRegistry(t), newFakeRegistry(t)
	for _, fr := range []*fakeRegistry{first, second} {
		if _, _, err := runCLI(t, strings.NewReader("agy_original\n"), "login", "--paste", "--registry", fr.url()); err != nil {
			t.Fatal(err)
		}
	}
	out, _, err := runCLI(t, nil, "logout", "--json", "--registry", first.url())
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if json.Unmarshal([]byte(out), &result) != nil || result["status"] != "signed_out" || result["registry"] != first.url() {
		t.Fatalf("logout result = %s", out)
	}
	if mustCredential(t, first.url()) != "" || mustCredential(t, second.url()) != "agy_original" {
		t.Fatal("logout did not isolate selected identity")
	}
	cfg, err := loadCLIConfig()
	if err != nil || cfg.DefaultRegistry != first.url() {
		t.Fatal("logout changed default registry")
	}
	if _, _, err := runCLI(t, nil, "whoami", "--json", "--registry", second.url()); err != nil {
		t.Fatal("other identity no longer works")
	}
	if _, _, err := runCLI(t, nil, "whoami", "--json", "--registry", first.url()); err == nil {
		t.Fatal("selected identity still authenticated")
	}
}

func TestMachineLoginReportsLifecycleWithoutCredential(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	machineBrowser(t)
	out, logs, err := runCLI(t, strings.NewReader(""), "login", "--json", "--timeout", "1s", "--registry", fr.url())
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	d := json.NewDecoder(strings.NewReader(out))
	var pending, result map[string]any
	if err := d.Decode(&pending); err != nil {
		t.Fatal(err)
	}
	if err := d.Decode(&result); err != nil {
		t.Fatal(err)
	}
	if pending["status"] != "pending" || !strings.HasPrefix(fmt.Sprint(pending["authorizeUrl"]), fr.url()+"/ui/authorize?") {
		t.Fatalf("pending event = %v", pending)
	}
	if result["status"] != "signed_in" || result["registry"] != fr.url() || result["handle"] != fr.userHandle || result["storage"] != "file" {
		t.Fatalf("completion = %v", result)
	}
	if strings.Contains(out+logs, "agy_browsertoken") || strings.Contains(out+logs, "agd_browser-code") {
		t.Fatal("machine login exposed a credential or callback code")
	}
	if mustCredential(t, fr.url()) != "agy_browsertoken" {
		t.Fatal("verified credential not stored for selected registry")
	}
	if result["signingKeys"] != "pinned" {
		t.Fatalf("pinning = %v", result["signingKeys"])
	}
}
