package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/harnessbuild"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// claudeSignInURL is what Claude Code 2.1.296 prints for a sign-in whose
// browser cannot reach its callback (the paste-code page).
const claudeSignInURL = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&state=abc"

// claudeAutoURL is the address Claude Code 2.1.296 hands its browser
// opener in the image: its own callback on the container's loopback, which
// the sign-in relays. claudeFixture sets it.
var claudeAutoURL string

// claudeFixture points the CLI's Claude sign-in at a fake daemon on which
// the Claude Code image is already built from its pinned artifact, and
// records what the browser was asked to open.
func claudeFixture(t *testing.T) (*dockerx.Fake, *[]string) {
	t.Helper()
	isolateEnv(t)
	t.Setenv("AGENTRY_RUNNER_HOME", t.TempDir())
	t.Setenv("AGENTRY_NO_BROWSER", "")
	h, _ := policy.Default().Images.HarnessImageFor("claude-image")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	claudeAutoURL = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=http%3A%2F%2Flocalhost%3A" + strconv.Itoa(port) + "%2Fcallback&state=abc"
	f := &dockerx.Fake{
		ExistingImages: map[string]bool{h.Ref: true},
		Labels:         map[string]map[string]string{h.Ref: {harnessbuild.LabelArtifact: h.Artifact, harnessbuild.LabelRecipe: harnessbuild.ClaudeRecipe}},
		Foreground: map[string]*dockerx.FakeForeground{
			"agentry-claude-login": {Stdout: "Opening browser to sign in…\nIf the browser didn't open, visit: " + claudeSignInURL + "\nPaste code here if prompted > Login successful.\n",
				Stderr: "agentry-open " + claudeAutoURL + "\n"},
			"agentry-claude-status": {Stdout: `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","email":"person@example.com","orgName":"Example","subscriptionType":"max"}`},
			"agentry-claude-logout": {Stdout: "Successfully logged out from your Anthropic account.\n"},
		},
	}
	var opened []string
	prevDocker, prevOpen, prevKeys := claudeDocker, openBrowser, connectionKeys
	claudeDocker = func() dockerx.Client { return f }
	openBrowser = func(u string) error { opened = append(opened, u); return nil }
	connectionKeys = func() connection.Keychain { return nil }
	t.Cleanup(func() { claudeDocker, openBrowser, connectionKeys = prevDocker, prevOpen, prevKeys })
	return f, &opened
}

// `agentry model-signins add claude` runs the unmodified Claude Code's own
// sign-in in the image, on its volume: Agentry opens, in the person's
// browser, the address Claude Code hands its stand-in opener, whose answer
// is relayed to Claude Code's callback; the terminal still reaches Claude
// Code's prompt, for the paste fallback; and what is kept is the volume's
// name and the account Claude Code reports, never a token. Nothing of the
// person's credentials reaches the container.
func TestModelSignInsAddClaudeRunsClaudeCodesOwnSignIn(t *testing.T) {
	f, opened := claudeFixture(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-person-0000")
	out, errOut, err := runCLI(t, strings.NewReader("the-code#the-state\n"), "model-signins", "add", "claude", "--json")
	if err != nil {
		t.Fatalf("add: %v\n%s", err, errOut)
	}
	var got signedInJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Vendor != "claude" || got.Kind != policy.KeyKindClaudeSignIn || got.Account != "person@example.com (Example)" {
		t.Fatalf("add = %s (%v)", out, err)
	}
	if !slices.Equal(*opened, []string{claudeAutoURL}) {
		t.Fatalf("opened %v", *opened)
	}
	if !strings.Contains(errOut, "Use your Claude subscription (runs Claude Code)") || !strings.Contains(errOut, "Approve the sign-in in your browser") ||
		strings.Contains(errOut, "agentry-open") {
		t.Fatalf("what the person saw:\n%s", errOut)
	}
	login, ok := f.FindRun("agentry-claude-login")
	if !ok {
		t.Fatal("Claude Code's sign-in never ran")
	}
	if login.Memory == "" || login.CPUs == 0 || login.PidsLimit == 0 || !slices.Equal(login.CapDrop, []string{"ALL"}) || !login.NoNewPrivileges {
		t.Fatalf("the sign-in container is not bounded: %+v", login)
	}
	if login.Entrypoint != "/usr/local/bin/claude" || !slices.Equal(login.Argv, []string{"auth", "login"}) || !login.ReadOnly || !login.Interactive ||
		len(login.Mounts) != 1 || login.Mounts[0] != (dockerx.Mount{Source: claudeVolume, Target: "/claude"}) || login.Env["CLAUDE_CONFIG_DIR"] != "/claude" ||
		login.Env["BROWSER"] != harnessbuild.ClaudeOpener {
		t.Fatalf("the sign-in container %+v", login)
	}
	for k := range login.Env {
		if strings.HasPrefix(k, "ANTHROPIC_") || strings.HasPrefix(k, "CLAUDE_CODE_OAUTH") {
			t.Fatalf("the sign-in container is given %s", k)
		}
	}
	if gotIn := f.Foreground["agentry-claude-login"].GotStdin; gotIn != "the-code#the-state\n" {
		t.Fatalf("Claude Code's prompt read %q", gotIn)
	}
	ops := strings.Join(f.Ops(), " ")
	if !strings.Contains(ops, "VolumeCreate:"+claudeVolume) || strings.Contains(ops, "Build:") || strings.Contains(ops, "Pull:") {
		t.Fatalf("ops %s", ops)
	}
	// Every Claude Code container the sign-in started is removed after it,
	// so an interrupted CLI leaves none running.
	for _, c := range f.Calls {
		if c.Op == "RunForeground" && !strings.Contains(ops, "RemoveForce:"+c.Arg) {
			t.Fatalf("%s was not removed: %s", c.Arg, ops)
		}
	}
	r, err := modelsignin.Open(mustRunnerHome(t), nil).Get(modelsignin.Claude)
	if err != nil || r.ConfigDir != claudeVolume || r.HostConfigDir != "" || r.SecretIn != "" || r.Label != "person@example.com (Example)" {
		t.Fatalf("record %+v (%v)", r, err)
	}
}

// A sign-in Claude Code did not complete, or one that is not a Claude
// account's, keeps nothing.
func TestModelSignInsAddClaudeKeepsNothingUnlessClaudeCodeIsSignedIn(t *testing.T) {
	for name, edit := range map[string]func(*dockerx.Fake){
		"login failed": func(f *dockerx.Fake) { f.Foreground["agentry-claude-login"].ExitCode = 1 },
		"console": func(f *dockerx.Fake) {
			f.Foreground["agentry-claude-status"].Stdout = `{"loggedIn":true,"authMethod":"api_key"}`
		},
		"not signed in": func(f *dockerx.Fake) {
			f.Foreground["agentry-claude-status"].Stdout = `{"loggedIn":false,"authMethod":"none"}`
		},
	} {
		f, _ := claudeFixture(t)
		edit(f)
		if _, _, err := runCLI(t, strings.NewReader(""), "model-signins", "add", "claude"); err == nil {
			t.Fatalf("%s: kept", name)
		}
		if _, err := modelsignin.Open(mustRunnerHome(t), nil).Get(modelsignin.Claude); err == nil {
			t.Fatalf("%s: a record was kept", name)
		}
		// Nothing kept means nothing: the volume the first sign-in made goes.
		if !strings.Contains(strings.Join(f.Ops(), " "), "VolumeRemove:"+claudeVolume) {
			t.Fatalf("%s: the volume of a failed first sign-in was left: %v", name, f.Ops())
		}
	}
}

// Removing the Claude sign-in runs Claude Code's own logout in the image,
// then deletes the volume, then forgets the record.
func TestModelSignInsRemoveClaudeLogsOutThenDeletesTheVolume(t *testing.T) {
	f, _ := claudeFixture(t)
	if _, errOut, err := runCLI(t, strings.NewReader(""), "model-signins", "add", "claude"); err != nil {
		t.Fatalf("add: %v\n%s", err, errOut)
	}
	out, errOut, err := runCLI(t, nil, "model-signins", "remove", "claude")
	if err != nil || !strings.Contains(out, "Claude Code logged out, and its sign-in directory was deleted") {
		t.Fatalf("remove = %q (%v)\n%s", out, err, errOut)
	}
	logout, ok := f.FindRun("agentry-claude-logout")
	if !ok || !slices.Equal(logout.Argv, []string{"auth", "logout"}) || logout.Mounts[0].Source != claudeVolume {
		t.Fatalf("logout %+v (%v)", logout, ok)
	}
	ops := f.Ops()
	li, vi := slices.Index(ops, "RunForeground:"+logout.Name), slices.Index(ops, "VolumeRemove:"+claudeVolume)
	if li < 0 || vi < li {
		t.Fatalf("the volume was not deleted after the logout: %v", ops)
	}
	if _, err := modelsignin.Open(mustRunnerHome(t), nil).Get(modelsignin.Claude); err == nil {
		t.Fatal("the record outlived the removal")
	}
}

// fakeInstalledClaude puts a `claude` on PATH that plays Claude Code's auth
// commands in CLAUDE_CONFIG_DIR, and logs each call with the directory and
// whether a key reached it.
func fakeInstalledClaude(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake installed Claude Code is a shell script")
	}
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls.log")
	script := `#!/bin/sh
echo "$1 $2 config=$CLAUDE_CONFIG_DIR key=${ANTHROPIC_API_KEY:-none}" >> "` + log + `"
case "$1 $2" in
"auth login") echo "Opening browser to sign in…"; echo "If the browser didn't open, visit: ` + claudeSignInURL + `"; printf '{}' > "$CLAUDE_CONFIG_DIR/.credentials.json"; echo "Login successful.";;
"auth status") if [ -f "$CLAUDE_CONFIG_DIR/.credentials.json" ]; then echo '{"loggedIn":true,"authMethod":"claude.ai","email":"person@example.com"}'; else echo '{"loggedIn":false,"authMethod":"none"}'; exit 1; fi;;
"auth logout") rm -f "$CLAUDE_CONFIG_DIR/.credentials.json"; echo "Successfully logged out";;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil { //nolint:gosec // a test executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// --installed signs in the Claude Code installed on this computer, in a
// directory of Agentry's own under the model sign-in store (never
// ~/.claude), with none of the person's keys; Claude Code opens the
// browser itself, so Agentry opens nothing. The image's sign-in beside it
// is kept, and removal logs out and deletes both.
func TestModelSignInsAddClaudeInstalledUsesAgentrysOwnDirectory(t *testing.T) {
	f, opened := claudeFixture(t)
	log := fakeInstalledClaude(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-person-0000")
	if _, errOut, err := runCLI(t, strings.NewReader(""), "model-signins", "add", "claude"); err != nil {
		t.Fatalf("add (image): %v\n%s", err, errOut)
	}
	*opened = nil
	out, errOut, err := runCLI(t, strings.NewReader(""), "model-signins", "add", "claude", "--installed")
	if err != nil || !strings.Contains(out, "installed on this computer") {
		t.Fatalf("add --installed: %q %v\n%s", out, err, errOut)
	}
	if len(*opened) != 0 {
		t.Fatalf("Agentry opened %v; the installed Claude Code opens its own browser", *opened)
	}
	home := mustRunnerHome(t)
	dir := claudeHostDir(home)
	r, err := modelsignin.Open(home, nil).Get(modelsignin.Claude)
	if err != nil || r.HostConfigDir != dir || r.ConfigDir != claudeVolume {
		t.Fatalf("record %+v (%v)", r, err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the directory %v %v", fi, err)
	}
	b, _ := os.ReadFile(log) //nolint:gosec // the test's log
	if !strings.Contains(string(b), "auth login config="+dir+" key=none") || strings.Contains(string(b), ".claude ") {
		t.Fatalf("calls:\n%s", b)
	}
	out, _, err = runCLI(t, nil, "model-signins", "remove", "claude")
	if err != nil || !strings.Contains(out, "Claude Code logged out") {
		t.Fatalf("remove = %q (%v)", out, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the installed Claude Code's directory is still there: %v", err)
	}
	b, _ = os.ReadFile(log) //nolint:gosec // the test's log
	if !strings.Contains(string(b), "auth logout config="+dir) || !strings.Contains(strings.Join(f.Ops(), " "), "VolumeRemove:"+claudeVolume) {
		t.Fatalf("both were not ended:\n%s\n%v", b, f.Ops())
	}
}

// `agentry setup` with the Claude kind signs Claude Code in and writes the
// policy: no key, Anthropic's two hosts alone.
func TestSetupWithAClaudeSubscriptionWritesThePolicy(t *testing.T) {
	claudeFixture(t)
	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	_, errOut, err := runCLI(t, strings.NewReader(""), "setup", "--provider", "anthropic", "--api-key-kind", "claude-signin", "--model", "sonnet", "--yes", "--skip-images", "--policy", policyPath)
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, errOut)
	}
	pol, _, err := policy.Load(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	m := pol.Model
	if m.Provider != "anthropic" || m.Name != "sonnet" || m.APIKeyKind != policy.KeyKindClaudeSignIn || m.APIKeyEnv != "" ||
		strings.Join(m.ProxyAllow, ",") != "api.anthropic.com:443,platform.claude.com:443" {
		t.Fatalf("policy model %+v", m)
	}
	if r, err := modelsignin.Open(mustRunnerHome(t), nil).Get(modelsignin.Claude); err != nil || r.ConfigDir != claudeVolume {
		t.Fatalf("setup kept no sign-in: %+v %v", r, err)
	}
}

// A run is handed the directory of the Claude Code that runs it: the
// image's volume, or the installed one's directory; never a token.
func TestARunIsHandedItsClaudeCodesDirectory(t *testing.T) {
	claudeFixture(t)
	home := mustRunnerHome(t)
	if _, err := modelsignin.Open(home, nil).Put(modelsignin.Claude, modelsignin.Record{ConfigDir: claudeVolume, HostConfigDir: claudeHostDir(home)}, modelsignin.Grant{}); err != nil {
		t.Fatal(err)
	}
	msi := &spec.ModelSignInSpec{Kind: policy.KeyKindClaudeSignIn, Provider: policy.ProviderAnthropic, Delivery: spec.ModelSignInConfigDir, Path: "/claude"}
	image := &spec.Plan{Harness: spec.HarnessSpec{ModelSignIn: msi}}
	g, renew, err := runModelSignIn(context.Background(), home, image, false)
	if err != nil || g.ConfigDir != claudeVolume || g.Token.AccessToken != "" || renew != nil {
		t.Fatalf("image: %+v %v", g, err)
	}
	host := &spec.Plan{Harness: spec.HarnessSpec{ModelSignIn: msi, Host: &spec.HostProcessSpec{Command: "claude"}}}
	if g, _, err = runModelSignIn(context.Background(), home, host, false); err != nil || g.ConfigDir != claudeHostDir(home) {
		t.Fatalf("host: %+v %v", g, err)
	}
	if _, _, err = runModelSignIn(context.Background(), home, image, true); err == nil || !strings.Contains(err.Error(), "a hosted run never spends a model sign-in") {
		t.Fatalf("hosted: %v", err)
	}
}

// A failed sign-in again leaves the kept one alone: its volume is the
// record's, and remove ends it.
func TestAFailedSignInAgainKeepsTheKeptOne(t *testing.T) {
	f, _ := claudeFixture(t)
	if _, errOut, err := runCLI(t, strings.NewReader(""), "model-signins", "add", "claude"); err != nil {
		t.Fatalf("add: %v\n%s", err, errOut)
	}
	f.Foreground["agentry-claude-login"].ExitCode = 1
	if _, _, err := runCLI(t, strings.NewReader(""), "model-signins", "add", "claude"); err == nil {
		t.Fatal("a failed sign-in kept")
	}
	if strings.Contains(strings.Join(f.Ops(), " "), "VolumeRemove:") {
		t.Fatalf("the kept sign-in's volume was removed: %v", f.Ops())
	}
	if r, err := modelsignin.Open(mustRunnerHome(t), nil).Get(modelsignin.Claude); err != nil || r.ConfigDir != claudeVolume {
		t.Fatalf("the kept record: %+v %v", r, err)
	}
}

// Removal deletes only the volume and the directory this CLI makes, never
// another a tampered record names.
func TestRemovingClaudeDeletesOnlyAgentrysOwn(t *testing.T) {
	for name, rec := range map[string]modelsignin.Record{
		"another volume": {ConfigDir: "agentry-runner-cache"},
		"the store":      {HostConfigDir: "STORE"},
	} {
		f, _ := claudeFixture(t)
		home := mustRunnerHome(t)
		if rec.HostConfigDir == "STORE" {
			rec.HostConfigDir = modelsignin.Dir(home)
		}
		if _, err := modelsignin.Open(home, nil).Put(modelsignin.Claude, rec, modelsignin.Grant{}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := runCLI(t, nil, "model-signins", "remove", "claude"); err == nil || !strings.Contains(err.Error(), "nothing removed") {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(strings.Join(f.Ops(), " "), "VolumeRemove:") {
			t.Fatalf("%s: removed %v", name, f.Ops())
		}
		if _, err := os.Stat(modelsignin.Dir(home)); err != nil {
			t.Fatalf("%s: the store went: %v", name, err)
		}
	}
}
