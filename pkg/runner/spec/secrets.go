package spec

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// A persistent agent's stored secret is named by the environment variable a
// program or a server reads it as. One rule holds the name wherever it is
// taken or given: when the person stores it, when a segment reads it, and
// when the plan gives it to the Coding agent's programs.

var storedSecretNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// reservedSecretNames are variables the tools server, the programs it runs
// or the gateway set themselves: a secret by one of these names would change
// how a program runs or where it connects, not hand it a credential.
var reservedSecretNames = []string{
	"PATH", "HOME", "LANG", "LANGUAGE", "TERM", "TZ", "USER", "LOGNAME", "SHELL", "PWD", "OLDPWD", "HOSTNAME",
	"TMPDIR", "TMP", "TEMP", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "FTP_PROXY",
	"GOTOOLCHAIN", "NPM_CONFIG_UPDATE_NOTIFIER",
}

// reservedSecretPrefixes begin variables of the same kind: the locale, the
// dynamic loader, the runner's own keys and the gateway's.
var reservedSecretPrefixes = []string{"LC_", "LD_", "DYLD_", "AGENTRY_", "MCP_", "DOCKER_", "SSL_CERT_"}

// CheckStoredSecretName refuses a name that is not an environment
// variable's ([A-Z][A-Z0-9_]{0,63}), or one the runner and the programs it
// runs set themselves.
func CheckStoredSecretName(name string) error {
	if !storedSecretNameRE.MatchString(name) {
		return fmt.Errorf("a secret's name is an environment variable's: a capital letter, then at most 63 capital letters, digits and underscores (%q is not)", name)
	}
	if slices.Contains(reservedSecretNames, name) {
		return fmt.Errorf("%s is set by the runner or the programs it runs; name the secret otherwise", name)
	}
	for _, p := range reservedSecretPrefixes {
		if strings.HasPrefix(name, p) {
			return fmt.Errorf("a name that begins %s is set by the runner or the programs it runs; name the secret otherwise", p)
		}
	}
	return nil
}
