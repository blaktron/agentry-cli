package persistent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

const testGoogleClient = `{"installed":{"client_id":"123-abc.apps.googleusercontent.com","client_secret":"test-client-secret-value","redirect_uris":["http://localhost"]}}`

// TestAGoogleClientIsADesktopAppsClient: an installed client naming its id
// and secret passes; a web client, a service account, anything else and
// an oversized file are refused, each saying why.
func TestAGoogleClientIsADesktopAppsClient(t *testing.T) {
	if err := CheckGoogleClient([]byte(testGoogleClient)); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ file, want string }{
		"a web client":      {`{"web":{"client_id":"123-abc.apps.googleusercontent.com","client_secret":"x"}}`, "Desktop app"},
		"a service account": {`{"type":"service_account","private_key":"x"}`, "service account"},
		"no client":         {`{"other":{}}`, "no \"installed\" client"},
		"not a client id":   {`{"installed":{"client_id":"abc","client_secret":"x"}}`, "names no client id"},
		"no secret":         {`{"installed":{"client_id":"123-abc.apps.googleusercontent.com"}}`, "names no client id and secret"},
		"not JSON":          {`client_id=x`, "not JSON"},
		"too big":           {`{"installed":{"client_id":"` + strings.Repeat("1", 70<<10) + `"}}`, "64 KiB"},
	} {
		if err := CheckGoogleClient([]byte(tc.file)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// TestGogsAccountListNamesTheSignIn: the sign-in is recorded only as gog's
// list names it, whatever the letters' case; a list without it is an error.
func TestGogsAccountListNamesTheSignIn(t *testing.T) {
	if a, err := gogAccount(`{"accounts":[{"email":"Person@Example.com"}]}`, "person@example.com"); err != nil || a != "Person@Example.com" {
		t.Fatalf("%q, %v", a, err)
	}
	if _, err := gogAccount(`{"accounts":[]}`, "person@example.com"); err == nil {
		t.Fatal("an empty list named the sign-in")
	}
}

// googleRig is the fake engine and keychain of a Google sign-in, gog's
// answers played by argv.
type googleRig struct {
	g    Google
	f    *dockerx.Fake
	keys fakeSecrets
	runs []dockerx.RunSpec
	in   []string
}

func newGoogleRig(t *testing.T) *googleRig {
	t.Helper()
	withImageServers(t)
	a, err := Lookup("assistant")
	if err != nil {
		t.Fatal(err)
	}
	r := &googleRig{keys: fakeSecrets{}}
	r.f = &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"image-oneshot": {Handler: func(_ context.Context, s dockerx.RunSpec, stdin io.Reader, stdout, _ io.Writer) (int, error) {
		r.runs = append(r.runs, s)
		if stdin != nil {
			b, _ := io.ReadAll(stdin)
			r.in = append(r.in, string(b))
		}
		switch {
		case slices.Contains(s.Argv, "credentials"):
			_, _ = io.WriteString(stdout, `{"saved":true,"client_secret_in_keyring":true}`)
		case slices.Contains(s.Argv, "--step") && s.Argv[slices.Index(s.Argv, "--step")+1] == "1":
			redirect := s.Argv[slices.Index(s.Argv, "--redirect-uri")+1]
			_, _ = fmt.Fprintf(stdout, `{"auth_url":"https://accounts.google.com/o/oauth2/auth?redirect_uri=%s&state=s1"}`, url.QueryEscape(redirect))
		case slices.Contains(s.Argv, "list"):
			_, _ = io.WriteString(stdout, `{"accounts":[{"email":"person@example.com"}]}`)
		}
		return 0, nil
	}}}}
	pol := policy.Default()
	r.g = Google{Docker: r.f, Home: shortHome(t), Agent: a, Policy: pol, RunnerImage: "agentry-runner:test", Keys: r.keys}
	return r
}

// browseBack plays the person's consent: Google sends the browser back to
// the redirect the sign-in's address names, with a code.
func browseBack(t *testing.T) func(string) error {
	return func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		resp, err := http.Get(u.Query().Get("redirect_uri") + "?state=s1&code=test-code") //nolint:gosec,noctx // the test's own listener
		if err != nil {
			t.Error(err)
			return err
		}
		_ = resp.Body.Close()
		return nil
	}
}

// TestGoogleWorkspaceKeepsTheClientInGog: the client file goes to gog on
// stdin in a one-shot of its image with no network on the server's own
// volume, its keyring password made in the keychain and given by name; the
// configuration records that a client was set and nothing of it.
func TestGoogleWorkspaceKeepsTheClientInGog(t *testing.T) {
	r := newGoogleRig(t)
	if err := r.g.SetClient(context.Background(), []byte(testGoogleClient)); err != nil {
		t.Fatal(err)
	}
	password := r.keys[ImageKeyringAccount("assistant", "gog")]
	if len(password) < 32 {
		t.Fatalf("the keyring password %q", password)
	}
	if len(r.runs) != 1 || len(r.in) != 1 || r.in[0] != testGoogleClient {
		t.Fatalf("runs %d, stdin %q", len(r.runs), r.in)
	}
	s := r.runs[0]
	vol := ImageVolumeIn(r.g.Home, "assistant", "gog")
	if s.Image != policy.DefaultGogImage || s.Network != "none" || !slices.Equal(s.Argv[:4], []string{"auth", "credentials", "set", "-"}) ||
		len(s.Mounts) != 1 || s.Mounts[0].Source != vol || s.Mounts[0].Target != assistant.GogHome || s.Env[assistant.GogKeyringSecret] != password || !s.ReadOnly {
		t.Fatalf("the one-shot: %+v", s)
	}
	if slices.ContainsFunc(s.Argv, func(a string) bool { return strings.Contains(a, "client-secret") || strings.Contains(a, password) }) {
		t.Fatal("the client or the password is on the command line")
	}
	c, err := LoadConfig(r.g.Home, "assistant")
	if err != nil || c.SignIns["gog"].Client.IsZero() || c.SignIns["gog"].Account != "" {
		t.Fatalf("the configuration: %+v, %v", c.SignIns, err)
	}
	if !slices.ContainsFunc(r.f.Calls, func(c dockerx.FakeCall) bool { return c.Op == "VolumeCreate" && c.Arg == vol }) {
		t.Error("the server's volume was not made")
	}
}

// TestGoogleWorkspaceSignsInInTwoSteps: the sign-in's address is made with
// no network, the browser's redirect is caught on 127.0.0.1, the code is
// exchanged through a proxy reaching oauth2.googleapis.com alone, and the
// account is recorded as gog lists it, read-only with changes off and
// with them when on.
func TestGoogleWorkspaceSignsInInTwoSteps(t *testing.T) {
	r := newGoogleRig(t)
	if _, err := r.g.SignIn(context.Background(), "person@example.com", browseBack(t)); err == nil || !strings.Contains(err.Error(), "--google-client") {
		t.Fatalf("a sign-in with no client: %v", err)
	}
	if err := r.g.SetClient(context.Background(), []byte(testGoogleClient)); err != nil {
		t.Fatal(err)
	}
	r.runs = nil
	account, err := r.g.SignIn(context.Background(), "Person@Example.com", browseBack(t))
	if err != nil || account != "person@example.com" {
		t.Fatalf("%q, %v", account, err)
	}
	if len(r.runs) != 3 {
		t.Fatalf("%d one-shots, want step 1, step 2 and the list", len(r.runs))
	}
	step1, step2, list := r.runs[0], r.runs[1], r.runs[2]
	if step1.Network != "none" || list.Network != "none" || !slices.Contains(step1.Argv, "--readonly") {
		t.Fatalf("step 1 %+v, list %+v", step1, list)
	}
	back := step2.Argv[slices.Index(step2.Argv, "--auth-url")+1]
	// gog matches the second step to the first by the redirect and the
	// consent flag, so both are given again.
	if r1, r2 := step1.Argv[slices.Index(step1.Argv, "--redirect-uri")+1], step2.Argv[slices.Index(step2.Argv, "--redirect-uri")+1]; r1 != r2 || !slices.Contains(step2.Argv, "--force-consent") {
		t.Fatalf("step 2 does not repeat step 1's redirect (%q, %q) and consent", r1, r2)
	}
	if !strings.HasPrefix(back, "http://127.0.0.1:") || !strings.Contains(back, "code=test-code") || step2.Network == "none" ||
		step2.Env["HTTPS_PROXY"] == "" || !slices.Contains(step2.Argv, "--readonly") {
		t.Fatalf("step 2: %+v", step2)
	}
	proxy := slices.IndexFunc(r.f.Calls, func(c dockerx.FakeCall) bool {
		return c.Op == "RunDetached" && strings.Contains(c.Arg, "oneshot-proxy")
	})
	if proxy < 0 || !slices.Contains(r.f.Calls[proxy].Run.Argv, "oauth2.googleapis.com:443") {
		t.Fatalf("the exchange's proxy: %+v", r.f.Calls)
	}
	c, err := LoadConfig(r.g.Home, "assistant")
	if err != nil || c.SignIns["gog"].Account != "person@example.com" || len(c.SignIns["gog"].Granted) != 0 {
		t.Fatalf("after a read-only sign-in: %+v, %v", c.SignIns, err)
	}

	c.Switches = map[string]bool{"server:gog:writes": true}
	if err := SaveConfig(r.g.Home, "assistant", c); err != nil {
		t.Fatal(err)
	}
	r.runs = nil
	if _, err := r.g.SignIn(context.Background(), "person@example.com", browseBack(t)); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(r.runs[0].Argv, "--readonly") {
		t.Fatal("a sign-in with changes on asked for read-only scopes")
	}
	if c, _ := LoadConfig(r.g.Home, "assistant"); !slices.Equal(c.SignIns["gog"].Granted, []string{"writes"}) {
		t.Fatalf("after a sign-in with changes: %+v", c.SignIns)
	}
}

// TestGoogleSignInHoldsTheAgentInItsCallersName: while the sign-in waits
// for the browser, the agent's lock names the client that called it, and
// the CLI when none is named.
func TestGoogleSignInHoldsTheAgentInItsCallersName(t *testing.T) {
	for _, tc := range []struct{ client, want string }{{"", ClientCLI}, {ClientCLI, ClientCLI}, {ClientDesktop, ClientDesktop}} {
		r := newGoogleRig(t)
		r.g.Client = tc.client
		if err := r.g.SetClient(context.Background(), []byte(testGoogleClient)); err != nil {
			t.Fatal(err)
		}
		back := browseBack(t)
		var held *Holder
		if _, err := r.g.SignIn(context.Background(), "person@example.com", func(u string) error {
			_, held = Held(r.g.Home, "assistant")
			return back(u)
		}); err != nil {
			t.Fatal(err)
		}
		if held == nil || held.Client != tc.want {
			t.Errorf("client %q: the lock's holder %+v, want %q", tc.client, held, tc.want)
		}
		if again, _ := Held(r.g.Home, "assistant"); again {
			t.Errorf("client %q: the lock was kept after the sign-in", tc.client)
		}
	}
}

// TestASignInAddressIsNeverAnOption: an address whose first character is
// "-" would read as one of gog's options, so it is refused before gog runs.
func TestASignInAddressIsNeverAnOption(t *testing.T) {
	r := newGoogleRig(t)
	if err := r.g.SetClient(context.Background(), []byte(testGoogleClient)); err != nil {
		t.Fatal(err)
	}
	r.runs = nil
	for _, email := range []string{"--x=y@a.bc", "-person@example.com", " -person@example.com"} {
		if _, err := r.g.SignIn(context.Background(), email, browseBack(t)); err == nil || !strings.Contains(err.Error(), "not an email address") {
			t.Errorf("%q: %v", email, err)
		}
	}
	if len(r.runs) != 0 {
		t.Fatalf("gog ran %d times for an address that reads as an option", len(r.runs))
	}
	for _, email := range []string{"a-b@example.com", "person-@example.com"} {
		if !googleEmailRE.MatchString(email) {
			t.Errorf("%q was refused", email)
		}
	}
}

// TestALostKeyringPasswordStartsTheSignInAgain: a volume whose password the
// keychain no longer has cannot be opened, so it is made again.
func TestALostKeyringPasswordStartsTheSignInAgain(t *testing.T) {
	r := newGoogleRig(t)
	vol := ImageVolumeIn(r.g.Home, "assistant", "gog")
	r.f.Volumes = append(r.f.Volumes, dockerx.Labeled{Name: vol, Labels: map[string]string{"agentry.persistent.home": HomeID(r.g.Home)}})
	if err := r.g.SetClient(context.Background(), []byte(testGoogleClient)); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(r.f.Calls, func(c dockerx.FakeCall) bool { return c.Op == "VolumeRemove" && c.Arg == vol }) {
		t.Fatal("a volume that could not be opened was kept")
	}
}
