// agentry agent messenger: the messengers a persistent agent is reached on,
// each an account the person makes on the service and pairs with their own
// (pkg/runner/persistent).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/messenger"
)

const agentMessengerLong = "A messenger lets you talk to a persistent agent from a messaging app, away\n" +
	"from this computer: each message you send there becomes a turn of the\n" +
	"agent's conversation, as in Agentry, and its answer comes back there. The\n" +
	"Assistant, the Coding agent and the persistent agents you install can each\n" +
	"have one of each kind; the agent must be running (agentry agent start) to\n" +
	"answer.\n\n" +
	"Telegram, through a bot of your own:\n" +
	"  1. In Telegram, open @BotFather, send /newbot and choose a name and a\n" +
	"     username ending in \"bot\". BotFather answers with the bot's token.\n" +
	"  2. agentry agent messenger add assistant telegram, and paste the token.\n\n" +
	"Discord, through a bot of your own:\n" +
	"  1. In Discord's developer portal (discord.com/developers), make an\n" +
	"     application; on its Bot page, reset and copy the bot's token.\n" +
	"  2. On its Installation page, add the bot to a server of yours (scope\n" +
	"     bot, no permission), so you can open a direct message with it.\n" +
	"  3. agentry agent messenger add assistant discord, and paste the token.\n\n" +
	"Slack, through an app of your own in your workspace:\n" +
	"  1. agentry agent messenger manifest slack prints an app manifest. At\n" +
	"     api.slack.com/apps, create an app from it and install it.\n" +
	"  2. Copy the bot's token (xoxb-, OAuth & Permissions) and make an\n" +
	"     app-level token with connections:write (xapp-, Basic Information).\n" +
	"  3. agentry agent messenger add assistant slack, and paste both.\n\n" +
	"Matrix, through an account of its own on any homeserver:\n" +
	"  1. Make an account for the agent, on your homeserver or another.\n" +
	"  2. agentry agent messenger add assistant matrix --homeserver\n" +
	"     https://matrix.example.org --user @agent:example.org, and type its\n" +
	"     password: Agentry signs in once and keeps the session's access\n" +
	"     token, never the password. Without --user, paste an access token.\n" +
	"  3. Start a direct chat with that account, with encryption off: the\n" +
	"     agent's account holds no encryption keys, so it cannot read an\n" +
	"     encrypted chat, and your homeserver can read this one. The\n" +
	"     homeserver must be on a public address: the messenger's container\n" +
	"     reaches public addresses alone.\n\n" +
	"Then pair it: agentry agent messenger pair assistant <messenger> shows a\n" +
	"code; send it to the account from your own within 10 minutes. Each\n" +
	"credential is checked with its service, kept in your system keychain, and\n" +
	"adding it asks you to agree to what the service carries.\n\n" +
	"Only the account that sent the code reaches the agent; anyone else who\n" +
	"writes to it is ignored and never answered, and so is every group, server\n" +
	"channel and Slack channel. None of these services encrypts a bot's\n" +
	"conversation end to end, so each can read it. Send /stop to stop what the\n" +
	"agent is doing. Nothing is approved from a messenger: what the agent asks\n" +
	"your agreement to waits for you in Agentry. A message sent while the agent\n" +
	"answers another waits, and the waiting ones go together as its next turn.\n" +
	"One account reaches one agent: give each agent its own.\n\n" +
	"An agent on a remote runhost keeps its messengers connected while this\n" +
	"computer is off: add, pair, list, test and remove it there with --on\n" +
	"<runhost>. The credential is read and agreed to here, and kept in the\n" +
	"runhost's store, encrypted; a Matrix password signs in here, and the\n" +
	"runhost is given the session's access token alone.\n\n" +
	"The credential stays in your keychain; Agentry hands it to one container of\n" +
	"its own alone, which reaches that service's hosts and nothing else, and the\n" +
	"agent is never given it. A messenger is a roster entry, switched with\n" +
	"agentry agent grant <name> messenger:<messenger> on|off, which starts or\n" +
	"stops it at once. The runner policy's allowMessengers and messengerKinds\n" +
	"can refuse messengers on this computer.\n\n" +
	"  agentry agent messenger add assistant telegram < token.txt\n" +
	"  agentry agent messenger pair assistant telegram\n" +
	"  agentry agent messenger list assistant\n" +
	"  agentry agent messenger test assistant telegram\n" +
	"  agentry agent messenger remove assistant telegram\n" +
	"  agentry agent messenger add assistant telegram --on lab\n" +
	"  agentry agent messenger manifest slack"

// messengerAPI is the messengers' address for add and test: the service's
// own, but in a test.
var messengerAPI = ""

func newAgentMessengerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "messenger",
		Short: "Add, pair, list, test and remove the messengers a persistent agent is reached on",
		Long:  agentMessengerLong,
	}
	cmd.AddCommand(newAgentMessengerAddCmd(), newAgentMessengerPairCmd(), newAgentMessengerListCmd(), newAgentMessengerTestCmd(), newAgentMessengerRemoveCmd(), newAgentMessengerManifestCmd())
	return cmd
}

// messengerArgs are the agent and the messenger kind two arguments name.
func messengerArgs(agent, kind string) (persistent.Agent, messenger.Kind, error) {
	a, err := cliAgent(agent)
	if err != nil {
		return a, messenger.Kind{}, err
	}
	if !a.TakesMessengers() {
		return a, messenger.Kind{}, engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent takes no messenger", a.Name))
	}
	k, ok := messenger.Lookup(kind)
	if !ok {
		return a, k, engine.Classify(engine.ExitUsage, fmt.Errorf("%q is not a messenger Agentry knows (%s)", kind, strings.Join(messenger.Names(), ", ")))
	}
	return a, k, nil
}

// homeMessengerAllowed is the runner home's policy's judgement of a
// messenger, allowing it where the policy cannot be read, as a new home has
// none.
func homeMessengerAllowed(home string) func(string) (bool, string) {
	pol, _, err := policy.Load(policy.PathIn(home))
	if err != nil {
		pol = policy.Default()
	}
	return managed.Hold(pol).MessengerAllowed
}

// checkHomeserver is a Matrix account's homeserver as add keeps it, refused
// unless it is a public https:// address.
func checkHomeserver(server string) (string, error) {
	if _, err := messenger.ServerHost(server); err != nil || !strings.HasPrefix(server, "https://") {
		if err == nil {
			err = fmt.Errorf("%q is not https://: a homeserver is reached over TLS", server)
		}
		return "", fmt.Errorf("--homeserver: %w", err)
	}
	return strings.TrimRight(server, "/"), nil
}

// readMessengerCredential reads an account's credential the way its kind
// gives it: a Slack app's two tokens, a Matrix account's access token or,
// with user, its password, which signs in once on server and is never kept;
// any other kind's one token.
func readMessengerCredential(cmd *cobra.Command, k messenger.Kind, server, user string) (string, error) {
	switch {
	case k.Name == messenger.KindSlack:
		in := cmd.InOrStdin()
		if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			app, err := readSecretValue(cmd, "Slack app-level token (xapp-)", "the app-level token")
			if err != nil {
				return "", err
			}
			bot, err := readSecretValue(cmd, "Slack bot token (xoxb-)", "the bot's token")
			if err != nil {
				return "", err
			}
			return app + " " + bot, nil
		}
		// From a file or a pipe: both tokens, on one line or two.
		b, err := io.ReadAll(io.LimitReader(in, persistent.MaxSecretBytes+3))
		if err != nil {
			return "", fmt.Errorf("read the tokens from stdin: %w", err)
		}
		app, bot, err := messenger.SlackTokens(string(b))
		if err != nil {
			return "", err
		}
		return app + " " + bot, nil
	case k.Name == messenger.KindMatrix && user != "":
		password, err := readSecretValue(cmd, "Password of "+user, "the password")
		if err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
		defer cancel()
		api := server
		if messengerAPI != "" {
			api = messengerAPI
		}
		return messenger.MatrixLogin(ctx, api, user, password)
	}
	return readSecretValue(cmd, k.Title+" "+k.Account+" token", "the "+k.Account+"'s token")
}

func newAgentMessengerAddCmd() *cobra.Command {
	var yes bool
	var server, user string
	cmd := &cobra.Command{
		Use:   "add <name> <messenger>",
		Short: "Add a messenger account to a persistent agent, or set its credential again",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, k, err := messengerArgs(args[0], args[1])
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			if ok, why := homeMessengerAllowed(home)(k.Name); !ok {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("%s: %s", k.Title, why))
			}
			if k.NamesServer {
				if server, err = checkHomeserver(server); err != nil {
					return engine.Classify(engine.ExitUsage, err)
				}
			} else if server != "" || user != "" {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("--homeserver and --user are for a Matrix account, not %s", k.Title))
			}
			token, err := readMessengerCredential(cmd, k, server, user)
			if err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			if err := messenger.CheckCredentialShape(k.Name, token); err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			if err := persistent.CheckSecretValue(token); err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			ready, err := messenger.Check(ctx, messenger.Credential{Kind: k.Name, Token: token, Server: server}, messengerAPI)
			if err != nil {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("%s did not take the credential: %w", k.Title, err))
			}
			if other := persistent.MessengerAccountOf(home, k.Name, ready.Account); other != "" && other != a.Name {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("%s is the %s agent's %s %s: one %s reaches one agent, so make another for this one", ready.Name, other, k.Title, k.Account, k.Account))
			}
			account := persistent.MessengerKeychainAccount(a.Name, k.Name)
			previous, _ := agentSecrets.Get(account)
			if err := agentSecrets.Set(account, token); err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			m := persistent.MessengerAccount{Kind: k.Name, Account: ready.Account, Name: ready.Name, Server: server, Set: time.Now().UTC()}
			r, running, err := configureAgent(cmd, home, a, persistent.Change{SetMessenger: &m}, yes)
			if err != nil {
				if previous != "" {
					_ = agentSecrets.Set(account, previous)
				} else {
					_ = agentSecrets.Delete(account)
				}
				return err
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), struct {
					Agent   string                      `json:"agent"`
					Account persistent.MessengerAccount `json:"messenger"`
				}{a.Name, m})
			}
			w := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(w, "%s took the credential of %s, now kept in %s.\n", k.Title, safeTerminalText(ready.Name, 80), persistent.KeptIn)
			printChangedMessenger(w, a, r, running)
			_, _ = fmt.Fprintf(w, "Next, pair your own %s account: agentry agent messenger pair %s %s%s\n", k.Title, a.Name, k.Name, onThisRunhost())
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "agree to what the messenger carries without being asked")
	cmd.Flags().StringVar(&server, "homeserver", "", "a Matrix account's homeserver, as https://matrix.example.org")
	cmd.Flags().StringVar(&user, "user", "", "a Matrix account's user id, to sign in with its password instead of pasting an access token")
	return cmd
}

// pairHint says where the person sends the pairing code from.
func pairHint(k messenger.Kind, acct persistent.MessengerAccount) string {
	name := safeTerminalText(acct.Name, 80)
	switch k.Name {
	case messenger.KindDiscord:
		return "From your own Discord account, open a direct message with " + name + " (from its profile, in a server you share) and send"
	case messenger.KindSlack:
		return "In Slack, open " + name + " under Apps, and in its Messages tab send"
	case messenger.KindMatrix:
		return "From your own Matrix account, start a direct chat with " + name + ", with encryption off, and send"
	}
	return "From your own " + k.Title + " account, send " + name
}

// removeHint says how the account itself is deleted on its service.
func removeHint(k messenger.Kind) string {
	switch k.Name {
	case messenger.KindTelegram:
		return " (BotFather's /deletebot)"
	case messenger.KindDiscord:
		return " (the application, in the developer portal)"
	case messenger.KindSlack:
		return " (the app, at api.slack.com/apps)"
	case messenger.KindMatrix:
		return " (its session too, in your Matrix client's sessions)"
	}
	return ""
}

func newAgentMessengerManifestCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "manifest slack",
		Short: "Print the Slack app manifest to create a messenger's app from",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !strings.EqualFold(args[0], messenger.KindSlack) {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("only Slack's app is made from a manifest; %q needs none", args[0]))
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), messenger.SlackManifest(name))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "Agentry", "the app's name, as Slack shows it")
	return cmd
}

// printChangedMessenger says when a messenger change takes effect: at once
// for a running agent, which starts or stops its messenger without a
// restart.
func printChangedMessenger(w io.Writer, a persistent.Agent, r persistent.Roster, running bool) {
	if running {
		_, _ = fmt.Fprintf(w, "Saved. The %s agent takes it now.\n", a.Name)
	} else {
		_, _ = fmt.Fprintf(w, "Saved. The %s agent takes it when it next starts (agentry agent start %s%s).\n", a.Name, a.Name, onThisRunhost())
	}
	printRoster(w, r)
}

func newAgentMessengerPairCmd() *cobra.Command {
	var noWait bool
	cmd := &cobra.Command{
		Use:   "pair <name> <messenger>",
		Short: "Show a code to send from your own messenger account, which pairs it with the agent",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, k, err := messengerArgs(args[0], args[1])
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			c, err := persistent.LoadConfig(home, a.Name)
			if err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			acct, ok := persistent.MessengerOf(c, k.Name)
			if !ok {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent has no %s messenger: agentry agent messenger add %s %s%s", a.Name, k.Title, a.Name, k.Name, onThisRunhost()))
			}
			since := time.Now().UTC()
			code, p, err := persistent.NewPairingCode(since)
			if err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			_, running, err := configureAgent(cmd, home, a, persistent.PairingChangeFor(k.Name, p), false)
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), struct {
					Agent   string    `json:"agent"`
					Kind    string    `json:"messenger"`
					Code    string    `json:"code"`
					Until   time.Time `json:"until"`
					Running bool      `json:"running"`
				}{a.Name, k.Name, code, p.Until, running})
			}
			w := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(w, "%s this code before %s:\n\n    %s\n\n",
				pairHint(k, acct), p.Until.Local().Format("15:04"), persistent.FormatPairingCode(code))
			if !running {
				_, _ = fmt.Fprintf(w, "The %s agent is not running, and takes the code only while it runs: start it now (agentry agent start %s%s).\n", a.Name, a.Name, onThisRunhost())
			}
			if acct.Peer != nil {
				_, _ = fmt.Fprintln(w, "The account paired now stays paired until another sends the code.")
			}
			if noWait {
				return nil
			}
			_, _ = fmt.Fprintln(w, "Waiting for it…")
			for time.Now().Before(p.Until) {
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				case <-time.After(2 * time.Second):
				}
				c, err := persistent.LoadConfig(home, a.Name)
				if err != nil {
					continue
				}
				for _, m := range c.Messengers {
					if m.Kind == k.Name && m.Peer != nil && !m.Peer.Paired.Before(since) {
						_, _ = fmt.Fprintf(w, "Paired: your %s account reaches the %s agent from now on.\n", k.Title, a.Name)
						return nil
					}
				}
			}
			return engine.Classify(engine.ExitUsage, fmt.Errorf("the code expired before %s sent it: run agentry agent messenger pair %s %s%s again", k.Title, a.Name, k.Name, onThisRunhost()))
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "show the code and return, without waiting for it to be sent")
	return cmd
}

func newAgentMessengerListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <name>",
		Short: "List a persistent agent's messengers, whether each is paired, and its connection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := cliAgent(args[0])
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			st, err := readAgentState(cmd.Context(), home, a)
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), struct {
					Agent      string                      `json:"agent"`
					Messengers []persistent.MessengerState `json:"messengers"`
				}{a.Name, st.Messengers})
			}
			printMessengers(cmd.OutOrStdout(), a, st.Messengers)
			return nil
		},
	}
}

// printMessengers prints an agent's messengers, one a line.
func printMessengers(w io.Writer, a persistent.Agent, ms []persistent.MessengerState) {
	if len(ms) == 0 {
		_, _ = fmt.Fprintf(w, "The %s agent has no messenger: agentry agent messenger add %s <%s>%s\n", a.Name, a.Name, strings.Join(messenger.Names(), "|"), onThisRunhost())
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  MESSENGER\tACCOUNT\tSWITCH\tPAIRED\tCONNECTION")
	for _, m := range ms {
		on := "off"
		if m.On {
			on = "on"
		}
		paired := "no"
		switch {
		case m.Paired:
			paired = "yes, " + m.PairedAt.Local().Format("2 Jan 2006 15:04")
		case m.Pairing:
			paired = "no, a code is taken until " + m.PairingUntil.Local().Format("15:04")
		}
		conn := m.Connection
		switch {
		case m.Refused != "":
			conn = "refused: " + m.Refused
		case conn == "":
			conn = "the agent is not running"
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", m.Title, safeTerminalText(m.Name, 80), on, paired, safeTerminalText(conn, 300))
	}
	_ = tw.Flush()
}

func newAgentMessengerTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test <name> <messenger>",
		Short: "Check a messenger's credential with the service, and send its paired person a test message",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, k, err := messengerArgs(args[0], args[1])
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			c, err := persistent.LoadConfig(home, a.Name)
			if err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			acct, ok := persistent.MessengerOf(c, k.Name)
			if !ok {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent has no %s messenger", a.Name, k.Title))
			}
			token, err := agentSecrets.Get(persistent.MessengerKeychainAccount(a.Name, k.Name))
			if err != nil {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s credential is not in %s (%v): agentry agent messenger add %s %s%s", k.Title, persistent.KeptIn, err, a.Name, k.Name, onThisRunhost()))
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			cred := messenger.Credential{Kind: k.Name, Token: token, Server: acct.Server}
			ready, err := messenger.Check(ctx, cred, messengerAPI)
			if err != nil {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("%s did not take the credential: %w", k.Title, err))
			}
			w := cmd.OutOrStdout()
			if ready.Account != acct.Account {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the credential in %s is %s's, not %s's: add it again", persistent.KeptIn, ready.Name, acct.Name))
			}
			_, _ = fmt.Fprintf(w, "%s takes the credential of %s.\n", k.Title, safeTerminalText(ready.Name, 80))
			if acct.Peer == nil {
				_, _ = fmt.Fprintf(w, "No account is paired, so no test message was sent: agentry agent messenger pair %s %s%s\n", a.Name, k.Name, onThisRunhost())
				return nil
			}
			msg := "A test from Agentry on " + yourMachine() + ": this " + k.Account + " reaches you. The agent did not write this."
			if err := messenger.SendTest(ctx, cred, messengerAPI, acct.Peer.Chat, msg); err != nil {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the test message was not sent: %w", err))
			}
			_, _ = fmt.Fprintf(w, "Sent a test message to your paired %s account.\n", k.Title)
			return nil
		},
	}
}

func newAgentMessengerRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name> <messenger>",
		Short: "Remove a messenger from a persistent agent, and its credential from where it is kept",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, k, err := messengerArgs(args[0], args[1])
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			r, running, err := configureAgent(cmd, home, a, persistent.Change{RemoveMessenger: k.Name}, false)
			if err != nil {
				return err
			}
			if err := agentSecrets.Delete(persistent.MessengerKeychainAccount(a.Name, k.Name)); err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), struct {
					Agent   string `json:"agent"`
					Removed string `json:"removed"`
				}{a.Name, k.Name})
			}
			w := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(w, "Removed %s from the %s agent and its credential from %s. The %s itself is still yours on %s: delete it there%s if you no longer want it.\n", k.Title, a.Name, persistent.KeptIn, k.Account, k.Title, removeHint(k))
			printChangedMessenger(w, a, r, running)
			return nil
		},
	}
}
