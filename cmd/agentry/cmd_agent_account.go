// The Coding agent's git host accounts, set up by agentry agent configure:
// whose account it is, its user name and commit identity, and its token,
// read without echo and kept in the system keychain (pkg/runner/persistent).
package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// accountFlags are configure's flags for a git host account.
type accountFlags struct {
	host, kind, user, name, email string
	own, yours                    bool
}

// given says any of the flags that describe an account was passed.
func (f accountFlags) given() bool {
	return f.host != "" || f.kind != "" || f.user != "" || f.name != "" || f.email != "" || f.own || f.yours
}

const agentAccountLong = "--account gives the Coding agent an account on a git host: GitHub (github.com\n" +
	"or an Enterprise Server host), GitLab (gitlab.com or a self-managed host), a\n" +
	"Gitea, Forgejo or Codeberg host, or any other git host by name. Say whose\n" +
	"account it is: --own, an account of its own that you share repositories with\n" +
	"(recommended: it reaches only what you share, its work is its own, and taking\n" +
	"access away is removing a collaborator), or --yours, your own account, which\n" +
	"lets it do whatever the token can. --account-user is the account's user name\n" +
	"and --account-email its commit email; --account-name is the commit name\n" +
	"(default: the user name). --account-kind (github, gitlab, gitea or git) is\n" +
	"needed for a host other than github.com, gitlab.com, codeberg.org and\n" +
	"gitea.com. The token is read from the terminal without echo, or from stdin,\n" +
	"and kept in your system keychain. While the account is on, the programs the\n" +
	"agent runs sign in with it: git over https to that host alone, and gh, glab\n" +
	"or tea. --account with a host it has already sets that account's token again.\n" +
	"--no-account takes one away and removes its token from the keychain.\n\n" +
	"  agentry agent configure coder --account github.com --own --account-user my-coder-bot --account-email bot@example.com\n" +
	"  agentry agent configure coder --account gitlab.example.com --account-kind gitlab --yours --account-user me --account-email me@example.com < token.txt\n" +
	"  agentry agent configure coder --no-account github.com"

// readyAccount is the account the flags describe, its existing settings
// kept where a flag leaves them, and its token, read here. The person is
// told what giving it means before the token is asked for.
func readyAccount(cmd *cobra.Command, a persistent.Agent, c persistent.Config, f accountFlags) (persistent.GitAccount, string, error) {
	var g persistent.GitAccount
	if !a.Builtin.Codes() {
		return g, "", engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent has no git host accounts: the Coding agent alone is given them", a.Name))
	}
	host := strings.ToLower(strings.TrimSpace(f.host))
	if err := spec.CheckGitHost(host); err != nil {
		return g, "", engine.Classify(engine.ExitUsage, fmt.Errorf("--account: %w", err))
	}
	if f.own && f.yours {
		return g, "", engine.Classify(engine.ExitUsage, errors.New("pass --own or --yours, not both"))
	}
	if i := slices.IndexFunc(c.Accounts, func(x persistent.GitAccount) bool { return x.Host == host }); i >= 0 {
		g = c.Accounts[i]
		if f.own || f.yours {
			g.Own = f.own
		}
	} else {
		if !f.own && !f.yours {
			return g, "", engine.Classify(engine.ExitUsage, fmt.Errorf("--account %s: say whose account it is, --own (an account of its own, recommended) or --yours", host))
		}
		g = persistent.GitAccount{Host: host, Own: f.own, Kind: spec.GitHostKindOf(host)}
	}
	if f.kind != "" {
		g.Kind = spec.GitHostKind(f.kind)
	}
	if g.Kind == "" {
		return g, "", engine.Classify(engine.ExitUsage, fmt.Errorf("--account %s: say what kind of host it is with --account-kind (github, gitlab, gitea or git)", host))
	}
	if err := spec.CheckGitHostKind(g.Kind); err != nil {
		return g, "", engine.Classify(engine.ExitUsage, fmt.Errorf("--account-kind: %w", err))
	}
	if f.user != "" {
		g.User = f.user
	}
	if f.email != "" {
		g.Email = f.email
	}
	if f.name != "" {
		g.Name = f.name
	} else if g.Name == "" {
		g.Name = g.User
	}
	switch {
	case g.User == "":
		return g, "", engine.Classify(engine.ExitUsage, fmt.Errorf("--account %s: name the account's user name with --account-user", host))
	case g.Email == "":
		return g, "", engine.Classify(engine.ExitUsage, fmt.Errorf("--account %s: name the email its commits carry with --account-email (the account's own, or the host's private address for it)", host))
	}
	if err := spec.CheckGitIdentity(g.User, g.Name, g.Email); err != nil {
		return g, "", engine.Classify(engine.ExitUsage, err)
	}
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), safeTerminalText(persistent.AccountGuidance(g.Kind, g.Own), 1200))
	token, err := readSecretValue(cmd, "Token for "+g.User+" on "+host, "the token for "+g.User+" on "+host)
	if err != nil {
		return g, "", engine.Classify(engine.ExitUsage, err)
	}
	g.Set = time.Now().UTC()
	return g, token, nil
}

// keepAccountToken keeps the account's token in the keychain before the
// change records it, so no segment reads an account it has no token for;
// undo puts the keychain back as it was, for a change that fails.
func keepAccountToken(c persistent.Config, g persistent.GitAccount, token string) (undo func(), err error) {
	account := persistent.AccountKeychainAccount(g.Host, g.User)
	previous := ""
	if slices.ContainsFunc(c.Accounts, func(x persistent.GitAccount) bool { return x.Host == g.Host && x.User == g.User }) {
		previous, _ = agentSecrets.Get(account)
	}
	if err := agentSecrets.Set(account, token); err != nil {
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	return func() {
		if previous != "" {
			_ = agentSecrets.Set(account, previous)
		} else {
			_ = agentSecrets.Delete(account)
		}
	}, nil
}

// replacedAccountToken is the keychain account of a token the change
// leaves behind: the account's earlier user's, when the user name changed.
func replacedAccountToken(c persistent.Config, g persistent.GitAccount) string {
	i := slices.IndexFunc(c.Accounts, func(x persistent.GitAccount) bool { return x.Host == g.Host })
	if i < 0 || c.Accounts[i].User == g.User {
		return ""
	}
	return persistent.AccountKeychainAccount(g.Host, c.Accounts[i].User)
}
