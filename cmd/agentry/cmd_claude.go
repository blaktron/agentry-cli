// Use your Claude subscription (runs Claude Code) (plan model-sign-ins.md
// §4.4, D19–D24; agentry-cli#740): `agentry model-signins add claude`, its
// removal, and the offer in `agentry setup`.
//
// Anthropic forbids a third party to "collect, store, or intermediate
// Claude.ai credentials or session tokens", so Agentry never runs this
// sign-in: it starts the unmodified Claude Code's own `claude auth login`
// against a config directory of Agentry's own, never the person's
// ~/.claude, and Claude Code keeps and refreshes its credentials there.
// Agentry never reads that directory. Each Claude Code signs in where it
// runs: the image (the default) in its docker volume, where the browser
// cannot reach Claude Code's callback inside the container, so the person
// pastes the code Anthropic's page shows into Claude Code's own prompt; the
// installed Claude Code in a directory under the model sign-in store, with
// its own browser and callback.
package main

import (
	"bufio"
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
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/harnessprobe"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
)

// ClaudeSignInLabel is what every surface calls the choice (D24).
const ClaudeSignInLabel = setup.ClaudeSignInLabel

// claudeDocker is the daemon the image's sign-in runs on: a seam for tests.
var claudeDocker = func() dockerx.Client { return dockerx.New() }

// claudeVolume is the Claude Code image's config directory: a docker volume
// of Agentry's own (engine.OwnSignInDir), labelled as the sign-in's.
const claudeVolume = "agentry-claude-signin"

// claudeSignInTimeout bounds one sign-in, browser included.
const claudeSignInTimeout = 15 * time.Minute

// claudeHostDir is the installed Claude Code's config directory: under the
// model sign-in store, so the engine admits it (engine.OwnSignInDir).
func claudeHostDir(home string) string { return filepath.Join(modelsignin.Dir(home), "claude-code") }

// claudeStatus is what `claude auth status` prints (Claude Code 2.1.296).
type claudeStatus struct {
	LoggedIn   bool   `json:"loggedIn"`
	AuthMethod string `json:"authMethod"`
	Email      string `json:"email"`
	OrgName    string `json:"orgName"`
}

// label is how the person knows the account: "Signed in as …".
func (s claudeStatus) label() string {
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
func (s claudeStatus) signedIn() error {
	if !s.LoggedIn || s.AuthMethod != "claude.ai" {
		return fmt.Errorf("nothing kept: Claude Code is not signed in with a Claude account (it reports %q)", s.AuthMethod)
	}
	return nil
}

func parseClaudeStatus(out []byte) (claudeStatus, error) {
	var s claudeStatus
	if err := json.Unmarshal(bytes.TrimSpace(out), &s); err != nil {
		return s, fmt.Errorf("read Claude Code's sign-in status: %w", err)
	}
	return s, nil
}

// claudeURLRE finds an address in Claude Code's output.
var claudeURLRE = regexp.MustCompile(`https://[^\s\x00-\x1f"'<>]+`)

// anthropicSignInURL reports whether raw is a sign-in page of Anthropic's
// own, the only kind of address Agentry opens from Claude Code's output.
func anthropicSignInURL(raw string) bool {
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

// urlOpener passes Claude Code's output through and opens the first
// Anthropic sign-in address it prints, once its line is complete.
type urlOpener struct {
	w      io.Writer
	open   func(string)
	line   []byte
	opened bool
}

func (o *urlOpener) Write(p []byte) (int, error) {
	n, err := o.w.Write(p)
	if o.opened {
		return n, err
	}
	o.line = append(o.line, p...)
	for {
		i := bytes.IndexByte(o.line, '\n')
		if i < 0 {
			break
		}
		for _, m := range claudeURLRE.FindAllString(string(o.line[:i]), -1) {
			if anthropicSignInURL(m) {
				o.opened = true
				o.open(m)
				o.line = nil
				return n, err
			}
		}
		o.line = o.line[i+1:]
	}
	if len(o.line) > 64<<10 {
		o.line = nil
	}
	return n, err
}

// claudeStdin is what Claude Code's prompt reads: the terminal itself when
// nothing of it is buffered here, so the code the person pastes goes from
// the terminal to Claude Code without passing through Agentry.
func claudeStdin(cmd *cobra.Command, in *bufio.Reader) io.Reader {
	if f, ok := cmd.InOrStdin().(*os.File); ok && in.Buffered() == 0 {
		return f
	}
	return in
}

// claudeRun is one Claude Code command in the image, on its volume: the
// image's user, a read-only root, nothing of the person's.
func claudeRun(image, what string, argv ...string) dockerx.RunSpec {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
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
		Mounts:          []dockerx.Mount{{Source: claudeVolume, Target: adapter.ClaudeImageConfigDir}},
		Interactive:     true,
		AutoRemove:      true,
		ReadOnly:        true,
		Tmpfs:           []string{"/tmp:mode=1777", "/home/node:mode=1777"},
		CapDrop:         []string{"ALL"},
		NoNewPrivileges: true,
	}
}

// runClaude runs one Claude Code command in the image and removes its
// container when it returns, whatever the reason: a CLI interrupted or
// timed out leaves no Claude Code running on the daemon (the docker client
// it kills does not stop a container it attached to).
func runClaude(ctx context.Context, d dockerx.Client, r dockerx.RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = d.RemoveForce(rctx, r.Name)
	}()
	return d.RunForeground(ctx, r, stdin, stdout, stderr)
}

// claudePolicy is the operator policy the sign-in prepares its image under.
func claudePolicy() (*policy.Policy, error) {
	pf, err := policy.Resolve("")
	if err != nil {
		return nil, err
	}
	pol, _, err := policy.EnsureDefault(pf.Path)
	return pol, err
}

// signInClaudeImage builds the Claude Code image when it is not here, then
// runs Claude Code's own sign-in in it, on its volume, and asks it who is
// signed in. It returns the volume and the account.
func signInClaudeImage(cmd *cobra.Command, in *bufio.Reader, pol *policy.Policy) (string, string, error) {
	ctx, errOut := cmd.Context(), cmd.ErrOrStderr()
	h, ok := pol.Images.HarnessImageFor(adapter.ClaudeImageAdapterName)
	if !ok {
		return "", "", errors.New("the policy names no Claude Code image")
	}
	d := claudeDocker()
	if err := gateway.EnsureHarnessImage(ctx, d, adapter.ClaudeImageAdapterName, h, errOut); err != nil {
		return "", "", engine.Classify(engine.ExitGateway, err)
	}
	if err := d.VolumeCreate(ctx, claudeVolume, map[string]string{"org.agentry.model-signin": modelsignin.Claude}); err != nil {
		return "", "", err
	}
	_, _ = fmt.Fprintf(errOut, "%s: Claude Code signs you in itself, in Anthropic's own pages, and keeps the sign-in in its docker volume %s, which Agentry never reads.\n", ClaudeSignInLabel, claudeVolume)
	_, _ = fmt.Fprintln(errOut, "When the browser shows a code, copy it, paste it at Claude Code's prompt below and press Enter.")
	open := func(u string) {
		if os.Getenv("AGENTRY_NO_BROWSER") != "" {
			return
		}
		if err := openBrowser(u); err != nil {
			_, _ = fmt.Fprintf(errOut, "(could not open a browser: %v; open the address above yourself)\n", err)
		}
	}
	lctx, cancel := context.WithTimeout(ctx, claudeSignInTimeout)
	defer cancel()
	code, err := runClaude(lctx, d, claudeRun(h.Ref, "login", "auth", "login"), claudeStdin(cmd, in), &urlOpener{w: errOut, open: open}, errOut)
	if lctx.Err() != nil {
		return "", "", fmt.Errorf("nothing kept: the sign-in was interrupted, or not finished within %s", claudeSignInTimeout)
	}
	if err != nil {
		return "", "", fmt.Errorf("the sign-in in Claude Code: %w", err)
	}
	if code != 0 {
		return "", "", fmt.Errorf("nothing kept: the sign-in in Claude Code did not complete (exit %d)", code)
	}
	var out bytes.Buffer
	if _, err := runClaude(ctx, d, claudeRun(h.Ref, "status", "auth", "status"), nil, &out, io.Discard); err != nil {
		return "", "", fmt.Errorf("the sign-in status in Claude Code: %w", err)
	}
	st, err := parseClaudeStatus(out.Bytes())
	if err != nil {
		return "", "", err
	}
	if err := st.signedIn(); err != nil {
		return "", "", err
	}
	return claudeVolume, st.label(), nil
}

// claudeHostEnv is the installed Claude Code's environment for its sign-in
// and logout: the person's, without any credential Claude Code would read
// before its own sign-in, and with its config directory Agentry's own.
func claudeHostEnv(dir string) []string {
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

// signInInstalledClaude runs the installed Claude Code's own sign-in in
// Agentry's directory for it: Claude Code opens the browser and receives
// the code on its own callback. It returns the directory and the account.
func signInInstalledClaude(cmd *cobra.Command, in *bufio.Reader, home string, pol *policy.Policy) (string, string, error) {
	ctx, errOut := cmd.Context(), cmd.ErrOrStderr()
	exe, _, err := harnessprobe.Find(pol, adapter.ClaudeAdapterName)
	if err != nil {
		return "", "", fmt.Errorf("the installed Claude Code: %w", err)
	}
	dir := claudeHostDir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	_, _ = fmt.Fprintf(errOut, "%s: the Claude Code installed on this computer signs you in itself and keeps the sign-in in %s (never your own ~/.claude), which Agentry never reads.\n", ClaudeSignInLabel, dir)
	lctx, cancel := context.WithTimeout(ctx, claudeSignInTimeout)
	defer cancel()
	login := exec.CommandContext(lctx, exe, "auth", "login") //nolint:gosec // the installed Claude Code, found as the run finds it
	login.Env, login.Stdin, login.Stdout, login.Stderr = claudeHostEnv(dir), claudeStdin(cmd, in), errOut, errOut
	if err := login.Run(); err != nil {
		return "", "", fmt.Errorf("nothing kept: the sign-in in Claude Code did not complete (%w)", err)
	}
	status := exec.CommandContext(ctx, exe, "auth", "status") //nolint:gosec // as above
	status.Env = claudeHostEnv(dir)
	out, _ := status.Output()
	st, err := parseClaudeStatus(out)
	if err != nil {
		return "", "", err
	}
	if err := st.signedIn(); err != nil {
		return "", "", err
	}
	return dir, st.label(), nil
}

// signInClaude signs in the image's Claude Code, or the installed one, and
// keeps the record beside the other's sign-in if there is one.
func signInClaude(cmd *cobra.Command, in *bufio.Reader, home string, installed bool) (*modelsignin.Record, error) {
	pol, err := claudePolicy()
	if err != nil {
		return nil, engine.Classify(engine.ExitPolicy, err)
	}
	var dir, label string
	if installed {
		dir, label, err = signInInstalledClaude(cmd, in, home, pol)
	} else {
		dir, label, err = signInClaudeImage(cmd, in, pol)
	}
	if err != nil {
		return nil, err
	}
	s := modelsignin.Open(home, connectionKeys())
	var rec modelsignin.Record
	if old, err := s.Get(modelsignin.Claude); err == nil {
		rec.ConfigDir, rec.HostConfigDir = old.ConfigDir, old.HostConfigDir
	}
	if installed {
		rec.HostConfigDir = dir
	} else {
		rec.ConfigDir = dir
	}
	rec.Label = label
	return s.Put(modelsignin.Claude, rec, modelsignin.Grant{})
}

// endClaude is the store's EndClaude: Claude Code's own logout in each of
// the sign-in's directories, then the directory deleted. A logout that
// cannot run (no image, no installed Claude Code, no network) does not
// keep the sign-in here: deleting the directory ends it on this computer,
// and the outcome says what Anthropic still holds.
func endClaude(errOut io.Writer, home string) func(context.Context, modelsignin.Record) (string, error) {
	return func(ctx context.Context, r modelsignin.Record) (string, error) {
		pol, err := claudePolicy()
		if err != nil {
			return "", err
		}
		var notLoggedOut []string
		if r.ConfigDir != "" {
			if !engine.OwnSignInDir("", r.ConfigDir) {
				return "", fmt.Errorf("the Claude sign-in names %q, which is not a docker volume of Agentry's own; nothing removed", r.ConfigDir)
			}
			d := claudeDocker()
			loggedOut := false
			if h, ok := pol.Images.HarnessImageFor(adapter.ClaudeImageAdapterName); ok {
				if here, _ := d.ImageExists(ctx, h.Ref); here {
					code, err := runClaude(ctx, d, claudeRun(h.Ref, "logout", "auth", "logout"), nil, errOut, errOut)
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
			if !engine.OwnSignInDir(home, r.HostConfigDir) {
				return "", fmt.Errorf("the Claude sign-in names %s, which is not a directory of Agentry's own; nothing removed", r.HostConfigDir)
			}
			loggedOut := false
			if exe, _, err := harnessprobe.Find(pol, adapter.ClaudeAdapterName); err == nil {
				logout := exec.CommandContext(ctx, exe, "auth", "logout") //nolint:gosec // the installed Claude Code
				logout.Env, logout.Stdout, logout.Stderr = claudeHostEnv(r.HostConfigDir), errOut, errOut
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

// claudeOffer is what `agentry setup` offers of the Claude sign-in: the
// image's, the default adapter's (D23).
func claudeOffer(cmd *cobra.Command, in *bufio.Reader, home string) setup.ClaudeOffer {
	return setup.ClaudeOffer{
		Kept: func() string {
			r, err := modelsignin.Open(home, nil).Get(modelsignin.Claude)
			if err != nil || r.ConfigDir == "" {
				return ""
			}
			if r.Label == "" {
				return "an account Claude Code did not name"
			}
			return r.Label
		},
		SignIn: func() (string, error) {
			rec, err := signInClaude(cmd, in, home, false)
			if err != nil {
				return "", err
			}
			return rec.Label, nil
		},
	}
}
