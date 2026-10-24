package persistent

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

const testEntraClient = "11111111-2222-3333-4444-555555555555"

// TestTheMicrosoftServerHoldsToItsOffer: the Assistant's Microsoft 365
// server passes the image server's rules at the CLI's pinned image, reaches
// Microsoft's sign-in and Graph hosts alone, has one secret slot, and is
// given the read list with --read-only, the changes list with changes, and
// the sending list with sending too, which only it holds the send tools.
func TestTheMicrosoftServerHoldsToItsOffer(t *testing.T) {
	servers := assistant.ImageServers(policy.DefaultGogImage, policy.DefaultMS365Image)
	if err := offer.CheckImageServers(servers); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(servers, func(s offer.ImageServer) bool { return s.Key == assistant.MS365Key })
	if i < 0 {
		t.Fatal("no ms365 server")
	}
	s := servers[i]
	if !slices.Equal(s.Egress, []string{"login.microsoftonline.com", "graph.microsoft.com"}) || !slices.Equal(s.Secrets, []string{assistant.MS365CacheKey}) ||
		s.VolumeTarget != "/persist/ms365" || s.ClientEnv != "MS365_MCP_CLIENT_ID" || s.TenantEnv != "MS365_MCP_TENANT_ID" {
		t.Fatalf("the server: %+v", s)
	}
	tools := func(args []string) []string {
		i := slices.Index(args, "--enabled-tools")
		if i < 0 {
			t.Fatalf("no --enabled-tools in %v", args)
		}
		re := args[i+1]
		if !strings.HasPrefix(re, "^(") || !strings.HasSuffix(re, ")$") {
			t.Fatalf("the allow-list is not anchored: %q", re)
		}
		return strings.Split(strings.TrimSuffix(strings.TrimPrefix(re, "^("), ")$"), "|")
	}
	on := func(keys ...string) func(string) bool { return func(k string) bool { return slices.Contains(keys, k) } }
	read, chg, snd := s.Args(on()), s.Args(on("writes")), s.Args(on("writes", "send"))
	if !slices.Contains(read, "--read-only") || slices.Contains(chg, "--read-only") || slices.Contains(snd, "--read-only") {
		t.Errorf("--read-only: %v / %v / %v", read[:1], chg[:1], snd[:1])
	}
	if n := [3]int{len(tools(read)), len(tools(chg)), len(tools(snd))}; n != [3]int{29, 55, 61} {
		t.Errorf("tool counts %v, want 29, 55, 61", n)
	}
	for _, send := range []string{"send-mail", "send-draft-message", "reply-mail-message", "forward-mail-message", "forward-calendar-event"} {
		if slices.Contains(tools(chg), send) || !slices.Contains(tools(snd), send) {
			t.Errorf("%s: with changes alone %v, with sending %v", send, slices.Contains(tools(chg), send), slices.Contains(tools(snd), send))
		}
	}
	for _, never := range []string{"login", "logout", "download-bytes", "download-bytes-to-file", "create-mail-rule", "update-mailbox-settings", "share-drive-item", "upload-file-content"} {
		if slices.Contains(tools(snd), never) {
			t.Errorf("%s is given", never)
		}
	}
	// Sending without changes is not a combination: the server is read only.
	if !slices.Equal(s.Args(on("send")), read) {
		t.Errorf("sending alone: %v", s.Args(on("send")))
	}
}

// TestProfilesCoverEveryCombination: a server with profiles must have one
// for each combination its switches allow, and none for another.
func TestProfilesCoverEveryCombination(t *testing.T) {
	base := offer.ImageServer{Key: "x", Name: "X", Image: "r@sha256:" + strings.Repeat("a", 64), Egress: []string{"example.com"}, VolumeTarget: "/v", SignIn: "sign in",
		Switches: []offer.ImageSwitch{{Key: "a", Name: "A", Line: "a"}, {Key: "b", Name: "B", Line: "b", Requires: "a"}},
		Profiles: map[string][]string{"": {"-0"}, "a": {"-a"}, "a+b": {"-ab"}}}
	if err := base.Check(); err != nil {
		t.Fatal(err)
	}
	missing := base
	missing.Profiles = map[string][]string{"": {"-0"}, "a": {"-a"}}
	if err := missing.Check(); err == nil || !strings.Contains(err.Error(), `"a+b"`) {
		t.Errorf("a missing profile: %v", err)
	}
	extra := base
	extra.Profiles = map[string][]string{"": {"-0"}, "a": {"-a"}, "a+b": {"-ab"}, "b": {"-b"}}
	if err := extra.Check(); err == nil || !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("a profile no combination reaches: %v", err)
	}
}

// TestAnEntraClientIsThePersonsOwn: an application id passes; Softeria's
// own clients, and anything not an id, are refused.
func TestAnEntraClientIsThePersonsOwn(t *testing.T) {
	if err := CheckMicrosoftClient(testEntraClient); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"084a3e9f-a9f4-43f7-89f9-d229cf97853e", "F3E61A6E-BC26-4281-8588-2C7359A02141", "my-app", "", "11111111-2222-3333-4444-55555555555"} {
		if err := CheckMicrosoftClient(bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}

// TestMicrosoftsRealmAnswerChoosesTheTenant: an address Microsoft federates
// to its consumer sign-in, or that no organization holds, signs in on
// consumers; one an organization holds, on organizations.
func TestMicrosoftsRealmAnswerChoosesTheTenant(t *testing.T) {
	for _, tc := range []struct{ ns, domain, want string }{
		{"Federated", "live.com", TenantConsumers},
		{"Unknown", "", TenantConsumers},
		{"Managed", "contoso.com", TenantOrganizations},
		{"Federated", "contoso.com", TenantOrganizations},
	} {
		if got := realmTenant(tc.ns, tc.domain); got != tc.want {
			t.Errorf("%s %s: %s, want %s", tc.ns, tc.domain, got, tc.want)
		}
	}
}

// TestOnlyMicrosoftsDeviceLoginPageIsOpened: the device-code sentence is
// relayed and its page opened only when it is Microsoft's own.
func TestOnlyMicrosoftsDeviceLoginPageIsOpened(t *testing.T) {
	for page, ok := range map[string]bool{
		"https://microsoft.com/devicelogin":                          true,
		"https://login.microsoftonline.com/common/oauth2/deviceauth": true,
		"http://microsoft.com/devicelogin":                           false,
		"https://microsoft.com.example.net/devicelogin":              false,
		"https://user@microsoft.com/devicelogin":                     false,
		"https://microsoft.com/other":                                false,
	} {
		if deviceLoginPage(page) != ok {
			t.Errorf("%s: %v", page, !ok)
		}
	}
	var said []string
	var opened []string
	r := &deviceCodeRelay{say: func(f string, a ...any) { said = append(said, fmt.Sprintf(f, a...)) }, browse: func(u string) error { opened = append(opened, u); return nil }}
	_, _ = io.WriteString(r, "\nTo sign in, use a web browser to open the page https://evil.example/devicelogin and enter the code AAAA1111 to authenticate.\n")
	if r.shown() || len(opened) != 0 {
		t.Fatalf("another page was relayed: %v %v", said, opened)
	}
	_, _ = io.WriteString(r, "\nTo sign in, use a web browser to open the page ")
	_, _ = io.WriteString(r, "https://microsoft.com/devicelogin and enter the code B2C3D4E5F to authenticate.\n")
	if !r.shown() || !slices.Equal(opened, []string{"https://microsoft.com/devicelogin"}) || !strings.Contains(strings.Join(said, "\n"), "B2C3D4E5F") {
		t.Fatalf("relayed %v, opened %v", said, opened)
	}
}

// microsoftRig is the fake engine and keychain of a Microsoft sign-in, the
// server's answers played by argv.
type microsoftRig struct {
	m        Microsoft
	f        *dockerx.Fake
	keys     fakeSecrets
	runs     []dockerx.RunSpec
	accounts string
	loginOut string
	loginRC  int
}

func newMicrosoftRig(t *testing.T) *microsoftRig {
	t.Helper()
	withImageServers(t)
	a, err := Lookup("assistant")
	if err != nil {
		t.Fatal(err)
	}
	r := &microsoftRig{keys: fakeSecrets{}, accounts: `[{"id":"h1","username":"Person@Outlook.com","name":"Person","selected":true}]`,
		loginOut: "\nTo sign in, use a web browser to open the page https://microsoft.com/devicelogin and enter the code ABCD12345 to authenticate.\n\n{\"success\":false}\n"}
	r.f = &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"image-oneshot": {Handler: func(_ context.Context, s dockerx.RunSpec, _ io.Reader, stdout, stderr io.Writer) (int, error) {
		r.runs = append(r.runs, s)
		switch {
		case slices.Contains(s.Argv, "--login"):
			_, _ = io.WriteString(stdout, r.loginOut)
			if r.loginRC != 0 {
				_, _ = io.WriteString(stderr, "Error in device code flow: AADSTS50199: device code flow is blocked by Conditional Access\n")
			}
			return r.loginRC, nil
		case slices.Contains(s.Argv, "--list-accounts"):
			_, _ = io.WriteString(stdout, r.accounts)
		}
		return 0, nil
	}}}}
	old := RealmTenant
	RealmTenant = func(context.Context, string) (string, error) { return TenantConsumers, nil }
	t.Cleanup(func() { RealmTenant = old })
	r.m = Microsoft{Docker: r.f, Home: shortHome(t), Agent: a, Policy: policy.Default(), RunnerImage: "agentry-runner:test", Keys: r.keys}
	return r
}

// TestMicrosoftSignsInByDeviceCode: the client is recorded and Microsoft
// enabled; the sign-in runs --login with the read-only allow-list, reaching
// Microsoft's sign-in host alone through the proxy, given the client, the
// tenant Microsoft named and the cache key made in the keychain by name;
// the code is relayed and only Microsoft's page opened; the account is
// recorded as the server lists it.
func TestMicrosoftSignsInByDeviceCode(t *testing.T) {
	r := newMicrosoftRig(t)
	if _, err := r.m.SignIn(context.Background(), "person@outlook.com", nil); err == nil || !strings.Contains(err.Error(), "--microsoft-client") {
		t.Fatalf("a sign-in with no client: %v", err)
	}
	if err := r.m.SetClient("084a3e9f-a9f4-43f7-89f9-d229cf97853e", ""); err == nil {
		t.Fatal("Softeria's client was taken")
	}
	if err := r.m.SetClient(testEntraClient, "nowhere"); err == nil {
		t.Fatal("a tenant that is none was taken")
	}
	if err := r.m.SetClient(testEntraClient, ""); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(r.m.Home, "assistant")
	if err != nil || c.SignIns["ms365"].ClientID != testEntraClient || !slices.Contains(c.MailProviders, "microsoft") {
		t.Fatalf("after the client: %+v, %v", c, err)
	}
	var opened []string
	var told strings.Builder
	r.m.Say = &told
	account, err := r.m.SignIn(context.Background(), "Person@Outlook.com", func(u string) error { opened = append(opened, u); return nil })
	if err != nil || account != "person@outlook.com" {
		t.Fatalf("%q, %v", account, err)
	}
	if !slices.Equal(opened, []string{"https://microsoft.com/devicelogin"}) || !strings.Contains(told.String(), "ABCD12345") {
		t.Errorf("opened %v, told %q", opened, told.String())
	}
	login := r.runs[0]
	key := r.keys[ImageKeyringAccount("assistant", "ms365")]
	if b, err := base64.StdEncoding.DecodeString(key); err != nil || len(b) != 32 {
		t.Fatalf("the cache key %q", key)
	}
	if login.Image != policy.DefaultMS365Image || login.Argv[0] != "--login" || !slices.Contains(login.Argv, "--read-only") ||
		login.Env[assistant.MS365CacheKey] != key || login.Env["MS365_MCP_CLIENT_ID"] != testEntraClient || login.Env["MS365_MCP_TENANT_ID"] != TenantConsumers ||
		login.Env["NODE_USE_ENV_PROXY"] != "1" || login.Env["HTTPS_PROXY"] == "" || login.Network == "none" {
		t.Fatalf("the sign-in's one-shot: %+v", login)
	}
	if slices.ContainsFunc(login.Argv, func(a string) bool { return strings.Contains(a, key) }) {
		t.Fatal("the cache key is on the command line")
	}
	list := r.runs[len(r.runs)-1]
	if list.Argv[0] != "--list-accounts" || list.Network != "none" {
		t.Fatalf("the account list's one-shot: %+v", list)
	}
	c, err = LoadConfig(r.m.Home, "assistant")
	if in := c.SignIns["ms365"]; err != nil || in.Account != "person@outlook.com" || in.Tenant != TenantConsumers || len(in.Granted) != 0 || in.At.IsZero() {
		t.Fatalf("the record: %+v, %v", c.SignIns, err)
	}
	if !slices.ContainsFunc(r.f.Calls, func(c dockerx.FakeCall) bool {
		return c.Op == "VolumeCreate" && c.Arg == ImageVolumeIn(r.m.Home, "assistant", "ms365")
	}) {
		t.Error("the server's volume was not made")
	}
}

// TestAMicrosoftSignInIsRecordedOnlyWhenItHolds: a sign-in whose account
// list names another account, or none, or that Microsoft refused, records
// nothing; an organization's block on device code is said as such.
func TestAMicrosoftSignInIsRecordedOnlyWhenItHolds(t *testing.T) {
	r := newMicrosoftRig(t)
	if err := r.m.SetClient(testEntraClient, TenantOrganizations); err != nil {
		t.Fatal(err)
	}
	r.accounts = `[{"username":"someone.else@contoso.com"}]`
	if _, err := r.m.SignIn(context.Background(), "person@contoso.com", nil); err == nil || !strings.Contains(err.Error(), "someone.else@contoso.com") {
		t.Errorf("another account: %v", err)
	}
	r.accounts = `[]`
	if _, err := r.m.SignIn(context.Background(), "person@contoso.com", nil); err == nil || !strings.Contains(err.Error(), "holds no account") {
		t.Errorf("no account: %v", err)
	}
	r.loginRC = 1
	if _, err := r.m.SignIn(context.Background(), "person@contoso.com", nil); err == nil || !strings.Contains(err.Error(), "does not allow signing in by device code") {
		t.Errorf("a Conditional Access refusal: %v", err)
	}
	r.loginRC, r.loginOut = 0, "{\"success\":false}\n"
	if _, err := r.m.SignIn(context.Background(), "person@contoso.com", nil); err == nil || !strings.Contains(err.Error(), "gave no sign-in code") {
		t.Errorf("no code: %v", err)
	}
	c, err := LoadConfig(r.m.Home, "assistant")
	if err != nil || c.SignIns["ms365"].Account != "" || c.SignIns["ms365"].Tenant != TenantOrganizations {
		t.Fatalf("a failed sign-in was recorded: %+v, %v", c.SignIns, err)
	}
}

// TestMicrosoftsSegmentIsGivenItsSignIn: a signed-in Microsoft 365 with
// changes on is given its client and tenant in its environment, the
// changes list without --read-only, the signoff as its argument, and the
// cache key in its secret slot; with Microsoft disabled it is given nothing.
func TestMicrosoftsSegmentIsGivenItsSignIn(t *testing.T) {
	withImageServers(t)
	home := shortHome(t)
	a, err := Lookup("assistant")
	if err != nil {
		t.Fatal(err)
	}
	c := Config{Enabled: true, MailProviders: []string{"microsoft"}, SignIns: map[string]ImageSignIn{"ms365": {
		ClientID: testEntraClient, Tenant: TenantOrganizations, Account: "person@contoso.com", At: time.Now(), Granted: []string{"writes"}, Signoff: "Sent by my assistant"}}}
	for _, id := range []string{"server:ms365", "server:ms365:writes"} {
		if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: id, On: true}, Consent: true}); err != nil {
			t.Fatal(err)
		}
	}
	keys := fakeSecrets{ImageKeyringAccount("assistant", "ms365"): "c2VhbGluZy1rZXk="}
	gr, _, done, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, keys)
	if err != nil {
		t.Fatal(err)
	}
	done()
	if len(gr.ImageServers) != 1 {
		t.Fatalf("given %+v", gr.ImageServers)
	}
	g := gr.ImageServers[0]
	if g.Key != "ms365" || g.Env["MS365_MCP_CLIENT_ID"] != testEntraClient || g.Env["MS365_MCP_TENANT_ID"] != TenantOrganizations ||
		!slices.Equal(g.Args, []string{"--message-signoff-suffix", "Sent by my assistant"}) || !slices.Equal(g.Switches, []string{"writes"}) ||
		gr.ServerSecrets[assistant.MS365CacheKey] != "c2VhbGluZy1rZXk=" {
		t.Fatalf("the grant: %+v, slots %v", g, gr.ServerSecrets)
	}
	c.MailProviders = nil
	gr, _, done, err = Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, keys)
	if err != nil {
		t.Fatal(err)
	}
	done()
	if len(gr.ImageServers) != 0 || gr.ServerSecrets[assistant.MS365CacheKey] != "" {
		t.Errorf("Microsoft disabled, given %+v", gr.ImageServers)
	}
}

// TestASignoffIsOneShortLine: a signoff is recorded and cleared; one with a
// line break or past its bound is refused.
func TestASignoffIsOneShortLine(t *testing.T) {
	r := newMicrosoftRig(t)
	if err := r.m.SetSignoff("Sent by my assistant"); err != nil {
		t.Fatal(err)
	}
	if c, _ := LoadConfig(r.m.Home, "assistant"); c.SignIns["ms365"].Signoff != "Sent by my assistant" {
		t.Fatalf("%+v", c.SignIns)
	}
	for _, bad := range []string{"two\nlines", strings.Repeat("x", 201)} {
		if err := r.m.SetSignoff(bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	if err := r.m.SetSignoff(""); err != nil {
		t.Fatal(err)
	}
	if c, _ := LoadConfig(r.m.Home, "assistant"); c.SignIns["ms365"].Signoff != "" {
		t.Fatalf("not cleared: %+v", c.SignIns)
	}
}

// TestTheSegmentPlanRunsMicrosoft365: a signed-in Microsoft 365 reaches the
// segment's plan as one image server: the pinned image, its allow-list, its
// client and tenant, its volume, its secret slot and its two hosts in the
// gateway's catalogue, under the name the gateway is asked to start.
func TestTheSegmentPlanRunsMicrosoft365(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withImageServers(t)
	home := shortHome(t)
	a, err := Lookup("assistant")
	if err != nil {
		t.Fatal(err)
	}
	c := Config{Enabled: true, MailProviders: []string{"microsoft"}, SignIns: map[string]ImageSignIn{"ms365": {
		ClientID: testEntraClient, Tenant: TenantConsumers, Account: "person@outlook.com", At: time.Now()}}}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: "server:ms365", On: true}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	gr, _, done, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, fakeSecrets{ImageKeyringAccount("assistant", "ms365"): "c2VhbGluZy1rZXk="})
	if err != nil {
		t.Fatal(err)
	}
	done()
	sp, err := localrun.Segment{Builtin: a.Builtin, Home: home, Policy: policy.Default(), PolicySHA: "s", Build: "test", Grants: gr}.Plan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	images := spec.ImageServers(sp.Plan.MCPServers)
	if len(images) != 1 {
		t.Fatalf("the plan's image servers: %+v", images)
	}
	srv := images[0]
	if srv.Name != "mcp-image-ms365" || srv.Image != policy.DefaultMS365Image || srv.Command[0] != "--read-only" || srv.Command[1] != "--enabled-tools" ||
		srv.Env["MS365_MCP_CLIENT_ID"] != testEntraClient || srv.Env["MS365_MCP_TENANT_ID"] != TenantConsumers ||
		!slices.Equal(srv.Secrets, []string{assistant.MS365CacheKey}) || !slices.Equal(srv.Volumes, []string{ImageVolumeIn(home, "assistant", "ms365") + ":" + assistant.MS365Home}) {
		t.Fatalf("the plan's ms365 server: %+v", srv)
	}
	b, err := os.ReadFile(filepath.Join(sp.Dir, "gateway", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mcp-image-ms365:", "login.microsoftonline.com:443/tcp", "graph.microsoft.com:443/tcp", "--read-only"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the catalogue lacks %q", want)
		}
	}
	if strings.Contains(string(b), "c2VhbGluZy1rZXk=") {
		t.Error("the cache key is in the catalogue")
	}
}
