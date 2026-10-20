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
	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

// agentConsent asks the person to agree to what a change or a start turns
// on, on the terminal, unless yes says they already have.
func agentConsent(cmd *cobra.Command, a persistent.Agent, ce *persistent.ConsentError, yes bool) error {
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
			"conversation.\n\n" +
			"  agentry agent grant assistant share off\n" +
			"  agentry agent grant assistant knowledge:my-notes on",
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
		yes, allowVulnerable                                                                   bool
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
			"tools, secrets and env as a manifest declares them). A server's secrets are\n" +
			"read from your runner policy's secret source when the agent starts.\n\n" +
			"  agentry agent configure assistant --name Sandra --personality-file sandra.md\n" +
			"  agentry agent configure assistant --share ~/Documents/assistant\n" +
			"  agentry agent configure assistant --knowledge my-notes\n" +
			"  agentry agent configure assistant --add-server server.yaml",
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
			if set == 0 {
				return engine.Classify(engine.ExitUsage, errors.New("nothing to change: pass one of the flags (agentry agent configure --help)"))
			}
			r, running, err := configureAgent(cmd, home, a, ch, yes)
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
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "the name the agent goes by (empty clears it)")
	f.StringVar(&personalityFile, "personality-file", "", fmt.Sprintf("a text file describing the personality you want it to have (at most %d KiB)", persistent.MaxPersonalityBytes>>10))
	f.StringVar(&share, "share", "", "the folder it shares with you, read and write (empty: ~/agentry/<agent>share)")
	f.StringVar(&runOnStart, "run-on-start", "", "true or false: start it when Agentry Desktop starts")
	f.StringVar(&addServer, "add-server", "", "a file holding an MCP server to add, in a manifest's shape")
	f.StringVar(&removeServer, "remove-server", "", "the roster id of a server to remove (server:…)")
	f.StringVar(&addPack, "knowledge", "", "a knowledge pack kept on this computer for it to search")
	f.StringVar(&removePack, "no-knowledge", "", "a knowledge pack to take away")
	f.BoolVar(&yes, "yes", false, "agree to what the change turns on without being asked")
	f.BoolVar(&allowVulnerable, "allow-vulnerable-mcp", false, "add a server version the registry has a high or critical advisory for")
	return cmd
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
