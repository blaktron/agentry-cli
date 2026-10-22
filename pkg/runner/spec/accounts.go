package spec

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A git host account is the Coding agent's sign-in on a git host: GitHub,
// GitLab, a Gitea-family host or any other, by host name. Its token reaches
// the programs it runs, which sign in to that host with it: git over https
// through a credential helper that answers for that host alone, and the
// host's own CLI (gh, glab, tea) through the variables it reads. One rule
// holds an account wherever it is taken or given: when the person sets it
// up, when a segment reads it and when the plan gives it to the Coding
// agent's programs. The tools server checks again what it builds a
// program's environment from: the host, the user name and the token's
// variable.

// GitHostKind is the kind of a git host, which says which CLI is given its
// token.
type GitHostKind string

// The kinds of git host.
const (
	// GitHostGitHub is github.com or a GitHub Enterprise Server host: gh.
	GitHostGitHub GitHostKind = "github"
	// GitHostGitLab is gitlab.com or a self-managed GitLab: glab.
	GitHostGitLab GitHostKind = "gitlab"
	// GitHostGitea is a Gitea, Forgejo or Codeberg host: tea.
	GitHostGitea GitHostKind = "gitea"
	// GitHostGit is any other git host: git alone.
	GitHostGit GitHostKind = "git"
)

// GitHubHost and GitLabHost are the hosted services' hosts; another host of
// their kind is an Enterprise Server or a self-managed instance.
const (
	GitHubHost = "github.com"
	GitLabHost = "gitlab.com"
)

// MaxCoderAccounts bounds the Coding agent's git host accounts.
const MaxCoderAccounts = 8

// CoderAccount is one of the Coding agent's git host accounts its roster
// has on (CoderGrant.Accounts). Its token is the segment's, never the
// plan's: the plan names the variable of the tools server's environment
// that holds it.
type CoderAccount struct {
	// Host is the git host, host[:port], in lower case.
	Host string      `json:"host"`
	Kind GitHostKind `json:"kind"`
	// Own says the account is the agent's own, which the person shares
	// repositories with, rather than the person's.
	Own bool `json:"own,omitempty"`
	// User is the account's user name on the host, which git signs in as.
	User string `json:"user"`
	// Name and Email are the commit identity in a repository whose remote
	// is on the host.
	Name  string `json:"name"`
	Email string `json:"email"`
	// TokenEnv is the variable of the tools server's environment that holds
	// the token (AccountTokenEnv).
	TokenEnv string `json:"tokenEnv"`
}

// AccountTokenEnv is the tools server's variable for the i-th account's
// token (from 0). Its prefix is one a stored secret may not take, so no
// secret the person stores can take its place.
func AccountTokenEnv(i int) string { return "AGENTRY_ACCOUNT_TOKEN_" + strconv.Itoa(i+1) }

var (
	gitHostRE    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+(:[0-9]{1,5})?$`)
	gitUserRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	gitEmailRE   = regexp.MustCompile(`^[^\s@<>"\\]+@[^\s@<>"\\]+$`)
	tokenEnvRE   = regexp.MustCompile(`^AGENTRY_ACCOUNT_TOKEN_[1-9][0-9]?$`)
	gitHostKinds = []GitHostKind{GitHostGitHub, GitHostGitLab, GitHostGitea, GitHostGit}
)

// CheckGitHost refuses a host that is not a host name with an optional
// port, in lower case: no scheme, no user, no path.
func CheckGitHost(host string) error {
	if !gitHostRE.MatchString(host) || len(host) > 253 {
		return fmt.Errorf("a git host is a host name with an optional port, in lower case, such as github.com or git.example.com:8443 (%q is not)", host)
	}
	if _, port, ok := strings.Cut(host, ":"); ok {
		if n, _ := strconv.Atoi(port); n < 1 || n > 65535 {
			return fmt.Errorf("the git host %s has no port %s", host, port)
		}
	}
	return nil
}

// GitHostKindOf is the kind of a hosted service's host, or "" for a host
// whose kind the person must say.
func GitHostKindOf(host string) GitHostKind {
	switch host {
	case GitHubHost:
		return GitHostGitHub
	case GitLabHost:
		return GitHostGitLab
	case "codeberg.org", "gitea.com":
		return GitHostGitea
	}
	return ""
}

// CheckGitHostKind refuses a kind that is not one.
func CheckGitHostKind(k GitHostKind) error {
	for _, x := range gitHostKinds {
		if k == x {
			return nil
		}
	}
	return fmt.Errorf("a git host's kind is github, gitlab, gitea or git (%q is not)", k)
}

// CheckGitIdentity refuses an account's user name, commit name or commit
// email that is not one: the user name is the host's (letters, digits,
// dots, hyphens and underscores), the name one line of at most 100
// characters, the email an address.
func CheckGitIdentity(user, name, email string) error {
	if !gitUserRE.MatchString(user) {
		return fmt.Errorf("an account's user name is letters, digits, dots, hyphens and underscores, at most 100 (%q is not)", user)
	}
	if name == "" || utf8.RuneCountInString(name) > 100 || !utf8.ValidString(name) {
		return errors.New("an account's commit name is 1 to 100 characters")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("an account's commit name is one line, with no control character")
		}
	}
	if len(email) > 254 || !gitEmailRE.MatchString(email) {
		return fmt.Errorf("an account's commit email is an address, such as coder@example.com (%q is not)", email)
	}
	return nil
}

// CheckCoderAccounts refuses accounts the Coding agent's programs cannot be
// given: one that is not one, two on a host, more than MaxCoderAccounts,
// and more than one host whose CLI reads a single token (a GitHub
// Enterprise Server, a GitLab, a Gitea), since that CLI would send the one
// token to every such host. The tokens' variables are AccountTokenEnv's.
func CheckCoderAccounts(accounts []CoderAccount) error {
	if len(accounts) > MaxCoderAccounts {
		return fmt.Errorf("the Coding agent has at most %d git host accounts", MaxCoderAccounts)
	}
	hosts := map[string]bool{}
	envs := map[string]bool{}
	single := map[string]string{}
	for _, a := range accounts {
		if err := CheckGitHost(a.Host); err != nil {
			return err
		}
		if err := CheckGitHostKind(a.Kind); err != nil {
			return fmt.Errorf("the account on %s: %w", a.Host, err)
		}
		if err := CheckGitIdentity(a.User, a.Name, a.Email); err != nil {
			return fmt.Errorf("the account on %s: %w", a.Host, err)
		}
		if !tokenEnvRE.MatchString(a.TokenEnv) || envs[a.TokenEnv] {
			return fmt.Errorf("the account on %s has no token variable of its own", a.Host)
		}
		if hosts[a.Host] {
			return fmt.Errorf("the Coding agent has one account on %s", a.Host)
		}
		hosts[a.Host], envs[a.TokenEnv] = true, true
		if c := SingleTokenCLI(a); c != "" {
			if other, ok := single[c]; ok {
				return fmt.Errorf("the Coding agent has one account on the hosts %s reads a single token for, so %s and %s cannot both have one", c, other, a.Host)
			}
			single[c] = a.Host
		}
	}
	return nil
}

// SingleTokenCLI names the CLI that reads one token for every host of the
// account's kind, or "" where the account's host has a token of its own
// (github.com) or no CLI (git).
func SingleTokenCLI(a CoderAccount) string {
	switch a.Kind {
	case GitHostGitHub:
		if a.Host == GitHubHost {
			return ""
		}
		return "gh (GitHub Enterprise Server)"
	case GitHostGitLab:
		return "glab"
	case GitHostGitea:
		return "tea"
	}
	return ""
}
