package persistent

// The Coding agent's git host accounts: a sign-in on GitHub, GitLab, a
// Gitea-family host or any other git host, each a roster entry
// account:<host>. The person chooses whose account it is: one of its own,
// which they share repositories with (recommended), or their own. The token
// is kept in the system keychain under account:<host>:<user>, never in
// config.yaml, which records the host, the kind, whose it is, the user name,
// the commit identity and when the token was last set. While the entry is
// on and the agent's commands are, the segment gives the token to the
// programs it runs: git signs in with it to that host alone, the host's CLI
// reads it, and it is masked as a stored secret is (spec.CoderAccount).

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// KindAccount is a git host account's roster entry.
const KindAccount EntryKind = "account"

// accountPrefix begins a git host account's roster id.
const accountPrefix = "account:"

// GitAccount is one of the Coding agent's git host accounts, as config.yaml
// keeps it.
type GitAccount struct {
	// Host is host[:port], in lower case.
	Host string           `yaml:"host" json:"host"`
	Kind spec.GitHostKind `yaml:"kind" json:"kind"`
	// Own says the account is the agent's own, rather than the person's.
	Own  bool   `yaml:"own" json:"own"`
	User string `yaml:"user" json:"user"`
	// Name and Email are the commit identity in a repository on the host.
	Name  string `yaml:"name" json:"name"`
	Email string `yaml:"email" json:"email"`
	// Set is when the token was last set, so setting it again reaches the
	// next turn.
	Set time.Time `yaml:"set" json:"set"`
}

// AccountEntryID is a git host account's roster id.
func AccountEntryID(host string) string { return accountPrefix + host }

// AccountKeychainAccount is the keychain account a git host account's token
// is kept under.
func AccountKeychainAccount(host, user string) string { return accountPrefix + host + ":" + user }

// check refuses an account that is not one (spec's rules), or one whose
// kind is not its host's where the host is a hosted service's.
func (g GitAccount) check() error {
	if err := spec.CheckGitHost(g.Host); err != nil {
		return err
	}
	if err := spec.CheckGitHostKind(g.Kind); err != nil {
		return err
	}
	if k := spec.GitHostKindOf(g.Host); k != "" && k != g.Kind {
		return fmt.Errorf("%s is a %s host, not %s", g.Host, k, g.Kind)
	}
	return spec.CheckGitIdentity(g.User, g.Name, g.Email)
}

// coder is the account as a segment is given it, its token in the
// variable tokenEnv.
func (g GitAccount) coder(tokenEnv string) spec.CoderAccount {
	return spec.CoderAccount{Host: g.Host, Kind: g.Kind, Own: g.Own, User: g.User, Name: g.Name, Email: g.Email, TokenEnv: tokenEnv}
}

// hostName is how a sentence names the account's host.
func (g GitAccount) hostName() string {
	switch g.Kind {
	case spec.GitHostGitHub:
		if g.Host == spec.GitHubHost {
			return "GitHub"
		}
		return "GitHub Enterprise Server " + g.Host
	case spec.GitHostGitLab:
		if g.Host == spec.GitLabHost {
			return "GitLab"
		}
		return "GitLab " + g.Host
	}
	return g.Host
}

// cli names the host's CLI that is given the token, or "".
func (g GitAccount) cli() string {
	switch g.Kind {
	case spec.GitHostGitHub:
		return "gh"
	case spec.GitHostGitLab:
		return "glab"
	case spec.GitHostGitea:
		return "tea"
	}
	return ""
}

// accountEntries are the Coding agent's git host accounts' entries.
func accountEntries(c Config) []RosterEntry {
	out := make([]RosterEntry, 0, len(c.Accounts))
	for _, g := range c.Accounts {
		whose := "your account"
		if g.Own {
			whose = "its own account"
		}
		out = append(out, RosterEntry{ID: AccountEntryID(g.Host), Kind: KindAccount, Name: g.hostName() + " as " + g.User + " (" + whose + ")", GrantedBy: GrantedByYou, Line: accountLine(g)})
	}
	return out
}

// accountLine is a git host account's consent sentence: whose account it
// is, what reaches the token, and what that lets the agent do.
func accountLine(g GitAccount) string {
	via := "git"
	if c := g.cli(); c != "" {
		via += " and " + c
	}
	line := "Signs in to " + g.hostName() + " as " + g.User + ", "
	if g.Own {
		line += "its own account: it reaches the repositories you share with that account and does there whatever the token lets it"
	} else {
		line += "your account: it can do whatever the token can do as you, in every repository the token reaches"
	}
	return line + "; the token reaches the programs it runs (" + via + " sign in with it), which can read it and send it anywhere they reach, while its commands are on; it commits there as " + g.Name + " <" + g.Email + ">; the token stays in your keychain, and Agentry masks it wherever it shows it whole"
}

// AccountGuidance is what the person is told before giving the agent an
// account: for one of its own, why and how to share repositories with it;
// for theirs, what the token lets it do and how to bound it.
func AccountGuidance(kind spec.GitHostKind, own bool) string {
	if own {
		g := "Its own account (recommended). The agent reaches only the repositories you share with that account, its commits, pull requests and comments are its own, and taking access away is removing a collaborator. "
		switch kind {
		case spec.GitHostGitHub:
			g += "GitHub's terms let a person keep one free machine account beside their own: sign up for one, make a fine-grained or classic token on it, and add it to a repository under Settings > Collaborators (or to a team in your organization)."
		case spec.GitHostGitLab:
			g += "Make a user for it, a personal access token with the api and write_repository scopes, and add it to a project under Manage > Members with the Developer role."
		default:
			g += "Make a user for it on the host, a token with repository read and write, and add it to your repositories as a collaborator."
		}
		return g
	}
	g := "Your account. The agent can do whatever the token can do as you: read and write every repository it reaches, open and merge pull requests, and change settings it has the scopes for. "
	switch kind {
	case spec.GitHostGitHub:
		g += "Make a fine-grained token limited to the repositories you choose, with Contents and Pull requests read and write, and nothing more."
	case spec.GitHostGitLab:
		g += "Make a project access token on the projects you choose, or a personal token with the fewest scopes that do the work."
	default:
		g += "Make a token with the fewest scopes the work needs, limited to chosen repositories where the host offers that."
	}
	return g
}

// addAccount records a git host account in c, on: a new one, or one whose
// token was set again. The token is kept in the keychain first.
func addAccount(a Agent, c *Config, g GitAccount) error {
	if !a.Builtin.Codes() {
		return fmt.Errorf("the %s agent has no git host accounts: the Coding agent alone is given them", a.Name)
	}
	if err := g.check(); err != nil {
		return err
	}
	next := slices.Clone(c.Accounts)
	if i := slices.IndexFunc(next, func(x GitAccount) bool { return x.Host == g.Host }); i >= 0 {
		next[i] = g
	} else {
		next = append(next, g)
	}
	if err := checkAccounts(next); err != nil {
		return err
	}
	c.Accounts = next
	delete(c.Switches, AccountEntryID(g.Host))
	return nil
}

// checkAccounts holds the accounts to the rules a segment's would be held
// to, so one the segment would refuse is refused when it is added. The
// token variables here only make them well formed: a segment numbers them
// by host.
func checkAccounts(accounts []GitAccount) error {
	given := make([]spec.CoderAccount, 0, len(accounts))
	for i, g := range accounts {
		given = append(given, g.coder(spec.AccountTokenEnv(i)))
	}
	return spec.CheckCoderAccounts(given)
}

// removeAccount takes a git host account out of c, by host, and returns it.
func removeAccount(a Agent, c *Config, host string) (GitAccount, error) {
	i := slices.IndexFunc(c.Accounts, func(x GitAccount) bool { return x.Host == host })
	if i < 0 {
		return GitAccount{}, fmt.Errorf("the %s agent has no account on %s", a.Name, host)
	}
	g := c.Accounts[i]
	c.Accounts = slices.Delete(slices.Clone(c.Accounts), i, i+1)
	delete(c.Switches, AccountEntryID(host))
	delete(c.Consented, AccountEntryID(host))
	return g, nil
}

// readAccounts reads the tokens of the accounts the roster has on from
// keys: every one must be there and must be a value the segment can carry
// and mask, as a stored secret's.
func readAccounts(keys Secrets, c Config, on func(string) bool) ([]localrun.Account, error) {
	var out []localrun.Account
	for _, g := range c.Accounts {
		if !on(AccountEntryID(g.Host)) {
			continue
		}
		if keys == nil {
			return nil, errors.New("the Coding agent has a git host account on, and this client cannot read the keychain")
		}
		if err := g.check(); err != nil {
			return nil, fmt.Errorf("the Coding agent's account on %s: %w (remove it: agentry agent configure coder --no-account %s)", g.Host, err, g.Host)
		}
		v, err := keys.Get(AccountKeychainAccount(g.Host, g.User))
		if err != nil {
			return nil, fmt.Errorf("the Coding agent's account on %s: %w (set its token again: agentry agent configure coder --account %s)", g.Host, err, g.Host)
		}
		if err := CheckSecretValue(v); err != nil {
			return nil, fmt.Errorf("the Coding agent's token for %s in the keychain: %w (set it again: agentry agent configure coder --account %s)", g.Host, err, g.Host)
		}
		out = append(out, localrun.Account{CoderAccount: g.coder(""), Token: v})
	}
	return out, nil
}
