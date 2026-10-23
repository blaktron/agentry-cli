package toolsrv

// The Coding agent's git host accounts (Exec.Accounts). A program run under
// Any signs in to each account's host with its token and to no other host:
// git through a credential helper scoped to that host's https URLs, which
// reads the token from the program's environment, and the host's CLI
// through the variables it reads (gh's GH_TOKEN or GH_ENTERPRISE_TOKEN,
// glab's GITLAB_TOKEN, the GITEA_SERVER_* that tea's login reads). A
// repository whose remote is on the host commits as the account's name and
// email, from a git config file this server writes at its start. All of it
// reaches git as command-scope configuration (GIT_CONFIG_COUNT), so nothing
// is written into HOME, which lasts between segments: switching an account
// off takes it away at the next segment.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The kinds of git host, and the hosted services' hosts, as the spec
// names them.
const (
	gitKindGitHub = "github"
	gitKindGitLab = "gitlab"
	gitKindGitea  = "gitea"
	gitHubHost    = "github.com"
	gitLabHost    = "gitlab.com"
)

var (
	gitAccountHostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+(:[0-9]{1,5})?$`)
	gitAccountUserRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	gitAccountEnvRE  = regexp.MustCompile(`^AGENTRY_ACCOUNT_TOKEN_[1-9][0-9]?$`)
)

// gitAccounts are the accounts a program is given: those the spec names
// whose host, user name and token variable are well formed, whose token
// variable is one of the stored secrets the server masks, and whose token
// this server's environment holds.
func (s *server) gitAccounts() []GitAccount {
	if !s.spec.Exec.Any {
		return nil
	}
	var out []GitAccount
	for _, a := range s.spec.Exec.Accounts {
		if !gitAccountHostRE.MatchString(a.Host) || !gitAccountUserRE.MatchString(a.User) || !gitAccountEnvRE.MatchString(a.TokenEnv) ||
			!slices.Contains(s.spec.Exec.SecretEnv, a.TokenEnv) || os.Getenv(a.TokenEnv) == "" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// gitIdentityDir holds one git config file an account, its commit identity.
func gitIdentityDir() string { return filepath.Join(os.TempDir(), "agentry-git") }

func gitIdentityFile(i int) string {
	return filepath.Join(gitIdentityDir(), "account-"+strconv.Itoa(i+1)+".gitconfig")
}

// writeGitIdentities writes each account's commit identity, readable by
// the programs, at the server's start. Best effort: a program whose
// repository finds none says so itself.
func (s *server) writeGitIdentities() {
	accounts := s.gitAccounts()
	if len(accounts) == 0 {
		return
	}
	dir := gitIdentityDir()
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the programs, which run as the person, read it
		s.logf("git accounts: make %s: %v", dir, err)
		return
	}
	for i, a := range accounts {
		body := "[user]\n\tname = " + gitConfigQuote(a.Name) + "\n\temail = " + gitConfigQuote(a.Email) + "\n"
		if err := os.WriteFile(gitIdentityFile(i), []byte(body), 0o644); err != nil { //nolint:gosec // an identity, no secret
			s.logf("git accounts: write the identity for %s: %v", a.Host, err)
		}
	}
}

// gitConfigQuote is v as a quoted git config value.
func gitConfigQuote(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(v) + `"`
}

// accountsEnv is what a program is given for the accounts, beside their
// tokens' own variables (anyEnv): git's configuration, and each host CLI's
// variables.
func (s *server) accountsEnv() []string {
	accounts := s.gitAccounts()
	if len(accounts) == 0 {
		return nil
	}
	var keys, values []string
	set := func(k, v string) { keys, values = append(keys, k), append(values, v) }
	var env []string
	hasGitHub := slices.ContainsFunc(accounts, func(a GitAccount) bool { return a.Host == gitHubHost })
	for i, a := range accounts {
		base := "https://" + a.Host
		// An empty helper first, so a helper a program configured in HOME
		// is not asked for this host.
		set("credential."+base+".helper", "")
		set("credential."+base+".helper", `!f() { test "$1" = get || exit 0; echo username=`+a.User+`; echo "password=$`+a.TokenEnv+`"; }; f`)
		for _, pattern := range []string{base + "/**", "https://*@" + a.Host + "/**"} {
			set("includeIf.hasconfig:remote.*.url:"+pattern+".path", gitIdentityFile(i))
		}
		token := os.Getenv(a.TokenEnv)
		switch a.Kind {
		case gitKindGitHub:
			if a.Host == gitHubHost {
				env = append(env, "GH_TOKEN="+token)
			} else {
				env = append(env, "GH_ENTERPRISE_TOKEN="+token)
				if !hasGitHub {
					env = append(env, "GH_HOST="+a.Host)
				}
			}
		case gitKindGitLab:
			env = append(env, "GITLAB_TOKEN="+token)
			if a.Host != gitLabHost {
				env = append(env, "GITLAB_HOST="+a.Host)
			}
		case gitKindGitea:
			env = append(env, "GITEA_SERVER_URL="+base, "GITEA_SERVER_TOKEN="+token, "GITEA_SERVER_USER="+a.User)
		}
	}
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(keys)))
	for i := range keys {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, keys[i]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, values[i]))
	}
	return env
}

// accountsDescription says which hosts the programs sign in to, and how.
func (s *server) accountsDescription() string {
	accounts := s.gitAccounts()
	if len(accounts) == 0 {
		return ""
	}
	var named []string
	for _, a := range accounts {
		whose := "the person's own account"
		if a.Own {
			whose = "your own account, which the person shares repositories with"
		}
		cli := ""
		switch a.Kind {
		case gitKindGitHub:
			cli = ", and gh has its token"
		case gitKindGitLab:
			cli = ", and glab has its token"
		case gitKindGitea:
			cli = ", and tea's login is made from it (below)"
		}
		named = append(named, fmt.Sprintf("%s as %s (%s): commits in a repository whose remote is there are by %s <%s>%s", a.Host, a.User, whose, a.Name, a.Email, cli))
	}
	d := " You have git host accounts: " + strings.Join(named, "; ") +
		". git over https signs in to those hosts by itself, so clone and push https://HOST/OWNER/REPO.git with no token in the URL; never print a token, write it into a file, a remote URL or a commit, or send it anywhere."
	if slices.ContainsFunc(accounts, func(a GitAccount) bool { return a.Kind == gitKindGitea }) {
		d += " tea reads no token from its environment: run XDG_CONFIG_HOME=/tmp/tea tea login add --name HOST once (it takes the GITEA_SERVER_URL, GITEA_SERVER_TOKEN and GITEA_SERVER_USER it is given), and set XDG_CONFIG_HOME=/tmp/tea for every tea command, so its login file, which holds the token, is not kept in HOME."
	}
	return d
}
