package claudesignin

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/harnessbuild"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// signInURL is what Claude Code 2.1.296 prints for a sign-in whose browser
// cannot reach its callback (the paste-code page).
const signInURL = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&state=abc"

// fixture is a sign-in on a fake daemon where the Claude Code image is
// already built from its pinned artifact, with Claude Code's login, status
// and logout scripted.
func fixture(t *testing.T, login *dockerx.FakeForeground, statusJSON string) (SignIn, *dockerx.Fake) {
	t.Helper()
	pol := policy.Default()
	h, _ := pol.Images.HarnessImageFor("claude-image")
	f := &dockerx.Fake{
		ExistingImages: map[string]bool{h.Ref: true},
		Labels:         map[string]map[string]string{h.Ref: {harnessbuild.LabelArtifact: h.Artifact}},
		Foreground: map[string]*dockerx.FakeForeground{
			"agentry-claude-login":  login,
			"agentry-claude-status": {Stdout: statusJSON},
			"agentry-claude-logout": {Stdout: "Successfully logged out from your Anthropic account.\n"},
		},
	}
	return SignIn{Home: t.TempDir(), Policy: pol, Docker: func() dockerx.Client { return f }}, f
}

const signedIn = `{"loggedIn":true,"authMethod":"claude.ai","email":"person@example.com","orgName":"Example"}`

// The image's sign-in is Claude Code's own, driven through the package's
// seams alone: the person's input reaches Claude Code's prompt unread, only
// Anthropic's printed page is opened, and the record keeps the volume and
// the account, never a token.
func TestTheImageSignsInThroughThePersonsSide(t *testing.T) {
	login := &dockerx.FakeForeground{Stdout: "see https://evil.example/x\nIf the browser didn't open, visit: " + signInURL + "\nPaste code here if prompted > Login successful.\n"}
	s, f := fixture(t, login, signedIn)
	var opened []string
	var out strings.Builder
	rec, err := s.Run(context.Background(), Person{In: strings.NewReader("the-code#the-state\n"), Out: &out, Open: func(u string) { opened = append(opened, u) }}, false)
	if err != nil {
		t.Fatal(err)
	}
	if login.GotStdin != "the-code#the-state\n" {
		t.Fatalf("Claude Code's prompt read %q", login.GotStdin)
	}
	if !slices.Equal(opened, []string{signInURL}) {
		t.Fatalf("opened %v", opened)
	}
	if rec.ConfigDir != Volume || rec.HostConfigDir != "" || rec.Label != "person@example.com (Example)" || rec.SecretIn != "" {
		t.Fatalf("record %+v", rec)
	}
	if strings.Contains(out.String(), "the-code") {
		t.Fatal("the pasted code was echoed to the output")
	}
	if !strings.Contains(strings.Join(f.Ops(), " "), "VolumeCreate:"+Volume) {
		t.Fatalf("ops %v", f.Ops())
	}
	if Kept(s.Home) != "person@example.com (Example)" {
		t.Fatalf("Kept = %q", Kept(s.Home))
	}
}

// A first sign-in that does not end in a Claude account keeps nothing: no
// record, and the volume it made is removed.
func TestAFailedFirstSignInRemovesTheVolumeItMade(t *testing.T) {
	s, f := fixture(t, &dockerx.FakeForeground{Stdout: "Login successful.\n"}, `{"loggedIn":true,"authMethod":"console"}`)
	if _, err := s.Run(context.Background(), Person{}, false); err == nil || !strings.Contains(err.Error(), "nothing kept") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(strings.Join(f.Ops(), " "), "VolumeRemove:"+Volume) {
		t.Fatalf("ops %v", f.Ops())
	}
	if _, err := modelsignin.Open(s.Home, nil).Get(modelsignin.Claude); err == nil {
		t.Fatal("a record was kept")
	}
}

// Removing it runs Claude Code's own logout in the image before the volume
// is deleted, and touches nothing a record names that is not its own.
func TestEndLogsOutThenDeletesOnlyItsOwn(t *testing.T) {
	s, f := fixture(t, &dockerx.FakeForeground{}, signedIn)
	end := s.End(nil)
	if _, err := end(context.Background(), modelsignin.Record{ConfigDir: "someone-elses"}); err == nil {
		t.Fatal("a volume that is not the sign-in's was accepted")
	}
	msg, err := end(context.Background(), modelsignin.Record{ConfigDir: Volume})
	if err != nil || !strings.Contains(msg, "logged out") {
		t.Fatalf("%q, %v", msg, err)
	}
	ops := f.Ops()
	li := slices.IndexFunc(ops, func(o string) bool { return strings.HasPrefix(o, "RunForeground:agentry-claude-logout") })
	vi := slices.Index(ops, "VolumeRemove:"+Volume)
	if li < 0 || vi < li || slices.Contains(ops, "VolumeRemove:someone-elses") {
		t.Fatalf("ops %v", ops)
	}
}

// Agentry opens Anthropic's own sign-in pages from Claude Code's output and
// nothing else.
func TestOnlyAnthropicsSignInPagesAreOpened(t *testing.T) {
	for raw, ok := range map[string]bool{
		signInURL: true, "https://platform.claude.com/oauth/authorize?x=1": true,
		"http://claude.com/cai/oauth/authorize": false, "https://claude.com.example/x": false,
		"https://user@claude.com/x": false, "https://evil.example/claude.com": false,
	} {
		if AnthropicSignInURL(raw) != ok {
			t.Errorf("%s: %v", raw, !ok)
		}
	}
	var opened []string
	var sink strings.Builder
	o := &urlOpener{w: &sink, open: func(u string) { opened = append(opened, u) }}
	for _, chunk := range []string{"see https://evil.example/x\nIf the browser didn't open, visit: https://claude", ".com/cai/oauth/authorize?state=1\n", "again " + signInURL + "\n"} {
		_, _ = o.Write([]byte(chunk))
	}
	if !slices.Equal(opened, []string{"https://claude.com/cai/oauth/authorize?state=1"}) || !strings.Contains(sink.String(), "evil.example") {
		t.Fatalf("opened %v; passed through %q", opened, sink.String())
	}
}
