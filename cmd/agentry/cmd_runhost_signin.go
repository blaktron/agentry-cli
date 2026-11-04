package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/remote"
	"github.com/blaktron/agentry-cli/pkg/runhost/signin"
	"github.com/blaktron/agentry-cli/pkg/runhost/store"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// A remote runhost's model sign-ins: Claude Code, ChatGPT and Copilot signed
// in on the server from this device's browser (pkg/runhost/signin). Claude
// Code keeps its sign-in in its docker volume on the runhost; ChatGPT's and
// Copilot's refresh tokens are kept in the runhost's store, encrypted.

// onRunhostServer says whether this CLI runs as a runhost's own account on
// its server: the service and the runs it starts.
func onRunhostServer() bool {
	u, err := user.Current()
	if err != nil || u.Username != runhost.User || u.HomeDir != runhost.Home {
		return false
	}
	_, err = os.Stat(runhost.ConfigFile)
	return err == nil
}

func init() {
	// On a runhost the sign-in store's refresh tokens are kept in the
	// runhost's encrypted store: the server has no keychain of its own.
	osKeys := connectionKeys
	connectionKeys = func() connection.Keychain {
		if onRunhostServer() {
			return runhostKeychain{}
		}
		return osKeys()
	}
	// Its model keys too, which its persistent agents' runs read.
	if onRunhostServer() {
		_ = modelkey.UseKeychain(runhostModelKeys{})
		// Every container here runs under gVisor, which has no Landlock.
		engine.WorkspaceOwnership = true
		persistent.KeptIn = "this runhost's store, encrypted"

	}
}

// runhostModelKeys is the runhost's store as modelkey's keychain: it answers
// modelkey.ErrNotStored for a key it does not keep.
type runhostModelKeys struct{}

func (runhostModelKeys) Get(account string) (string, error) {
	v, err := runhostKeychain{}.Get(account)
	if errors.Is(err, store.ErrNoEntry) {
		return "", modelkey.ErrNotStored
	}
	return v, err
}

func (runhostModelKeys) Set(account, value string) error {
	return runhostKeychain{}.Set(account, value)
}

func (runhostModelKeys) Delete(account string) error {
	err := runhostKeychain{}.Delete(account)
	if errors.Is(err, store.ErrNoEntry) || errors.Is(err, fs.ErrNotExist) {
		return modelkey.ErrNotStored
	}
	return err
}

// runhostKeychain keeps the connection stores' secrets as entries of the
// runhost's store: a model sign-in's, a schedule's OAuth connection's
// refresh token, and the person's own OAuth client's secret. It takes no
// other account, so nothing else lands there by accident.
type runhostKeychain struct{}

const (
	signInAccountPrefix     = "model-signin:"
	connectionAccountPrefix = "connection:"
	ownClientAccountPrefix  = "mcp-client:"
)

// hexIDRE is a connection's or an issuer's id in its keychain account;
// providerRE a model provider's name in a model key's;
// agentAccountRE a persistent agent's: a stored secret's, an image server's
// key's, a git host account's, a calendar feed's address's or its
// sign-in's, or a messenger account's credential.
var (
	hexIDRE        = regexp.MustCompile(`^[0-9a-f]{16,24}$`)
	providerRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
	agentAccountRE = regexp.MustCompile(`^(agent-secret:[a-z0-9][a-z0-9.-]{0,127}:[A-Za-z_][A-Za-z0-9_-]{0,127}|account:[a-z0-9][a-z0-9.:-]{0,252}:[A-Za-z0-9][A-Za-z0-9._-]{0,99}|calendar-feed:[a-z0-9][a-z0-9.-]{0,127}:[A-Za-z0-9][A-Za-z0-9._-]{0,63}|calendar-session:[a-z0-9][a-z0-9.-]{0,127}:[A-Za-z0-9][A-Za-z0-9._-]{0,63}:[0-9]{1,3}|messenger:[a-z0-9][a-z0-9.-]{0,127}:[a-z][a-z0-9-]{0,31})$`)
)

func runhostKeychainEntry(account string) (string, error) {
	if vendor, ok := strings.CutPrefix(account, signInAccountPrefix); ok && signin.IsVendor(vendor) {
		return "signin-" + vendor, nil
	}
	if id, ok := strings.CutPrefix(account, connectionAccountPrefix); ok && hexIDRE.MatchString(id) {
		return "conn-" + id, nil
	}
	if id, ok := strings.CutPrefix(account, ownClientAccountPrefix); ok && hexIDRE.MatchString(id) {
		return "client-" + id, nil
	}
	if p := modelkey.ProviderOf(account); p != "" && providerRE.MatchString(p) {
		return "model-key-" + p, nil
	}
	if m, ok := strings.CutPrefix(account, "stored:"); ok {
		if p := modelkey.ProviderOf(m); p != "" && providerRE.MatchString(p) {
			return "model-key-stored-" + p, nil
		}
	}
	if agentAccountRE.MatchString(account) {
		sum := sha256.Sum256([]byte(account))
		return "agent-" + hex.EncodeToString(sum[:12]), nil
	}
	return "", fmt.Errorf("a runhost keeps only its model keys, its agents' stored secrets, git host accounts and messenger credentials, its model sign-ins', its schedules' connections' and their OAuth clients' secrets, not %q", account)
}

func (runhostKeychain) Get(account string) (string, error) {
	name, err := runhostKeychainEntry(account)
	if err != nil {
		return "", err
	}
	st, err := openRunhostStore()
	if err != nil {
		return "", err
	}
	b, err := st.Entry(name)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (runhostKeychain) Set(account, value string) error {
	name, err := runhostKeychainEntry(account)
	if err != nil {
		return err
	}
	st, err := openRunhostStore()
	if err != nil {
		return err
	}
	return st.PutEntry(name, []byte(value))
}

func (runhostKeychain) Delete(account string) error {
	name, err := runhostKeychainEntry(account)
	if err != nil {
		return err
	}
	st, err := openRunhostStore()
	if err != nil {
		return err
	}
	return st.RemoveEntry(name)
}

// runhostSignIns is the service's sign-in server.
func runhostSignIns() (*signin.Server, error) {
	home, err := runnerHome()
	if err != nil {
		return nil, err
	}
	path, err := policy.DefaultPath()
	if err != nil {
		return nil, err
	}
	return &signin.Server{
		Home: home, Keys: connectionKeys(), PolicyPath: path, Docker: newDockerClient,
		Connections: func() (*connection.Store, *connection.Clients) {
			return connection.Open(home, runhostKeychain{}), connection.OpenClients(home, runhostKeychain{})
		},
		ConnectHTTP: vendorHTTP, CIMDURL: nativeCIMD,
		ChatGPT: func() mcpauth.ChatGPT {
			f := mcpauth.ChatGPT{HTTP: vendorHTTP()}
			if chatgptEndpoints != nil {
				chatgptEndpoints(&f)
			}
			return f
		},
		GitHub: func() mcpauth.GitHub {
			f := mcpauth.GitHub{HTTP: vendorHTTP()}
			if githubEndpoints != nil {
				githubEndpoints(&f)
			}
			return f
		},
	}, nil
}

// runhostSignInVerb answers the run key's sign-in verb:
//
//	sign-in start <vendor> [app]  signs the runhost in, the device's browser relaying
//	sign-in connection            a connection of the runhost's own, its request on the first line
//	sign-in relay <id>            one browser connection to the sign-in's callback
func runhostSignInVerb(s *signin.Server) runhost.Handler {
	return func(ctx context.Context, req runhost.Request, in io.Reader, fw *runhost.FrameWriter) (int, error) {
		switch {
		case len(req.Args) >= 2 && len(req.Args) <= 3 && req.Args[0] == "start":
			app := ""
			if len(req.Args) == 3 {
				app = req.Args[2]
				if app != mcpauth.GitHubApp && app != mcpauth.GitHubOAuthApp {
					return engine.ExitUsage, fmt.Errorf("GitHub's app %q is not one Copilot signs in through", app)
				}
			}
			return s.Start(ctx, req.Args[1], app, in, fw)
		case len(req.Args) == 1 && req.Args[0] == signin.ConnectionVendor:
			return s.Connect(ctx, in, fw)
		case len(req.Args) == 2 && req.Args[0] == "relay":
			return s.Relay(ctx, req.Args[1], in, fw)
		}
		return engine.ExitUsage, errors.New("sign-in takes start <vendor>, connection or relay <id>")
	}
}

// runhostSignInRemove answers the store verb's remove-signin.
func runhostSignInRemove(ctx context.Context, vendor string, fw *runhost.FrameWriter) (int, error) {
	s, err := runhostSignIns()
	if err != nil {
		return 1, err
	}
	out, err := s.Remove(ctx, vendor, &lineWriter{say: func(t string) { _ = fw.Out("%s", t) }})
	if err != nil {
		return 1, err
	}
	return 0, fw.Result(out)
}

// runhostSignInList is the store's sign-ins, for its list.
func runhostSignInList() []signin.Kept {
	s, err := runhostSignIns()
	if err != nil {
		return nil
	}
	list, _ := s.List()
	return list
}

// runhostJobSignIn refuses a job whose model is a sign-in the runhost does
// not keep.
func runhostJobSignIn(kind string) error {
	s, err := runhostSignIns()
	if err != nil {
		return err
	}
	if err := s.HasSignIn(kind); err != nil {
		return fmt.Errorf("%w: agentry runhost model %s --sign-in %s signs it in from your browser", err, runhostOwnName(), modelsignin.VendorOf(kind))
	}
	return nil
}

// lineWriter hands each line written to say.
type lineWriter struct {
	say func(string)
	buf []byte
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := strings.IndexByte(string(l.buf), '\n')
		if i < 0 {
			return len(p), nil
		}
		if t := strings.TrimSpace(string(l.buf[:i])); t != "" {
			l.say(t)
		}
		l.buf = l.buf[i+1:]
	}
}

// signInLabels are the sign-ins as the person is told of them.
var signInLabels = map[string]string{
	modelsignin.Claude:  "your Claude subscription (Claude Code)",
	modelsignin.ChatGPT: "your ChatGPT plan (Codex)",
	modelsignin.Copilot: "your GitHub Copilot plan (Copilot CLI)",
}

// signInConsent is what the person agrees to before a runhost signs in.
func signInConsent(vendor, name, host string) string {
	switch vendor {
	case modelsignin.Claude:
		return fmt.Sprintf("Sign Claude Code in on %s (%s) from your browser? Claude Code signs you in itself and keeps the sign-in in its docker volume on the runhost, which Agentry never reads; runs there whose model is your Claude subscription then use it.", name, host)
	case modelsignin.ChatGPT:
		return fmt.Sprintf("Sign in with ChatGPT for %s (%s) from your browser, and keep the sign-in's refresh token in the runhost's store, encrypted? Runs there whose model is your ChatGPT plan then use it.", name, host)
	}
	return fmt.Sprintf("Sign in with GitHub for %s (%s), and keep the sign-in's refresh token in the runhost's store, encrypted? Runs there whose model is your Copilot plan then use it.", name, host)
}

func runRunhostModelSignIn(cmd *cobra.Command, name, vendor string, oauthApp, yes bool) error {
	if !signin.IsVendor(vendor) {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("--sign-in %q: a runhost signs in to claude, chatgpt or copilot", vendor))
	}
	home, err := runnerHome()
	if err != nil {
		return err
	}
	recs, err := runhost.LoadRecords(home)
	if err != nil {
		return err
	}
	rec, ok := recs.Find(name)
	if !ok {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("no runhost named %s on this computer (`agentry runhost list`)", name))
	}
	if !yes {
		ask := terminalAsker(cmd)
		if ask == nil {
			return engine.Classify(engine.ExitUsage, errors.New("no one is at a terminal to confirm: pass --yes"))
		}
		ok, err := ask.Confirm(signInConsent(vendor, name, rec.Host), false)
		if err != nil || !ok {
			return errors.Join(err, errors.New("nothing signed in"))
		}
	}
	keys, err := runhost.LoadKeys(name)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
	defer cancel()
	c, err := runhost.DialRunhost(ctx, *rec, keys)
	if err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	defer func() { _ = c.Close() }()
	if _, err := remote.SameVersion(ctx, c, name); err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	app := ""
	if vendor == modelsignin.Copilot && oauthApp {
		app = mcpauth.GitHubOAuthApp
	}
	errOut := narration(cmd)
	p := signin.Person{
		Say: func(s string) { _, _ = fmt.Fprintf(errOut, "%s: %s\n", name, safeTerminalText(s, 4096)) },
		Open: func(u string) {
			if os.Getenv("AGENTRY_NO_BROWSER") != "" {
				return
			}
			if err := openBrowser(u); err != nil {
				_, _ = fmt.Fprintf(errOut, "(could not open a browser: %v; open the address above yourself)\n", err)
			}
		},
		Code: func(page, code string) {
			_, _ = fmt.Fprintf(errOut, "Sign in with GitHub for %s: enter the code %s at\n  %s\n", name, code, page)
			_, _ = fmt.Fprintln(errOut, "Waiting for GitHub (the code lasts 15 minutes)…")
		},
	}
	kept, err := signin.Run(ctx, c, vendor, app, p)
	if err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}
	rec.SignIns = upsertSignIn(rec.SignIns, runhost.SignInRecord{Vendor: kept.Vendor, Label: kept.Label, AddedAt: time.Now().UTC().Format(time.RFC3339)})
	if err := recs.Save(home); err != nil {
		return err
	}
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), map[string]any{"runhost": name, "signIn": kept})
	}
	who := kept.Label
	if who == "" {
		who = "an account the vendor did not name"
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "runhost %s is signed in to %s as %s\n", name, signInLabels[vendor], who)
	if pol, err := devicePolicy(); err == nil && modelsignin.VendorOf(pol.Model.APIKeyKind) != vendor {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  A run uses this computer's model: choose %s with agentry setup for runs on it to use this sign-in.\n", signInLabels[vendor])
	}
	return nil
}

func upsertSignIn(list []runhost.SignInRecord, r runhost.SignInRecord) []runhost.SignInRecord {
	for i := range list {
		if list[i].Vendor == r.Vendor {
			list[i] = r
			return list
		}
	}
	return append(list, r)
}

// removeRunhostSignIn has the runhost end a sign-in, and forgets it in this
// computer's record.
func removeRunhostSignIn(cmd *cobra.Command, name, vendor string) (signin.Removed, error) {
	_, _, c, err := dialRunhostNamed(cmd.Context(), name)
	if err != nil {
		return signin.Removed{}, err
	}
	out, err := remote.RemoveSignIn(cmd.Context(), c, vendor, func(t string) { _, _ = fmt.Fprintln(narration(cmd), t) })
	_ = c.Close()
	if err != nil {
		return signin.Removed{}, engine.Classify(engine.ExitCLI, fmt.Errorf("%s: %w", name, err))
	}
	if home, err := runnerHome(); err == nil {
		if recs, err := runhost.LoadRecords(home); err == nil {
			if rec, ok := recs.Find(name); ok {
				kept := rec.SignIns[:0]
				for _, s := range rec.SignIns {
					if s.Vendor != vendor {
						kept = append(kept, s)
					}
				}
				rec.SignIns = kept
				_ = recs.Save(home)
			}
		}
	}
	return out, nil
}

func printSignInRemoved(w io.Writer, name string, r signin.Removed) {
	if !r.Removed {
		_, _ = fmt.Fprintf(w, "runhost %s keeps no %s sign-in\n", name, r.Vendor)
		return
	}
	_, _ = fmt.Fprintf(w, "runhost %s's %s sign-in is removed: %s\n", name, r.Vendor, r.Outcome)
}
