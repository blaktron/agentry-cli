// agentry agent messenger: the messengers a persistent agent is reached on,
// each an account the person makes on the service and pairs with their own
// (pkg/runner/persistent).
package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/messenger"
)

const agentMessengerLong = "A messenger lets you talk to a persistent agent from a messaging app, away\n" +
	"from this computer: each message you send there becomes a turn of the\n" +
	"agent's conversation, as in Agentry, and its answer comes back there. The\n" +
	"Assistant, the Coding agent and the persistent agents you install can each\n" +
	"have one; the agent must be running (agentry agent start) to answer.\n\n" +
	"Telegram, through a bot of your own:\n" +
	"  1. In Telegram, open @BotFather, send /newbot and choose a name and a\n" +
	"     username ending in \"bot\". BotFather answers with the bot's token.\n" +
	"  2. agentry agent messenger add assistant telegram, and paste the token.\n" +
	"     It is checked with Telegram, kept in your system keychain, and asks\n" +
	"     you to agree to what Telegram carries.\n" +
	"  3. agentry agent messenger pair assistant telegram shows a code: send it\n" +
	"     to your bot from your own Telegram account within 10 minutes.\n\n" +
	"Only the account that sent the code reaches the agent; anyone else who\n" +
	"writes to the bot is ignored and never answered. Telegram does not encrypt\n" +
	"bot conversations end to end, so Telegram can read them. Send /stop to stop\n" +
	"what the agent is doing. Nothing is approved from a messenger: what the agent\n" +
	"asks your agreement to waits for you in Agentry. A message sent while the\n" +
	"agent answers another waits, and the waiting ones go together as its next\n" +
	"turn. One bot reaches one agent: give each agent a bot of its own.\n\n" +
	"The bot's token stays in your keychain; Agentry hands it to one container of\n" +
	"its own alone, which reaches api.telegram.org and nothing else, and the agent\n" +
	"is never given it. A messenger is a\n" +
	"roster entry, switched with agentry agent grant <name> messenger:telegram\n" +
	"on|off, which starts or stops it at once. The runner policy's\n" +
	"allowMessengers and messengerKinds can refuse messengers on this computer.\n\n" +
	"  agentry agent messenger add assistant telegram < token.txt\n" +
	"  agentry agent messenger pair assistant telegram\n" +
	"  agentry agent messenger list assistant\n" +
	"  agentry agent messenger test assistant telegram\n" +
	"  agentry agent messenger remove assistant telegram"

// messengerAPI is the messengers' address for add and test: the service's
// own, but in a test.
var messengerAPI = ""

func newAgentMessengerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "messenger",
		Short: "Add, pair, list, test and remove the messengers a persistent agent is reached on",
		Long:  agentMessengerLong,
	}
	cmd.AddCommand(newAgentMessengerAddCmd(), newAgentMessengerPairCmd(), newAgentMessengerListCmd(), newAgentMessengerTestCmd(), newAgentMessengerRemoveCmd())
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
		return func(string) (bool, string) { return true, "" }
	}
	return pol.MessengerAllowed
}

func newAgentMessengerAddCmd() *cobra.Command {
	var yes bool
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
			token, err := readSecretValue(cmd, k.Title+" "+k.Account+" token", "the "+k.Account+"'s token")
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
			ready, err := messenger.Check(ctx, k.Name, token, messengerAPI)
			if err != nil {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("%s did not take the token: %w", k.Title, err))
			}
			if other := persistent.MessengerAccountOf(home, k.Name, ready.Account); other != "" && other != a.Name {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("%s is the %s agent's %s %s: one %s reaches one agent, so make another for this one", ready.Name, other, k.Title, k.Account, k.Account))
			}
			account := persistent.MessengerKeychainAccount(a.Name, k.Name)
			previous, _ := agentSecrets.Get(account)
			if err := agentSecrets.Set(account, token); err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			m := persistent.MessengerAccount{Kind: k.Name, Account: ready.Account, Name: ready.Name, Set: time.Now().UTC()}
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
			_, _ = fmt.Fprintf(w, "%s took the token of %s, now kept in your system keychain.\n", k.Title, safeTerminalText(ready.Name, 80))
			printChangedMessenger(w, a, r, running)
			_, _ = fmt.Fprintf(w, "Next, pair your own %s account: agentry agent messenger pair %s %s\n", k.Title, a.Name, k.Name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "agree to what the messenger carries without being asked")
	return cmd
}

// printChangedMessenger says when a messenger change takes effect: at once
// for a running agent, which starts or stops its messenger without a
// restart.
func printChangedMessenger(w io.Writer, a persistent.Agent, r persistent.Roster, running bool) {
	if running {
		_, _ = fmt.Fprintf(w, "Saved. The %s agent takes it now.\n", a.Name)
	} else {
		_, _ = fmt.Fprintf(w, "Saved. The %s agent takes it when it next starts (agentry agent start %s).\n", a.Name, a.Name)
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
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent has no %s messenger: agentry agent messenger add %s %s", a.Name, k.Title, a.Name, k.Name))
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
			_, _ = fmt.Fprintf(w, "From your own %s account, send this code to %s before %s:\n\n    %s\n\n",
				k.Title, safeTerminalText(acct.Name, 80), p.Until.Local().Format("15:04"), persistent.FormatPairingCode(code))
			if !running {
				_, _ = fmt.Fprintf(w, "The %s agent is not running, and takes the code only while it runs: start it now (agentry agent start %s).\n", a.Name, a.Name)
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
			return engine.Classify(engine.ExitUsage, fmt.Errorf("the code expired before %s sent it: run agentry agent messenger pair %s %s again", k.Title, a.Name, k.Name))
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
		_, _ = fmt.Fprintf(w, "The %s agent has no messenger: agentry agent messenger add %s telegram\n", a.Name, a.Name)
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
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s token is not in your keychain (%v): agentry agent messenger add %s %s", k.Title, err, a.Name, k.Name))
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			ready, err := messenger.Check(ctx, k.Name, token, messengerAPI)
			if err != nil {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("%s did not take the token: %w", k.Title, err))
			}
			w := cmd.OutOrStdout()
			if ready.Account != acct.Account {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the token in your keychain is %s's, not %s's: add it again", ready.Name, acct.Name))
			}
			_, _ = fmt.Fprintf(w, "%s takes the token of %s.\n", k.Title, safeTerminalText(ready.Name, 80))
			if acct.Peer == nil {
				_, _ = fmt.Fprintf(w, "No account is paired, so no test message was sent: agentry agent messenger pair %s %s\n", a.Name, k.Name)
				return nil
			}
			msg := "A test from Agentry on your computer: this " + k.Account + " reaches you. The agent did not write this."
			if err := messenger.SendTest(ctx, k.Name, token, messengerAPI, acct.Peer.Chat, msg); err != nil {
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
		Short: "Remove a messenger from a persistent agent, and its credential from your keychain",
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
			_, _ = fmt.Fprintf(w, "Removed %s from the %s agent and its token from your keychain. The %s itself is still yours on %s: delete it there (BotFather's /deletebot) if you no longer want it.\n", k.Title, a.Name, k.Account, k.Title)
			printChangedMessenger(w, a, r, running)
			return nil
		},
	}
}
