package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// providersList is `providers list --json`.
type providersList struct {
	Providers []providerOut   `json:"providers"`
	SignIns   []keptSignInOut `json:"signIns"`
}

func listProviders(t *testing.T) providersList {
	t.Helper()
	var got providersList
	out := cli(t, "--json", "providers", "list")
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("providers list --json: %v\n%s", err, out)
	}
	return got
}

func listedProvider(t *testing.T, l providersList, id string) providerOut {
	t.Helper()
	for _, p := range l.Providers {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no provider %s in %+v", id, l.Providers)
	return providerOut{}
}

func signInRow(l providersList, vendor string) (keptSignInOut, bool) {
	for _, s := range l.SignIns {
		if s.Vendor == vendor {
			return s, true
		}
	}
	return keptSignInOut{}, false
}

// providers list shows what model-signins list showed: the provider a
// sign-in pays gains its account, its app and when it was made and last
// used; every kept sign-in and connected account is listed with the
// provider it belongs to, the ones no provider uses included, each of
// those in the text with the command that removes it. No token is printed.
func TestProvidersListShowsEachKeptSignInAndTheOnesNoProviderUses(t *testing.T) {
	home, _ := modelsFixture(t)
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	s := modelsignin.Open(home, nil)
	if _, err := s.Put(modelsignin.Copilot, modelsignin.Record{Label: "octo-fixture", App: mcpauth.GitHubApp}, modelsignin.Grant{RefreshToken: "PROBE-copilot-refresh"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(modelsignin.ChatGPT, modelsignin.Record{Label: "person@example.com"}, modelsignin.Grant{RefreshToken: "PROBE-chatgpt-refresh"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(modelsignin.Poe, modelsignin.Record{Label: "poe-person", KeyExpiresAt: "2027-01-01T00:00:00Z"}, modelsignin.Grant{}); err != nil {
		t.Fatal(err)
	}
	cli(t, "providers", "add", "copilot")
	// openai is paid by a key, so the ChatGPT sign-in is kept for nothing.
	cli(t, "providers", "add", "openai")

	l := listProviders(t)
	cp := listedProvider(t, l, "copilot")
	if cp.Account != "octo-fixture" || cp.App != mcpauth.GitHubApp || cp.CreatedAt == "" || cp.LastUsedAt != "" {
		t.Fatalf("the copilot provider's sign-in: %+v", cp)
	}
	if oa := listedProvider(t, l, "openai"); oa.Account != "" || oa.CreatedAt != "" {
		t.Fatalf("a key provider gained a sign-in's details: %+v", oa)
	}
	if len(l.SignIns) != 3 {
		t.Fatalf("signIns = %+v", l.SignIns)
	}
	for vendor, want := range map[string]struct {
		provider string
		inUse    bool
	}{"copilot": {"copilot", true}, "chatgpt": {"openai", false}, "poe": {"poe", false}} {
		r, ok := signInRow(l, vendor)
		if !ok || r.Provider != want.provider || r.InUse != want.inUse || r.CreatedAt == "" || r.State == "" {
			t.Errorf("%s = %+v (%t), want provider %s, inUse %t", vendor, r, ok, want.provider, want.inUse)
		}
	}
	if r, _ := signInRow(l, "poe"); !r.Connected || r.KeyExpiresAt != "2027-01-01T00:00:00Z" || r.Account != "poe-person" {
		t.Errorf("the connected Poe account: %+v", r)
	}

	text := cli(t, "providers", "list")
	for _, want := range []string{
		"copilot · " + policy.SignInSentence(policy.KeyKindCopilotSignIn) + " · signed in as octo-fixture · through Agentry's " + mcpauth.GitHubAppName(mcpauth.GitHubApp),
		"last used: never used",
		"Kept sign-ins and connected accounts no provider uses:",
		"chatgpt as person@example.com",
		"remove: agentry providers remove openai --sign-in",
		"poe as poe-person",
		"key expires 2027-01-01T00:00:00Z",
		"remove: agentry providers remove poe\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("providers list lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "copilot as octo-fixture") {
		t.Errorf("the copilot sign-in, which a provider uses, is listed as kept for nothing:\n%s", text)
	}
	out, errOut, _ := runCLI(t, nil, "--json", "providers", "list")
	if strings.Contains(text+out+errOut, "PROBE-") {
		t.Fatal("a token was printed")
	}
}

// providers add on a provider paid by a sign-in signs in again and replaces
// the kept sign-in, revoking the one before; the provider and its saved
// models are left as they were, and it says so. A flag that would change
// the provider is refused, and so is a second provider paid by a key.
func TestProvidersAddSignsInAgainOnAProviderPaidByASignIn(t *testing.T) {
	v := copilotFixture(t)
	_, path := modelsFixture(t)
	cli(t, "providers", "add", "copilot")
	if len(v.DeviceRequests) != 1 {
		t.Fatalf("sign-ins: %d", len(v.DeviceRequests))
	}
	_, pol := readPolicy(t, path)
	if _, err := pol.AddModel(policy.SavedModel{Name: "Copilot auto", Provider: "copilot", Model: "auto"}); err != nil {
		t.Fatal(err)
	}
	if err := policy.Save(path, pol); err != nil {
		t.Fatal(err)
	}
	before, _ := readPolicy(t, path)

	out, errOut, err := runCLI(t, strings.NewReader(""), "--json", "providers", "add", "copilot")
	if err != nil {
		t.Fatalf("add again: %v\n%s", err, errOut)
	}
	var got providerOut
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.ID != "copilot" || !got.SignedInAgain || got.Account != "octo-fixture" ||
		strings.Join(got.Models, ",") != "Copilot auto" {
		t.Fatalf("add again = %s (%v)", out, err)
	}
	if len(v.DeviceRequests) != 2 {
		t.Fatalf("add on the existing provider did not sign in again: %d sign-ins", len(v.DeviceRequests))
	}
	if after, _ := readPolicy(t, path); after != before {
		t.Fatalf("the policy changed:\n%s\n---\n%s", before, after)
	}
	if len(v.Revoked) == 0 {
		t.Error("the sign-in replaced was not revoked at GitHub")
	}
	out, _, err = runCLI(t, strings.NewReader(""), "providers", "add", "copilot")
	if err != nil || !strings.Contains(out, "signed in again for the provider copilot") || !strings.Contains(out, "the provider and its saved models (Copilot auto) are as they were") {
		t.Fatalf("add again, text = %q (%v)", out, err)
	}

	_, _, err = runCLI(t, strings.NewReader(""), "providers", "add", "copilot", "--copilot-plan", "business")
	if err == nil || !strings.Contains(err.Error(), "--copilot-plan") || len(v.DeviceRequests) != 3 {
		t.Fatalf("a flag that changes the provider: %v (%d sign-ins)", err, len(v.DeviceRequests))
	}
	wantClassified(t, err, engine.ExitUsage)

	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	cli(t, "providers", "add", "openai")
	for _, args := range [][]string{{"providers", "add", "openai"}, {"providers", "add", "openai", "--sign-in"}} {
		_, _, err := runCLI(t, strings.NewReader(""), args...)
		if err == nil || !strings.Contains(err.Error(), "already has the provider openai") {
			t.Fatalf("%v on a key provider: %v", args, err)
		}
		wantClassified(t, err, engine.ExitUsage)
	}
}

// --new-account signs in to another ChatGPT account, through providers add
// openai --sign-in, on a new provider and on the one already here.
func TestProvidersAddOpenAISignInTakesNewAccount(t *testing.T) {
	v := chatgptFixture(t)
	modelsFixture(t)
	cli(t, "providers", "add", "openai", "--sign-in")
	if len(v.Issued) != 1 {
		t.Fatalf("issued %d", len(v.Issued))
	}
	reg, err := modelsignin.Open(mustRunnerHome(t), nil).Registration(modelsignin.ChatGPT)
	if err != nil || reg == nil {
		t.Fatalf("registration %+v %v", reg, err)
	}
	out := cli(t, "providers", "add", "openai", "--sign-in", "--new-account")
	if len(v.Issued) != 2 || !strings.Contains(out, "signed in again for the provider openai") {
		t.Fatalf("--new-account on the provider here: %d issued\n%s", len(v.Issued), out)
	}
	if v.Live(v.Issued[0]) {
		t.Error("the sign-in replaced is still live at OpenAI")
	}
	_, _, err = runCLI(t, strings.NewReader(""), "providers", "add", "copilot", "--new-account")
	if err == nil || !strings.Contains(err.Error(), "--new-account is for openai --sign-in") {
		t.Fatalf("--new-account on copilot: %v", err)
	}
}

// providers add <vendor> --connect on a provider a connected account pays
// connects again, keeping the new key in the same keychain entry.
func TestProvidersAddConnectsAgainOnAConnectedProvider(t *testing.T) {
	f := newConnectFixture(t)
	cli(t, "providers", "add", "openrouter", "--connect")
	first := f.key("openrouter")
	if first == "" {
		t.Fatal("the connect kept no key")
	}
	out, errOut, err := runCLI(t, strings.NewReader(""), "--json", "providers", "add", "openrouter", "--connect")
	if err != nil {
		t.Fatalf("connect again: %v\n%s", err, errOut)
	}
	var got providerOut
	if err := json.Unmarshal([]byte(out), &got); err != nil || !got.SignedInAgain || got.Kind != "connected account" {
		t.Fatalf("connect again = %s (%v)", out, err)
	}
	if again := f.key("openrouter"); again == "" || again == first || strings.Contains(out+errOut, again) {
		t.Fatal("the key was not replaced, or was printed")
	}
	_, _, err = runCLI(t, strings.NewReader(""), "providers", "add", "openrouter")
	if err == nil || !strings.Contains(err.Error(), "already has the provider openrouter") {
		t.Fatalf("a key on the connected provider: %v", err)
	}
}

// providers remove <id> removes a kept sign-in no provider uses, revoking
// it as model-signins remove did, and says so in --json; --sign-in removes
// a sign-in kept beside a provider paid another way and leaves the
// provider. An id with neither is refused, and a vendor's own name is
// pointed at its provider's id.
func TestProvidersRemoveRemovesAKeptSignInNoProviderUses(t *testing.T) {
	v := chatgptFixture(t)
	_, path := modelsFixture(t)
	cli(t, "providers", "add", "openai", "--sign-in")
	live := v.Issued[len(v.Issued)-1]
	out := cli(t, "providers", "remove", "openai", "--keep-credential")
	if !strings.Contains(out, "credential kept") || !v.Live(live) {
		t.Fatalf("remove --keep-credential: %s", out)
	}
	if text := cli(t, "providers", "list"); !strings.Contains(text, "remove: agentry providers remove openai\n") {
		t.Fatalf("the kept sign-in is not listed with its removal:\n%s", text)
	}

	out, errOut, err := runCLI(t, nil, "--json", "providers", "remove", "openai")
	if err != nil {
		t.Fatalf("remove the kept sign-in: %v\n%s", err, errOut)
	}
	var got removedOut
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Provider != "openai" || got.ProviderRemoved || got.SignIn != modelsignin.ChatGPT ||
		got.Revocation != modelsignin.RevokeDone || got.Removed == nil || len(got.Removed) != 0 {
		t.Fatalf("remove --json = %s (%v)", out, err)
	}
	if v.Live(live) {
		t.Fatal("the sign-in was not revoked at OpenAI")
	}
	if _, err := modelsignin.Open(mustRunnerHome(t), nil).Get(modelsignin.ChatGPT); err == nil {
		t.Fatal("the sign-in is still kept")
	}

	_, _, err = runCLI(t, nil, "providers", "remove", "openai")
	if err == nil || !strings.Contains(err.Error(), `no provider "openai"`) {
		t.Fatalf("nothing kept: %v", err)
	}
	wantClassified(t, err, engine.ExitUsage)
	_, _, err = runCLI(t, nil, "providers", "remove", "chatgpt")
	if err == nil || !strings.Contains(err.Error(), "agentry providers remove openai") {
		t.Fatalf("a vendor's name: %v", err)
	}

	// A key provider and a sign-in kept beside it: --sign-in removes the
	// sign-in alone.
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	cli(t, "providers", "add", "openai")
	cli(t, "model-signins", "add", "chatgpt")
	before, _ := readPolicy(t, path)
	out = cli(t, "providers", "remove", "openai", "--sign-in")
	if !strings.Contains(out, "removed the chatgpt sign-in (revocation: "+modelsignin.RevokeDone+")") || !strings.Contains(out, "the provider openai is kept") {
		t.Fatalf("remove --sign-in: %s", out)
	}
	if after, _ := readPolicy(t, path); after != before {
		t.Fatal("remove --sign-in changed the policy")
	}
	_, _, err = runCLI(t, nil, "providers", "remove", "openai", "--connect")
	if err == nil || !strings.Contains(err.Error(), "--connect is for openrouter, poe and huggingface") {
		t.Fatalf("--connect on openai: %v", err)
	}
}

// Removing a kept Copilot sign-in no provider uses says what GitHub keeps.
func TestProvidersRemoveOfAKeptCopilotSignInNamesGitHubsAuthorizedApps(t *testing.T) {
	copilotFixture(t)
	modelsFixture(t)
	cli(t, "providers", "add", "copilot")
	cli(t, "providers", "remove", "copilot", "--keep-credential")
	out := cli(t, "providers", "remove", "copilot")
	if !strings.Contains(out, "revocation: "+modelsignin.RevokeSubmitted) || !strings.Contains(out, "stays in your authorized apps") {
		t.Fatalf("remove = %s", out)
	}
}

// model-signins is the deprecated alias: hidden from the help, each
// subcommand works as it did and says on stderr which providers command
// replaces it, so its --json stays one object on stdout. A removal's
// replacement removes the sign-in alone, as model-signins remove did,
// whether or not a provider is here.
func TestModelSignInsIsADeprecatedAliasOfProviders(t *testing.T) {
	home, _ := modelsFixture(t)
	if _, err := modelsignin.Open(home, nil).Put(modelsignin.ChatGPT, modelsignin.Record{Label: "person@example.com"}, modelsignin.Grant{}); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := runCLI(t, nil, "--json", "model-signins", "list")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		ModelSignIns []modelSignInOut `json:"modelSignIns"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list.ModelSignIns) != 1 || list.ModelSignIns[0].Account != "person@example.com" {
		t.Fatalf("model-signins list --json = %s (%v)", out, err)
	}
	if !strings.Contains(errOut, `"model-signins" is deprecated`) || !strings.Contains(errOut, "agentry providers list") {
		t.Fatalf("no deprecation note on stderr: %q", errOut)
	}
	out, errOut, err = runCLI(t, nil, "--json", "model-signins", "remove", "chatgpt")
	var removed removedModelSignIn
	if err != nil || json.Unmarshal([]byte(out), &removed) != nil || removed.Vendor != "chatgpt" || !strings.Contains(errOut, "agentry providers remove openai --sign-in") {
		t.Fatalf("model-signins remove --json = %s / %q (%v)", out, errOut, err)
	}

	help, _, err := runCLI(t, nil, "--help")
	if err != nil || strings.Contains(help, "model-signins") || !strings.Contains(help, "providers") {
		t.Fatalf("the root help names model-signins, or lacks providers:\n%s", help)
	}

	for args, want := range map[string]string{
		"list":                       "agentry providers list",
		"add chatgpt":                "agentry providers add openai --sign-in",
		"add chatgpt --new-account":  "agentry providers add openai --sign-in --new-account",
		"add claude":                 "agentry providers add anthropic --sign-in",
		"add claude --installed":     "agentry providers add anthropic --sign-in --installed",
		"add copilot":                "agentry providers add copilot",
		"add copilot --oauth-app":    "agentry providers add copilot --oauth-app",
		"add openrouter":             "agentry providers add openrouter --connect",
		"add huggingface":            "agentry providers add huggingface --connect",
		"remove poe":                 "agentry providers remove poe --connect",
		"remove claude":              "agentry providers remove anthropic --sign-in",
		"add somebody":               "agentry providers add",
		"remove copilot --something": "agentry providers remove copilot --sign-in",
	} {
		f := strings.Fields(args)
		flagsOn := map[string]bool{}
		var rest []string
		for _, a := range f[1:] {
			if strings.HasPrefix(a, "--") {
				flagsOn[strings.TrimPrefix(a, "--")] = true
			} else {
				rest = append(rest, a)
			}
		}
		if got := providersCommandFor(f[0], rest, flagsOn); got != want {
			t.Errorf("model-signins %s → %q, want %q", args, got, want)
		}
	}
}
