// agentry agent grant and configure: a persistent agent's roster, the share
// folder, the servers and packs the person adds, its name and personality
// (pkg/runner/persistent).
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
)

// agentConsent asks the person to agree to what a change or a start turns
// on, on the terminal, unless yes says they already have.
func agentConsent(cmd *cobra.Command, a persistent.Agent, ce *persistent.ConsentError, yes bool) error {
	if flags.jsonOut && !yes {
		// A program asks the person itself: it is told what to ask, and
		// nothing changes until it runs the command again with --yes.
		_ = emitJSON(cmd.OutOrStdout(), agreementNeeded{Agent: a.Name, NeedsAgreement: agreementEntries(ce.Entries)})
		return engine.Classify(engine.ExitPolicy, errors.New("this needs the person's agreement to what needsAgreement lists: nothing was changed; run it again with --yes once they agree"))
	}
	errOut := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(errOut, "The %s agent would be given, for the first time on this computer:\n", a.Name)
	for _, e := range ce.Entries {
		_, _ = fmt.Fprintf(errOut, "  - %s\n", safeTerminalText(e.Line, 600))
	}
	if yes {
		return nil
	}
	if !stdinIsTTY(cmd.InOrStdin()) {
		return engine.Classify(engine.ExitPolicy, errors.New("this needs your agreement, and there is no terminal to ask on: pass --yes to agree"))
	}
	_, _ = fmt.Fprint(errOut, "Give it these? [y/N] ")
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return engine.Classify(engine.ExitPolicy, errors.New("not agreed to; nothing was changed"))
	}
	return nil
}

// agreementNeeded is what --json prints when a start or a change needs the
// person's agreement: the entries, each with the sentence to agree to.
type agreementNeeded struct {
	Agent          string           `json:"agent"`
	NeedsAgreement []agreementEntry `json:"needsAgreement"`
}

type agreementEntry struct {
	ID   string `json:"id"`
	Line string `json:"line"`
}

func agreementEntries(es []persistent.RosterEntry) []agreementEntry {
	out := make([]agreementEntry, 0, len(es))
	for _, e := range es {
		out = append(out, agreementEntry{ID: e.ID, Line: e.Line})
	}
	return out
}

// configureAgent applies a change, asking for the person's agreement when
// it turns on what they have not agreed to.
func configureAgent(cmd *cobra.Command, home string, a persistent.Agent, ch persistent.Change, yes bool) (persistent.Roster, bool, error) {
	ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
	defer cancel()
	r, running, err := persistent.Configure(ctx, home, a, persistent.ClientCLI, ch)
	var ce *persistent.ConsentError
	if errors.As(err, &ce) {
		if err := agentConsent(cmd, a, ce, yes); err != nil {
			return nil, false, err
		}
		ch.Consent = true
		r, running, err = persistent.Configure(ctx, home, a, persistent.ClientCLI, ch)
	}
	if err != nil {
		var exit *engine.ClassifiedError
		if errors.As(err, &exit) {
			return nil, false, err
		}
		return nil, false, engine.Classify(engine.ExitUsage, err)
	}
	return r, running, nil
}

// printChanged says what a change did, and when it takes effect.
func printChanged(w io.Writer, a persistent.Agent, r persistent.Roster, running bool) {
	pending := false
	for _, e := range r {
		pending = pending || e.State == persistent.StatePending
	}
	switch {
	case running && pending:
		_, _ = fmt.Fprintf(w, "Saved. The %s agent restarts with the change at its next turn, and its conversation continues.\n", a.Name)
	case running:
		_, _ = fmt.Fprintf(w, "Saved. The %s agent takes it at its next turn.\n", a.Name)
	default:
		_, _ = fmt.Fprintf(w, "Saved. The %s agent takes it when it next starts.\n", a.Name)
	}
	printRoster(w, r)
}

// printRoster prints the roster, an entry a line.
func printRoster(w io.Writer, r persistent.Roster) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  ENTRY\tSTATE\tBY\tWHAT")
	for _, e := range r {
		state := string(e.State)
		if e.Fixed {
			state += " (always)"
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", e.ID, state, e.GrantedBy, safeTerminalText(e.Line, 300))
		if e.State == persistent.StateFailed && e.Reason != "" {
			_, _ = fmt.Fprintf(tw, "  \t\t\twhy it failed: %s\n", safeTerminalText(e.Reason, 300))
		}
		if e.State == persistent.StateNeedsSetUp && e.Reason != "" {
			_, _ = fmt.Fprintf(tw, "  \t\t\twaiting: %s\n", safeTerminalText(e.Reason, 300))
		}
		if e.ShareSwitch != "" {
			sees := "no"
			if e.Share {
				sees = "yes, read only, and writes new files into it"
			}
			_, _ = fmt.Fprintf(tw, "  \t\t\tsees your share folder: %s (switch %s)\n", sees, e.ShareSwitch)
		}
	}
	_ = tw.Flush()
}

func newAgentGrantCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "grant <name> <entry> on|off",
		Short: "Switch an entry of a persistent agent's roster on or off; a running agent takes it at its next turn",
		Long: "Each thing a persistent agent has is an entry of its roster: its share folder,\n" +
			"each knowledge pack and each MCP server you added. `agentry agent show <name>`\n" +
			"lists them with their ids. Switching one on that you have not agreed to on this\n" +
			"computer asks first; --yes agrees without asking. A running agent ends its\n" +
			"segment at its next turn, starts the next with the change and continues the\n" +
			"conversation. Switching a server whose install failed, off and on, installs\n" +
			"it again.\n\n" +
			"The Assistant's heartbeat entry, off until you switch it on, fires the tasks\n" +
			"it registers (`agentry agent tasks assistant`); switching it takes effect at\n" +
			"once and restarts nothing.\n\n" +
			"Each secret you stored is an entry, secret:<NAME> (`agentry agent secret`);\n" +
			"switched off, it is withheld: a server that declares it is given an empty\n" +
			"value, and the Coding agent's programs do not have it.\n\n" +
			"A package server with no network has a second switch, <entry>:share, that\n" +
			"lets it see the agent's share folder, read only, at /agent/share, so its\n" +
			"tools can open a file you put there. For the Assistant it sees a copy of\n" +
			"each file the share's check read (one ClamAV found is left out; with the\n" +
			"malware check off every file is read unchecked); for the Coding agent, the\n" +
			"folder as it is, unchecked. The server saves a file for the share into\n" +
			"/agent/share-out; after each turn the runner copies it into the share as a\n" +
			"new file, \"<name> (2)\" when the name is taken, never over a file you\n" +
			"have. Off by default; switching it on asks again, since the server's\n" +
			"sentence changes. A remote server, and one that reaches the network, are\n" +
			"never given it.\n\n" +
			"  agentry agent grant assistant share off\n" +
			"  agentry agent grant assistant heartbeat on\n" +
			"  agentry agent grant assistant knowledge:my-notes on\n" +
			"  agentry agent grant coder secret:GITHUB_TOKEN off\n" +
			"  agentry agent grant assistant server:pypi-office-word-mcp-server:share on",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := cliAgent(args[0])
			if err != nil {
				return err
			}
			var on bool
			switch args[2] {
			case "on":
				on = true
			case "off":
			default:
				return engine.Classify(engine.ExitUsage, fmt.Errorf("%q is neither on nor off", args[2]))
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			r, running, err := configureAgent(cmd, home, a, persistent.Change{Switch: &persistent.Switch{ID: args[1], On: on}}, yes)
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), r)
			}
			printChanged(cmd.OutOrStdout(), a, r, running)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "agree to what the switch turns on without being asked")
	return cmd
}

func newAgentConfigureCmd() *cobra.Command {
	var (
		name, personalityFile, share, runOnStart, addServer, removeServer, addPack, removePack string
		devHost, devKey, devPassphrase, devPassword, policyPath, addPort, removePort           string
		yes, allowVulnerable, noDevHost                                                        bool
		acct                                                                                   accountFlags
		noAccount, addSuggested, googleClient, googleSignIn                                    string
		mailOn, mailOff                                                                        []string
	)
	cmd := &cobra.Command{
		Use:   "configure <name>",
		Short: "Change a persistent agent: its name and personality, its share folder, the servers and packs it has",
		Long: "Configure changes what the agent is and what it has. A name and a personality\n" +
			"reach the agent at its next turn. A share folder, a server or a pack changes\n" +
			"its roster: a running agent restarts with it at its next turn, and its\n" +
			"conversation continues. Adding a server or a pack, or choosing another share\n" +
			"folder, asks for your agreement first; --yes agrees without asking.\n\n" +
			"--add-server takes a file holding one MCP server in a QuickAgent manifest's\n" +
			"shape (npm or pypi with a version, or url with auth none or secret; egress,\n" +
			"tools, secrets and env as a manifest declares them). A secret the server\n" +
			"declares is given the value of the agent's stored secret of that name\n" +
			"(`agentry agent secret`), or none while that secret is switched off; without\n" +
			"one, it is read from your runner policy's secret source when the agent starts.\n\n" +
			"A package server (npm or pypi) installs in the background: a running agent\n" +
			"keeps answering without it, `show` lists it as installing, then pending, and\n" +
			"it joins at the first turn after it is ready. One whose install fails is\n" +
			"failed, with why; the agent runs on without it, and switching it off and on\n" +
			"(agentry agent grant) installs it again. A start leaves out a server not yet\n" +
			"installed and installs it the same way.\n\n" +
			"  agentry agent configure assistant --name Sandra --personality-file sandra.md\n" +
			"  agentry agent configure assistant --share ~/Documents/assistant\n" +
			"  agentry agent configure assistant --knowledge my-notes\n" +
			"  agentry agent configure assistant --add-server server.yaml\n\n" +
			"--add-suggested adds a server the agent suggests (agentry agent show lists\n" +
			"them) as one of your own, the same way. The servers an agent comes with\n" +
			"are entries of its roster from the start, switched with agentry agent\n" +
			"grant; one that needs a key or an account waits, off, until it has one.\n\n" +
			"  agentry agent configure assistant --add-suggested weather\n\n" +
			"--dev-host gives the Coding agent a dev server to deploy to over SSH. It is\n" +
			"readied here: its host key is shown for you to accept, then pinned, and its\n" +
			"sign-in (--dev-key, --dev-password, or a prompt) is saved in your system\n" +
			"keychain, where the agent's later starts read it.\n\n" +
			"  agentry agent configure coder --dev-host deploy@devbox.lan --dev-key ~/.ssh/id_ed25519\n\n" +
			"--port publishes one of the Coding agent's ports on this computer, at\n" +
			"127.0.0.1 and the same number, so what it serves from its sandbox (a dev\n" +
			"server it started) opens in your browser; --no-port takes one away by its\n" +
			"name. A port is a roster entry, switched with agentry agent grant.\n\n" +
			"  agentry agent configure coder --port web=3000\n\n" + agentAccountLong + agentMailLong + agentGoogleLong,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := cliAgent(args[0])
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			if len(mailOn) > 0 || len(mailOff) > 0 {
				if cmd.Flags().NFlag() > countFlags(cmd, "mail-provider", "no-mail-provider") {
					return engine.Classify(engine.ExitUsage, errors.New("--mail-provider and --no-mail-provider go with each other alone"))
				}
				return configureMailProviders(cmd, a, home, mailOn, mailOff)
			}
			if googleClient != "" || googleSignIn != "" {
				if cmd.Flags().NFlag() > countFlags(cmd, "google-client", "google-sign-in", "policy") {
					return engine.Classify(engine.ExitUsage, errors.New("--google-client and --google-sign-in go with --policy alone"))
				}
				return configureGoogle(cmd, a, home, policyPath, googleClient, googleSignIn)
			}
			var ch persistent.Change
			set := 0
			if cmd.Flags().Changed("name") {
				ch.Name, set = &name, set+1
			}
			if personalityFile != "" {
				b, err := readBounded(personalityFile, persistent.MaxPersonalityBytes)
				if err != nil {
					return engine.Classify(engine.ExitUsage, fmt.Errorf("--personality-file: %w", err))
				}
				text := string(b)
				ch.Personality, set = &text, set+1
			}
			if cmd.Flags().Changed("share") {
				dir := share
				if dir != "" {
					if dir, err = filepath.Abs(expandHome(dir)); err != nil {
						return engine.Classify(engine.ExitUsage, err)
					}
				}
				ch.Share, set = &dir, set+1
			}
			if runOnStart != "" {
				v, err := strconv.ParseBool(runOnStart)
				if err != nil {
					return engine.Classify(engine.ExitUsage, fmt.Errorf("--run-on-start %q is neither true nor false", runOnStart))
				}
				ch.RunOnStart, set = &v, set+1
			}
			if addServer != "" {
				srv, err := readServerFile(addServer)
				if err != nil {
					return engine.Classify(engine.ExitUsage, fmt.Errorf("--add-server: %w", err))
				}
				if err := checkAgentServerAdvisories(cmd, srv, allowVulnerable); err != nil {
					return err
				}
				ch.AddServer, set = &srv, set+1
			}
			if addSuggested != "" {
				if addServer != "" {
					return engine.Classify(engine.ExitUsage, errors.New("--add-server and --add-suggested each add one server: pass one"))
				}
				srv, err := persistent.Suggested(a, addSuggested)
				if err != nil {
					return engine.Classify(engine.ExitUsage, fmt.Errorf("--add-suggested: %w", err))
				}
				if err := checkAgentServerAdvisories(cmd, srv, allowVulnerable); err != nil {
					return err
				}
				ch.AddServer, set = &srv, set+1
			}
			if removeServer != "" {
				ch.RemoveServer, set = removeServer, set+1
			}
			if addPack != "" {
				if !keptPackNamed(cmd, addPack) {
					return engine.Classify(engine.ExitUsage, fmt.Errorf("--knowledge %s: no pack of that name is kept on this computer (agentry knowledge list)", addPack))
				}
				ch.AddKnowledge, set = addPack, set+1
			}
			if removePack != "" {
				ch.RemoveKnowledge, set = removePack, set+1
			}
			if noDevHost {
				ch.ClearDevHost, set = true, set+1
			}
			if devHost != "" {
				if noDevHost {
					return engine.Classify(engine.ExitUsage, errors.New("--dev-host and --no-dev-host are each other's opposite"))
				}
				if !a.Builtin.Codes() {
					return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent has no dev server: the Coding agent alone is given one", a.Name))
				}
				d, err := readyDevHost(cmd, home, policyPath, devHost, devKey, devPassphrase, devPassword)
				if err != nil {
					return err
				}
				ch.DevHost, set = d, set+1
			} else if devKey != "" || devPassphrase != "" || devPassword != "" {
				return engine.Classify(engine.ExitUsage, errors.New("--dev-key, --dev-passphrase and --dev-password go with --dev-host"))
			}
			if addPort != "" {
				portName, number, ok := strings.Cut(addPort, "=")
				n, err := strconv.Atoi(number)
				if !ok || err != nil {
					return engine.Classify(engine.ExitUsage, fmt.Errorf("--port %q is not name=number (web=3000)", addPort))
				}
				ch.AddPort, set = &persistent.Port{Name: portName, Port: n}, set+1
			}
			if removePort != "" {
				ch.RemovePort, set = removePort, set+1
			}
			if noAccount != "" {
				if acct.given() {
					return engine.Classify(engine.ExitUsage, errors.New("--no-account takes an account away; the other --account flags set one up"))
				}
				ch.RemoveAccount, set = strings.ToLower(strings.TrimSpace(noAccount)), set+1
			}
			if acct.host == "" && acct.given() {
				return engine.Classify(engine.ExitUsage, errors.New("--own, --yours and the --account-* flags go with --account"))
			}
			// The account is readied last, after every other flag is
			// checked, since it asks for its token.
			var accountToken string
			var cfgBefore persistent.Config
			if acct.host != "" || noAccount != "" {
				if cfgBefore, err = persistent.LoadConfig(home, a.Name); err != nil {
					return engine.Classify(engine.ExitCLI, err)
				}
			}
			if set == 0 && acct.host == "" {
				return engine.Classify(engine.ExitUsage, errors.New("nothing to change: pass one of the flags (agentry agent configure --help)"))
			}
			if acct.host != "" {
				g, token, err := readyAccount(cmd, a, cfgBefore, acct)
				if err != nil {
					return err
				}
				ch.SetAccount, accountToken = &g, token
			}
			undoToken := func() {}
			if g := ch.SetAccount; g != nil {
				if undoToken, err = keepAccountToken(cfgBefore, *g, accountToken); err != nil {
					return err
				}
			}
			var oldDev *persistent.DevHost
			if noDevHost {
				if c, err := persistent.LoadConfig(home, a.Name); err == nil {
					oldDev = c.DevHost
				}
			}
			r, running, err := configureAgent(cmd, home, a, ch, yes)
			if err != nil {
				undoToken()
				return err
			}
			dropAccountTokens(cmd, cfgBefore, ch)
			if oldDev != nil {
				// A schedule of the same host and account reads the same
				// keychain entry, so it is left for the person to remove.
				if da, err := targethost.ParseAnswer(oldDev.Host); err == nil {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "The dev server's sign-in stays in your system keychain (account %s), where a schedule of the same host may read it.\n", da.Account())
				}
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), r)
			}
			printChanged(cmd.OutOrStdout(), a, r, running)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "the name the agent goes by (empty clears it)")
	f.StringVar(&personalityFile, "personality-file", "", fmt.Sprintf("a text file describing the personality you want it to have (at most %d KiB)", persistent.MaxPersonalityBytes>>10))
	f.StringVar(&share, "share", "", "the folder it shares with you, read and write (empty: ~/agentry/<agent>share)")
	f.StringVar(&runOnStart, "run-on-start", "", "true or false: start it when Agentry Desktop starts")
	f.StringVar(&addServer, "add-server", "", "a file holding an MCP server to add, in a manifest's shape")
	f.StringVar(&removeServer, "remove-server", "", "the roster id of a server to remove (server:…)")
	f.StringVar(&addSuggested, "add-suggested", "", "add a server the agent suggests, by its key, as your own (agentry agent show lists them)")
	f.StringArrayVar(&mailOn, "mail-provider", nil, "the Assistant: enable this mail provider (google), putting its servers on its roster; repeatable")
	f.StringArrayVar(&mailOff, "no-mail-provider", nil, "the Assistant: disable this mail provider, taking its servers off its roster and keeping its sign-in; repeatable")
	f.StringVar(&googleClient, "google-client", "", "the Assistant's Google Workspace: your Google Cloud OAuth client's JSON file (a Desktop app client)")
	f.StringVar(&googleSignIn, "google-sign-in", "", "the Assistant's Google Workspace: sign it in to this Google account, in your browser")
	f.StringVar(&addPack, "knowledge", "", "a knowledge pack kept on this computer for it to search")
	f.StringVar(&removePack, "no-knowledge", "", "a knowledge pack to take away")
	f.StringVar(&addPort, "port", "", "publish one of the Coding agent's ports on this computer, as name=number (web=3000)")
	f.StringVar(&removePort, "no-port", "", "take away the Coding agent's port of this name")
	f.StringVar(&devHost, "dev-host", "", "the Coding agent's dev server, user@host[:port], readied here and kept")
	f.StringVar(&devKey, "dev-key", "", "sign in to the dev server with this private key FILE (its path is kept in your keychain)")
	f.StringVar(&devPassphrase, "dev-passphrase", "", "the name of an environment variable holding the key's passphrase")
	f.StringVar(&devPassword, "dev-password", "", "sign in to the dev server with a password: the name of an environment variable holding it")
	f.BoolVar(&noDevHost, "no-dev-host", false, "take the Coding agent's dev server away")
	f.StringVar(&policyPath, "policy", "", "operator policy file whose host lists the dev server is held to (default ~/.agentry/runner/policy.yaml)")
	f.BoolVar(&yes, "yes", false, "agree to what the change turns on without being asked")
	f.BoolVar(&allowVulnerable, "allow-vulnerable-mcp", false, "add a server version the registry has a high or critical advisory for")
	f.StringVar(&acct.host, "account", "", "give the Coding agent an account on this git host (github.com, gitlab.com, git.example.com), or set its token again")
	f.BoolVar(&acct.own, "own", false, "with --account: an account of the agent's own, which you share repositories with (recommended)")
	f.BoolVar(&acct.yours, "yours", false, "with --account: your own account, which lets the agent do whatever its token can")
	f.StringVar(&acct.user, "account-user", "", "with --account: the account's user name on the host")
	f.StringVar(&acct.email, "account-email", "", "with --account: the email its commits carry")
	f.StringVar(&acct.name, "account-name", "", "with --account: the name its commits carry (default: the user name)")
	f.StringVar(&acct.kind, "account-kind", "", "with --account: github, gitlab, gitea or git, for a host whose kind is not known")
	f.StringVar(&noAccount, "no-account", "", "take away the Coding agent's account on this git host, and its token")
	return cmd
}

// dropAccountTokens removes from the keychain the tokens a change that
// succeeded left behind: a removed account's, and an account's earlier
// user's when its user name changed. The change stands either way, so one
// that cannot be removed is named for the person to remove.
func dropAccountTokens(cmd *cobra.Command, before persistent.Config, ch persistent.Change) {
	var accounts []string
	if h := ch.RemoveAccount; h != "" {
		for _, g := range before.Accounts {
			if g.Host == h {
				accounts = append(accounts, persistent.AccountKeychainAccount(g.Host, g.User))
			}
		}
	}
	if g := ch.SetAccount; g != nil {
		if old := replacedAccountToken(before, *g); old != "" {
			accounts = append(accounts, old)
		}
	}
	for _, keychainAccount := range accounts {
		if err := agentSecrets.Delete(keychainAccount); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "The token stays in your system keychain (account %s, service %s): %v\n", keychainAccount, modelkey.Service, err)
		}
	}
}

// readBounded reads a file of at most max bytes.
func readBounded(path string, max int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // a file the person names
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d KiB", path, max>>10)
	}
	return b, nil
}

// readServerFile reads one MCP server in a manifest's shape.
func readServerFile(path string) (manifest.MCPServer, error) {
	var srv manifest.MCPServer
	b, err := readBounded(path, 64<<10)
	if err != nil {
		return srv, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&srv); err != nil {
		return srv, fmt.Errorf("%s is not an MCP server in a manifest's shape: %w", path, err)
	}
	return srv, nil
}

// checkAgentServerAdvisories asks the registry about a package server
// before it is added, as a run asks about a release's: a version with a
// high or critical advisory is refused unless the flag or the policy allows
// it. A registry that cannot answer refuses nothing.
func checkAgentServerAdvisories(cmd *cobra.Command, srv manifest.MCPServer, allow bool) error {
	if srv.IsRemote() {
		return nil
	}
	statuses := mcpinstall.CheckVulnerabilities(cmd.Context(), nil, registryURL(), []manifest.MCPServer{srv})
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "the MCP server, per the registry: %s\n", mcpinstall.Summary(statuses))
	pol, _, _, err := agentPolicy("")
	allowPolicy := err == nil && pol.MCPServers.AllowVulnerable
	if _, err := vulnerabilityVerdict(statuses, registryURL(), allow, allowPolicy); err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}
	return nil
}

// agentPacks opens the knowledge packs a segment searches from the local
// store, each into a private folder its done removes.
func agentPacks(cmd *cobra.Command) persistent.Packs {
	return func(ctx context.Context, names []string) ([]engine.AttachedPack, func(), error) {
		var cleanups []func()
		done := func() {
			for _, f := range cleanups {
				f()
			}
		}
		s, err := knowledgeStore(cmd)
		if err != nil {
			return nil, done, err
		}
		out := make([]engine.AttachedPack, 0, len(names))
		for _, name := range names {
			p, cleanup, err := s.OpenScratch(ctx, name)
			if cleanup != nil {
				cleanups = append(cleanups, cleanup)
			}
			if err != nil {
				done()
				return nil, func() {}, fmt.Errorf("the pack %s: %w", name, err)
			}
			out = append(out, engine.AttachPack(p))
		}
		return out, done, nil
	}
}

// readyDevHost readies the Coding agent's dev server at the terminal, as a
// schedule's target host is readied when it is made: the address under the
// policy's host lists, the host key shown for the person to accept (and
// kept in Agentry's known hosts), and the sign-in from the flags or a
// prompt, saved in the keychain where the agent's supervisor reads it. It
// answers the server as config.yaml keeps it, its key pinned.
func readyDevHost(cmd *cobra.Command, home, policyPath, host, key, passphrase, password string) (*persistent.DevHost, error) {
	pol, _, _, err := agentPolicy(policyPath)
	if err != nil {
		return nil, err
	}
	a, err := targethost.ParseAnswer(host)
	if err != nil {
		return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("--dev-host: %w", err))
	}
	id := spec.DevHostID
	with := func(v string) []string {
		if v == "" {
			return nil
		}
		return []string{id + "=" + v}
	}
	if key != "" {
		if key, err = filepath.Abs(expandHome(key)); err != nil {
			return nil, engine.Classify(engine.ExitUsage, err)
		}
	}
	flags, err := targethost.ParseFlags(with(key), with(passphrase), with(password), nil)
	if err != nil {
		return nil, engine.Classify(engine.ExitUsage, err)
	}
	var ask targethost.Asker
	if stdinIsTTY(cmd.InOrStdin()) {
		ask = &setup.TTYPrompter{In: cmd.InOrStdin(), Err: cmd.ErrOrStderr()}
	}
	say := func(msg string) { _, _ = fmt.Fprintln(cmd.ErrOrStderr(), msg) }
	ready, err := prepareTargetHost(cmd.Context(), id, a, targethost.Options{
		Hosts: pol.Hosts, KnownHosts: targethost.DefaultKnownHostsFiles(home), Flags: flags,
		Getenv: os.Getenv, Ask: ask, Say: say, Keep: true,
	})
	if err != nil {
		var r *targethost.Refusal
		if errors.As(err, &r) {
			return nil, engine.Classify(engine.ExitPolicy, err)
		}
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	say(fmt.Sprintf("dev server: %s at %s, host key %s (%s); its sign-in is saved in your keychain", ready.Answer, ready.Address, ready.HostKey.Fingerprint(), ready.HostKey.From))
	return &persistent.DevHost{Host: ready.Answer.String(), HostKey: ssh.FingerprintSHA256(ready.HostKey.Key)}, nil
}

// countFlags counts the named flags the command line set.
func countFlags(cmd *cobra.Command, names ...string) int {
	n := 0
	for _, name := range names {
		if cmd.Flags().Changed(name) {
			n++
		}
	}
	return n
}
