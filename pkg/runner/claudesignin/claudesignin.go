// Package claudesignin is "Use your Claude subscription (runs Claude Code)":
// Claude Code's own sign-in into a config directory of Agentry's, and its
// removal. The CLI (`agentry model-signins add|remove claude`, `agentry setup`)
// and the desktop's model setup both call it, so there is one implementation
// per machine.
//
// Anthropic forbids a third party to "collect, store, or intermediate
// Claude.ai credentials or session tokens", so Agentry never runs this
// sign-in: it starts the unmodified Claude Code's own `claude auth login`
// against a config directory of Agentry's own, never the person's
// ~/.claude, and Claude Code keeps and refreshes its credentials there.
// Agentry never reads that directory. Each Claude Code signs in where it
// runs: the image (the default) in its docker volume, through the person's
// browser with Claude Code's callback relayed into the container
// (browser.go), pasting the code Anthropic's page shows
// only where that cannot work; the installed Claude Code in a directory
// under the model sign-in store, with its own browser and callback.
package claudesignin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/harnessbuild"
	"github.com/blaktron/agentry-cli/pkg/runner/harnessprobe"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
)

// Volume is the Claude Code image's config directory: a docker volume of
// Agentry's own (engine.OwnSignInDir), labelled as the sign-in's.
const Volume = "agentry-claude-signin"

// Timeout bounds one sign-in, browser included.
const Timeout = 15 * time.Minute

// HostDir is the installed Claude Code's config directory: under the model
// sign-in store, so the engine admits it (engine.OwnSignInDir).
func HostDir(home string) string { return filepath.Join(modelsignin.Dir(home), "claude-code") }

// Person is the person's side of a sign-in.
type Person struct {
	// In is what Claude Code's prompt reads: the code the person pastes
	// from Anthropic's page, passed to Claude Code unread, where the
	// browser cannot hand the sign-in back (the fallback). nil gives it no
	// input, and a sign-in that would need a paste ends instead.
	In io.Reader
	// Out receives Claude Code's output and the sign-in's own sentences.
	Out io.Writer
	// Open opens an address in the person's browser. It is only ever
	// handed one of Anthropic's own sign-in pages (AnthropicSignInURL);
	// nil opens nothing, and the person opens the printed address.
	// Through the image, the page's answer comes back to this computer and
	// is relayed into the container (browser.go).
	Open func(url string)
}

func (p Person) out() io.Writer {
	if p.Out == nil {
		return io.Discard
	}
	return p.Out
}

// SignIn signs a Claude Code in for this machine.
type SignIn struct {
	// Home is the runner home (the model sign-in store's parent).
	Home string
	// Policy is the operator policy the image is prepared under; nil
	// resolves the default policy file, writing it from defaults when
	// absent.
	Policy *policy.Policy
	// Docker is the daemon the image's sign-in runs on; nil is dockerx.New.
	Docker func() dockerx.Client
	// Keys is the keychain the sign-in store uses; nil keeps nothing there.
	Keys connection.Keychain
}

func (s SignIn) docker() dockerx.Client {
	if s.Docker == nil {
		return dockerx.New()
	}
	return s.Docker()
}

func (s SignIn) policy() (*policy.Policy, error) {
	if s.Policy != nil {
		return s.Policy, nil
	}
	return DefaultPolicy()
}

// DefaultPolicy is the operator policy a sign-in prepares its image under
// when none is given: the default file, written from defaults when absent.
func DefaultPolicy() (*policy.Policy, error) {
	pf, err := policy.Resolve("")
	if err != nil {
		return nil, err
	}
	pol, _, err := policy.EnsureDefault(pf.Path)
	return pol, err
}

// status is what `claude auth status` prints (Claude Code 2.1.296).
type status struct {
	LoggedIn   bool   `json:"loggedIn"`
	AuthMethod string `json:"authMethod"`
	Email      string `json:"email"`
	OrgName    string `json:"orgName"`
}

// label is how the person knows the account: "Signed in as …".
func (s status) label() string {
	switch {
	case s.Email != "" && s.OrgName != "":
		return s.Email + " (" + s.OrgName + ")"
	case s.Email != "":
		return s.Email
	}
	return s.OrgName
}

// signedIn holds the status to a Claude account's sign-in: a Console or
// key login is not a Claude subscription.
func (s status) signedIn() error {
	if !s.LoggedIn || s.AuthMethod != "claude.ai" {
		return fmt.Errorf("nothing kept: Claude Code is not signed in with a Claude account (it reports %q)", s.AuthMethod)
	}
	return nil
}

func parseStatus(out []byte) (status, error) {
	var s status
	if err := json.Unmarshal(bytes.TrimSpace(out), &s); err != nil {
		return s, fmt.Errorf("read Claude Code's sign-in status: %w", err)
	}
	return s, nil
}

// urlRE finds an address in Claude Code's output.
var urlRE = regexp.MustCompile(`https://[^\s\x00-\x1f"'<>]+`)

// AnthropicSignInURL reports whether raw is a sign-in page of Anthropic's
// own, the only kind of address Agentry opens from Claude Code's output.
func AnthropicSignInURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	switch u.Hostname() {
	case "claude.com", "claude.ai", "platform.claude.com", "console.anthropic.com":
		return true
	}
	return false
}

// containerRun is one Claude Code command in the image, on its volume: the
// image's user, a read-only root, nothing of the person's, every
// capability dropped and the policy's docker.limits. It is the one
// container Agentry starts with open egress (the default bridge): Claude
// Code's sign-in reaches Anthropic's pages and token endpoint itself, and
// it carries no model traffic and no run.
func containerRun(pol *policy.Policy, image, what string, argv ...string) dockerx.RunSpec {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	lim := pol.Docker.Limits.WithDefaults()
	return dockerx.RunSpec{
		Name:       "agentry-claude-" + what + "-" + hex.EncodeToString(b),
		Image:      image,
		Entrypoint: "/usr/local/bin/claude",
		Argv:       argv,
		Env: map[string]string{
			"CLAUDE_CONFIG_DIR":   adapter.ClaudeImageConfigDir,
			"HOME":                "/home/node",
			"DISABLE_AUTOUPDATER": "1",
		},
		Mounts:      []dockerx.Mount{{Source: Volume, Target: adapter.ClaudeImageConfigDir}},
		Interactive: true,
		AutoRemove:  true,
		ReadOnly:    true,
		Tmpfs:       []string{"/tmp:mode=1777", "/home/node:mode=1777"},
	}.WithPosture(lim.Memory, lim.CPUs, lim.Pids)
}

// runContainer runs one Claude Code command in the image and removes its
// container when it returns, whatever the reason: a caller interrupted or
// timed out leaves no Claude Code running on the daemon (the docker client
// it kills does not stop a container it attached to).
func runContainer(ctx context.Context, d dockerx.Client, r dockerx.RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = d.RemoveForce(rctx, r.Name)
	}()
	return d.RunForeground(ctx, r, stdin, stdout, stderr)
}

// signInImage builds the Claude Code image when it is not here, then runs
// Claude Code's own sign-in in it, on its volume, and asks it who is signed
// in. It returns the volume and the account.
func (s SignIn) signInImage(ctx context.Context, p Person, pol *policy.Policy) (string, string, error) {
	errOut := p.out()
	h, ok := pol.Images.HarnessImageFor(adapter.ClaudeImageAdapterName)
	if !ok {
		return "", "", errors.New("the policy names no Claude Code image")
	}
	d := s.docker()
	if err := gateway.EnsureHarnessImage(ctx, d, adapter.ClaudeImageAdapterName, h, errOut); err != nil {
		return "", "", engine.Classify(engine.ExitGateway, err)
	}
	if err := d.VolumeCreate(ctx, Volume, map[string]string{"org.agentry.model-signin": modelsignin.Claude}); err != nil {
		return "", "", err
	}
	_, _ = fmt.Fprintf(errOut, "%s: Claude Code signs you in itself, in Anthropic's own pages in your browser, and keeps the sign-in in its docker volume %s, which Agentry never reads.\n", setup.ClaudeSignInLabel, Volume)
	open := p.Open
	if open == nil {
		open = func(string) {}
	}
	login := containerRun(pol, h.Ref, "login", "auth", "login")
	login.Env["BROWSER"] = harnessbuild.ClaudeOpener
	lctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	var (
		failMu sync.Mutex
		failed error
	)
	w := &browserSignIn{out: errOut, open: open, canPaste: p.In != nil, fail: func(err error) {
		failMu.Lock()
		failed = err
		failMu.Unlock()
		cancel()
	}}
	if ex, ok := d.(dockerx.Execer); ok {
		w.startRelay = func(port int) (func(), error) { return relay(lctx, ex, login.Name, port) }
	}
	code, err := runContainer(lctx, d, login, p.In, &stream{b: w}, &stream{b: w})
	w.close()
	failMu.Lock()
	ferr := failed
	failMu.Unlock()
	if ferr != nil {
		return "", "", ferr
	}
	if lctx.Err() != nil {
		return "", "", fmt.Errorf("nothing kept: the sign-in was interrupted, or not finished within %s", Timeout)
	}
	if err != nil {
		return "", "", fmt.Errorf("the sign-in in Claude Code: %w", err)
	}
	if code != 0 {
		return "", "", fmt.Errorf("nothing kept: the sign-in in Claude Code did not complete (exit %d)", code)
	}
	var out bytes.Buffer
	if _, err := runContainer(ctx, d, containerRun(pol, h.Ref, "status", "auth", "status"), nil, &out, io.Discard); err != nil {
		return "", "", fmt.Errorf("the sign-in status in Claude Code: %w", err)
	}
	st, err := parseStatus(out.Bytes())
	if err != nil {
		return "", "", err
	}
	if err := st.signedIn(); err != nil {
		return "", "", err
	}
	return Volume, st.label(), nil
}

// hostEnv is the installed Claude Code's environment for its sign-in and
// logout: the person's, without any credential Claude Code would read
// before its own sign-in, and with its config directory Agentry's own.
func hostEnv(dir string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "CLAUDE_CODE_OAUTH") || name == "CLAUDE_CONFIG_DIR" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "CLAUDE_CONFIG_DIR="+dir, "DISABLE_AUTOUPDATER=1")
}

// signInInstalled runs the installed Claude Code's own sign-in in
// Agentry's directory for it: Claude Code opens the browser and receives
// the code on its own callback. It returns the directory and the account.
func (s SignIn) signInInstalled(ctx context.Context, p Person, pol *policy.Policy) (string, string, error) {
	errOut := p.out()
	exe, _, err := harnessprobe.Find(pol, adapter.ClaudeAdapterName)
	if err != nil {
		return "", "", fmt.Errorf("the installed Claude Code: %w", err)
	}
	dir := HostDir(s.Home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	_, _ = fmt.Fprintf(errOut, "%s: the Claude Code installed on this computer signs you in itself and keeps the sign-in in %s (never your own ~/.claude), which Agentry never reads.\n", setup.ClaudeSignInLabel, dir)
	lctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	login := exec.CommandContext(lctx, exe, "auth", "login") //nolint:gosec // the installed Claude Code, found as the run finds it
	login.Env, login.Stdin, login.Stdout, login.Stderr = hostEnv(dir), p.In, errOut, errOut
	if err := login.Run(); err != nil {
		return "", "", fmt.Errorf("nothing kept: the sign-in in Claude Code did not complete (%w)", err)
	}
	st := exec.CommandContext(ctx, exe, "auth", "status") //nolint:gosec // as above
	st.Env = hostEnv(dir)
	// auth status exits 1 when no one is signed in, with its JSON still
	// printed: the JSON is the answer, and only no answer is an error.
	out, serr := st.Output()
	if len(bytes.TrimSpace(out)) == 0 {
		return "", "", fmt.Errorf("nothing kept: the installed Claude Code did not say who is signed in (%v)", serr)
	}
	got, err := parseStatus(out)
	if err != nil {
		return "", "", err
	}
	if err := got.signedIn(); err != nil {
		return "", "", err
	}
	return dir, got.label(), nil
}

// Run signs in the image's Claude Code, or the installed one, and keeps
// the record beside the other's sign-in if there is one. A failed first
// sign-in leaves nothing behind; a failed sign-in again leaves the kept
// one alone.
func (s SignIn) Run(ctx context.Context, p Person, installed bool) (*modelsignin.Record, error) {
	pol, err := s.policy()
	if err != nil {
		return nil, engine.Classify(engine.ExitPolicy, err)
	}
	store := modelsignin.Open(s.Home, s.Keys)
	var rec modelsignin.Record
	if old, err := store.Get(modelsignin.Claude); err == nil {
		rec.ConfigDir, rec.HostConfigDir = old.ConfigDir, old.HostConfigDir
	}
	var dir, label string
	if installed {
		dir, label, err = s.signInInstalled(ctx, p, pol)
	} else {
		dir, label, err = s.signInImage(ctx, p, pol)
	}
	if err != nil {
		// "Nothing kept" must be true: a first sign-in that failed may
		// still have left Claude Code's state (a Console login, a half
		// sign-in) in the directory it made, which no record would name
		// for a removal.
		if installed && rec.HostConfigDir == "" {
			_ = os.RemoveAll(HostDir(s.Home))
		}
		if !installed && rec.ConfigDir == "" {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			_ = s.docker().VolumeRemove(rctx, Volume)
			cancel()
		}
		return nil, err
	}
	if installed {
		rec.HostConfigDir = dir
	} else {
		rec.ConfigDir = dir
	}
	rec.Label = label
	return store.Put(modelsignin.Claude, rec, modelsignin.Grant{})
}

// Kept is the account the image's sign-in names, for "Signed in as …": ""
// when there is none (the image's sign-in is the default adapter's).
func Kept(home string) string {
	r, err := modelsignin.Open(home, nil).Get(modelsignin.Claude)
	if err != nil || r.ConfigDir == "" {
		return ""
	}
	if r.Label == "" {
		return "an account Claude Code did not name"
	}
	return r.Label
}

// End is the store's EndClaude: Claude Code's own logout in each of the
// sign-in's directories, then the directory deleted. A logout that cannot
// run (no image, no installed Claude Code, no network) does not keep the
// sign-in here: deleting the directory ends it on this computer, and the
// outcome says what Anthropic still holds. Only the volume and directory
// this package makes are ever deleted, whatever a record says.
func (s SignIn) End(out io.Writer) func(context.Context, modelsignin.Record) (string, error) {
	if out == nil {
		out = io.Discard
	}
	return func(ctx context.Context, r modelsignin.Record) (string, error) {
		pol, err := s.policy()
		if err != nil {
			return "", err
		}
		var notLoggedOut []string
		if r.ConfigDir != "" {
			if r.ConfigDir != Volume {
				return "", fmt.Errorf("the Claude sign-in names %q, not the Claude Code image's volume %s; nothing removed", r.ConfigDir, Volume)
			}
			d := s.docker()
			loggedOut := false
			if h, ok := pol.Images.HarnessImageFor(adapter.ClaudeImageAdapterName); ok {
				if here, _ := d.ImageExists(ctx, h.Ref); here {
					code, err := runContainer(ctx, d, containerRun(pol, h.Ref, "logout", "auth", "logout"), nil, out, out)
					loggedOut = err == nil && code == 0
				}
			}
			if !loggedOut {
				notLoggedOut = append(notLoggedOut, "the image's")
			}
			if err := d.VolumeRemove(ctx, r.ConfigDir); err != nil {
				return "", fmt.Errorf("delete the Claude Code image's sign-in volume %s: %w; the sign-in is kept", r.ConfigDir, err)
			}
		}
		if r.HostConfigDir != "" {
			if filepath.Clean(r.HostConfigDir) != HostDir(s.Home) {
				return "", fmt.Errorf("the Claude sign-in names %s, not the installed Claude Code's directory %s; nothing removed", r.HostConfigDir, HostDir(s.Home))
			}
			loggedOut := false
			if exe, _, err := harnessprobe.Find(pol, adapter.ClaudeAdapterName); err == nil {
				logout := exec.CommandContext(ctx, exe, "auth", "logout") //nolint:gosec // the installed Claude Code
				logout.Env, logout.Stdout, logout.Stderr = hostEnv(r.HostConfigDir), out, out
				loggedOut = logout.Run() == nil
			}
			if !loggedOut {
				notLoggedOut = append(notLoggedOut, "the installed one's")
			}
			if err := os.RemoveAll(r.HostConfigDir); err != nil {
				return "", fmt.Errorf("delete the installed Claude Code's sign-in directory %s: %w; the sign-in is kept", r.HostConfigDir, err)
			}
		}
		if len(notLoggedOut) == 0 {
			return "Claude Code logged out, and its sign-in directory was deleted", nil
		}
		return "Claude Code could not log out (" + strings.Join(notLoggedOut, ", ") + "); its sign-in directory was deleted, so this computer keeps no Claude sign-in, " +
			"and Anthropic's session ends when it expires or when you sign out of your devices at claude.ai", nil
	}
}
