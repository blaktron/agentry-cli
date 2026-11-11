package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

func newAgentConnectCmd() *cobra.Command {
	var again, remove bool
	cmd := &cobra.Command{
		Use:   "connect <name> [server address]",
		Short: "Sign in to a persistent agent's remote MCP servers that sign in by OAuth",
		Long: "A remote MCP server on a persistent agent that signs in by OAuth (auth: oauth),\n" +
			"one you added or one your organization's managed settings put on it, signs in\n" +
			"with your own connection: the vendor's consent screen in your browser, or its\n" +
			"address printed and the redirect pasted back. The connection is kept on this\n" +
			"computer for the agent, through its updates, until you remove it or reset the\n" +
			"agent. Each segment is given the server's access token alone, refreshed before\n" +
			"it starts and renewed while it runs; never the refresh token or a client\n" +
			"secret. A running agent is given the server at its next turn.\n\n" +
			"With a server address, only that server is connected. --again replaces a\n" +
			"connection that works; --remove removes the agent's connections (or that\n" +
			"server's), revoked at the vendor where it offers it.\n\n" +
			"  agentry agent connect assistant\n" +
			"  agentry agent connect assistant https://mcp.example.com/mcp --again\n" +
			"  agentry agent connect assistant --remove",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := cliAgent(args[0])
			if err != nil {
				return err
			}
			if again && remove {
				return engine.Classify(engine.ExitUsage, errors.New("--again and --remove are two different things: choose one"))
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			cfg, err := persistent.LoadConfig(home, a.Name)
			if err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			only := ""
			if len(args) == 2 {
				only = args[1]
			}
			if remove {
				return removeAgentConnections(cmd, home, a.Name, only)
			}
			servers := agentSignInServers(a, cfg, only)
			if len(servers) == 0 {
				if only != "" {
					return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent has no remote server %s that signs in by OAuth (agentry agent show %s lists its servers)", a.Name, only, a.Name))
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "the %s agent has no remote MCP server that signs in by OAuth: nothing to connect\n", a.Name)
				return nil
			}
			if !consentTTY(cmd.InOrStdin()) {
				return engine.Classify(engine.ExitPolicy, errors.New("connecting needs a terminal to ask for your consent"))
			}
			store := connection.Open(home, connectionKeys())
			clients := connection.OpenClients(home, connectionKeys())
			public := connection.OpenPublicClients(home, registryURL(), httpClient)
			httpc := vendorHTTP()
			for _, srv := range servers {
				k := persistent.ConnectionKey(a.Name, srv)
				if c, err := store.Get(k); err == nil && c.Broken == "" && !again {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: connected (issuer %s, since %s); --again replaces it\n", k.Server, c.Issuer, c.CreatedAt)
					continue
				}
				if _, err := consentFor(cmd, store, clients, public, httpc, k, srv, ""); err != nil {
					return engine.Classify(engine.ExitPolicy, err)
				}
			}
			if held, _ := persistent.Held(home, a.Name); held {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "The %s agent is running: it is given the server at its next turn.\n", a.Name)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&again, "again", false, "make the connection again even where one works")
	cmd.Flags().BoolVar(&remove, "remove", false, "remove the agent's connections, revoked at the vendor where it offers it")
	return cmd
}

// agentSignInServers are agent a's remote servers that sign in by OAuth,
// or the one at address only.
func agentSignInServers(a persistent.Agent, cfg persistent.Config, only string) []manifest.MCPServer {
	var out []manifest.MCPServer
	for _, srv := range persistent.SignInServers(a, cfg) {
		if only == "" || srv.URL == only {
			out = append(out, srv)
		}
	}
	return out
}

// removeAgentConnections removes agent name's connections, or the one to
// the server at only, and says which went.
func removeAgentConnections(cmd *cobra.Command, home, name, only string) error {
	rm := persistent.Removal{Store: connection.Open(home, connectionKeys()), Clients: connection.OpenClients(home, connectionKeys()), HTTP: vendorHTTP()}
	gone, err := persistent.RemoveAgentConnections(cmd.Context(), rm, name, only, "removed with agentry agent connect --remove")
	if err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	if len(gone) == 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "the %s agent had no connection to remove\n", name)
		return nil
	}
	for _, s := range gone {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed the %s agent's connection to %s\n", name, s)
	}
	return nil
}
