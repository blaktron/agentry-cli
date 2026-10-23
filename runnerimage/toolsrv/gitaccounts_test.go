package toolsrv

import (
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const (
	testGitHubToken = "ghp_testtoken0123456789abcdef"
	testGitLabToken = "glpat-testtoken0123456789"
)

// accountsServer is a Coding agent's tools server with an account of its
// own on github.com and the person's on a self-managed GitLab, and one
// account out of shape, which is given nothing.
func accountsServer(t *testing.T) *server {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("AGENTRY_ACCOUNT_TOKEN_1", testGitHubToken)
	t.Setenv("AGENTRY_ACCOUNT_TOKEN_2", testGitLabToken)
	t.Setenv("AGENTRY_ACCOUNT_TOKEN_3", "a-token-for-an-account-out-of-shape")
	spec := anySpec(t)
	spec.FSRead = Access{Enabled: true}
	spec.Exec.SecretEnv = []string{"AGENTRY_ACCOUNT_TOKEN_1", "AGENTRY_ACCOUNT_TOKEN_2", "AGENTRY_ACCOUNT_TOKEN_3"}
	spec.Exec.Accounts = []GitAccount{
		{Host: "github.com", Kind: "github", Own: true, User: "coder-bot", Name: "Coder Bot", Email: "coder-bot@example.com", TokenEnv: "AGENTRY_ACCOUNT_TOKEN_1"},
		{Host: "gitlab.example.com", Kind: "gitlab", User: "person", Name: `The "Person"`, Email: "person@example.com", TokenEnv: "AGENTRY_ACCOUNT_TOKEN_2"},
		{Host: "evil.example.com", Kind: "git", User: "x; rm -rf /", Name: "X", Email: "x@example.com", TokenEnv: "AGENTRY_ACCOUNT_TOKEN_3"},
	}
	return newTestServer(t, spec)
}

// TestAProgramIsGivenEachAccountForItsHostAlone: the hosts' CLIs read the
// token of their own kind's host, git's configuration names each host's
// credential helper and identity, and an account out of shape is given
// nothing.
func TestAProgramIsGivenEachAccountForItsHostAlone(t *testing.T) {
	s := accountsServer(t)
	env := s.anyEnv()
	for _, want := range []string{
		"GH_TOKEN=" + testGitHubToken, "GITLAB_TOKEN=" + testGitLabToken, "GITLAB_HOST=gitlab.example.com",
		"AGENTRY_ACCOUNT_TOKEN_1=" + testGitHubToken, "GIT_CONFIG_COUNT=8",
		"GIT_CONFIG_KEY_0=credential.https://github.com.helper", "GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_4=credential.https://gitlab.example.com.helper",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("env has no %q: %v", want, env)
		}
	}
	for _, kv := range env {
		if strings.Contains(kv, "evil.example.com") || strings.HasPrefix(kv, "GH_ENTERPRISE_TOKEN=") || strings.HasPrefix(kv, "GH_HOST=") {
			t.Errorf("env holds %q", kv)
		}
		// No token is written into a helper: the helper reads it.
		if strings.HasPrefix(kv, "GIT_CONFIG_VALUE_") && (strings.Contains(kv, testGitHubToken) || strings.Contains(kv, testGitLabToken)) {
			t.Errorf("a git config value holds a token: %q", kv)
		}
	}
	d := s.runAnyDescription()
	if !strings.Contains(d, "github.com as coder-bot (your own account") || !strings.Contains(d, "gitlab.example.com as person (the person's own account)") || strings.Contains(d, "evil") {
		t.Errorf("description = %q", d)
	}
}

// TestGitSignsInToAnAccountsHostAndCommitsAsItsIdentity runs git under
// run_command: it is answered with the token for the account's host and
// for no other, the result masks it, and a repository whose remote is on
// the host commits as the account.
func TestGitSignsInToAnAccountsHostAndCommitsAsItsIdentity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("a program runs confined on Linux alone")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git here")
	}
	s := accountsServer(t)
	if s.confine.refused != "" {
		t.Skipf("this machine cannot confine a program: %s", s.confine.refused)
	}
	run := func(script string) string {
		t.Helper()
		got, isErr := s.callTool(s.tools[ToolRunCommand], rawArgs(t, map[string]any{"command": "sh", "args": []string{"-c", script}, "dir": "data"}))
		if isErr {
			t.Fatalf("run_command: %s", got)
		}
		return got
	}
	got := run(`printf 'protocol=https\nhost=github.com\npath=o/r.git\n\n' | git credential fill; ` +
		`printf 'protocol=https\nhost=gitlab.example.com\n\n' | git credential fill; ` +
		`printf 'protocol=https\nhost=other.example.com\n\n' | GIT_TERMINAL_PROMPT=0 git credential fill 2>&1 || echo other-refused`)
	for _, want := range []string{"username=coder-bot\npassword=[redacted:AGENTRY_ACCOUNT_TOKEN_1]", "username=person\npassword=[redacted:AGENTRY_ACCOUNT_TOKEN_2]", "other-refused"} {
		if !strings.Contains(got, want) {
			t.Errorf("git credential fill: no %q in %q", want, got)
		}
	}
	if strings.Contains(got, testGitHubToken) || strings.Contains(got, testGitLabToken) {
		t.Fatalf("a token came back unmasked: %q", got)
	}
	got = run(`git init -q gh && cd gh && git remote add origin https://github.com/o/r.git && git config user.name && git config user.email; cd ..; ` +
		`git init -q gl && cd gl && git remote add origin https://person@gitlab.example.com/g/r.git && git config user.name; cd ..; ` +
		`git init -q none && cd none && (git config user.email || echo no-identity)`)
	for _, want := range []string{"Coder Bot\ncoder-bot@example.com\n", `The "Person"` + "\n", "no-identity"} {
		if !strings.Contains(got, want) {
			t.Errorf("identity: no %q in %q", want, got)
		}
	}
}

// TestAnAccountsTokenIsMaskedWhereverItIsReadBack: printed, or left in a
// file another tool reads, the token comes back masked.
func TestAnAccountsTokenIsMaskedWhereverItIsReadBack(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("a program runs confined on Linux alone")
	}
	s := accountsServer(t)
	if s.confine.refused != "" {
		t.Skipf("this machine cannot confine a program: %s", s.confine.refused)
	}
	got, _ := s.callTool(s.tools[ToolRunCommand], rawArgs(t, map[string]any{"command": "sh", "args": []string{"-c", `echo "gh=$GH_TOKEN"; echo "gl=$GITLAB_TOKEN" > data/left.txt`}}))
	if strings.Contains(got, testGitHubToken) || !strings.Contains(got, "gh=[redacted:AGENTRY_ACCOUNT_TOKEN_1]") {
		t.Errorf("run_command = %q", got)
	}
	got, _ = s.callTool(s.tools[ToolFSRead], rawArgs(t, map[string]any{"path": "data/left.txt"}))
	if strings.Contains(got, testGitLabToken) || !strings.Contains(got, "gl=[redacted:AGENTRY_ACCOUNT_TOKEN_2]") {
		t.Errorf("fs_read = %q", got)
	}
}

// TestAnAccountsVariablesComeBeforeAStoredSecretOfTheirName: a stored
// secret named GH_TOKEN does not take the account's place.
func TestAnAccountsVariablesComeBeforeAStoredSecretOfTheirName(t *testing.T) {
	s := accountsServer(t)
	s.spec.Exec.SecretEnv = append(s.spec.Exec.SecretEnv, "GH_TOKEN")
	t.Setenv("GH_TOKEN", "a-stored-secret-0123456789")
	env := s.anyEnv()
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "GH_TOKEN=") {
			n++
			if kv != "GH_TOKEN="+testGitHubToken {
				t.Errorf("GH_TOKEN is %q", kv)
			}
		}
	}
	if n != 1 {
		t.Errorf("GH_TOKEN set %d times: %v", n, env)
	}
}

// TestAnEnterpriseServerAndAGiteaHostGetTheirCLIsVariables: gh is told the
// Enterprise Server's host when it has no github.com account, and tea's
// login variables name the Gitea host.
func TestAnEnterpriseServerAndAGiteaHostGetTheirCLIsVariables(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("AGENTRY_ACCOUNT_TOKEN_1", testGitHubToken)
	t.Setenv("AGENTRY_ACCOUNT_TOKEN_2", testGitLabToken)
	spec := anySpec(t)
	spec.Exec.SecretEnv = []string{"AGENTRY_ACCOUNT_TOKEN_1", "AGENTRY_ACCOUNT_TOKEN_2"}
	spec.Exec.Accounts = []GitAccount{
		{Host: "codeberg.org", Kind: "gitea", User: "coder", Name: "Coder", Email: "c@example.com", TokenEnv: "AGENTRY_ACCOUNT_TOKEN_1"},
		{Host: "ghe.example.com:8443", Kind: "github", User: "coder", Name: "Coder", Email: "c@example.com", TokenEnv: "AGENTRY_ACCOUNT_TOKEN_2"},
	}
	env := newTestServer(t, spec).anyEnv()
	for _, want := range []string{
		"GITEA_SERVER_URL=https://codeberg.org", "GITEA_SERVER_TOKEN=" + testGitHubToken, "GITEA_SERVER_USER=coder",
		"GH_ENTERPRISE_TOKEN=" + testGitLabToken, "GH_HOST=ghe.example.com:8443",
		"GIT_CONFIG_KEY_4=credential.https://ghe.example.com:8443.helper",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("env has no %q: %v", want, env)
		}
	}
}
