package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"

	"golang.org/x/net/idna"
)

// Environment tokens. A personal access token is a bearer credential:
// whichever host receives it can act as the account, so each variable
// authenticates to exactly one registry and is attached to no other.
//
//   - SECUREAGENTRY_TOKEN goes to the SecureAgentry registry this build is
//     for: its baked default registry, when that is a SecureAgentry
//     address. A build that bakes in anything else sends it nowhere.
//   - AGENTRY_TOKEN goes to the self-hosted registry the person
//     configured: AGENTRY_REGISTRY, else the config file's
//     defaultRegistry. A SecureAgentry address is never a self-hosted
//     registry, so AGENTRY_TOKEN never reaches SecureAgentry, whatever
//     --registry names. Set with no self-hosted registry configured, it is
//     refused rather than ignored, so a script that relied on it for
//     SecureAgentry fails loudly instead of running as someone else or as
//     nobody.
//
// A token the target does not match is not sent, the CLI says so once on
// stderr, and the target's stored sign-in applies as if the variable were
// unset. Registries are compared by origin (scheme, host and port), the
// scope the keychain's per-host entries already have.
const (
	envToken              = "AGENTRY_TOKEN"
	envSecureAgentryToken = "SECUREAGENTRY_TOKEN"
	// envRegistry names the registry the CLI targets when --registry is
	// not passed, ahead of the config file's defaultRegistry, and is the
	// self-hosted registry AGENTRY_TOKEN authenticates to.
	envRegistry = "AGENTRY_REGISTRY"
)

// secureAgentryDomain is SecureAgentry's registrable domain: the registry
// and every one of its environments are this host or a subdomain of it.
const secureAgentryDomain = "secureagentry.net"

// isSecureAgentry reports whether regURL is a SecureAgentry address. A
// host that cannot be put in the ASCII form a request would use counts as
// one, so a token that must never reach SecureAgentry is not sent to it.
func isSecureAgentry(regURL string) bool {
	u, err := url.Parse(strings.TrimSpace(regURL))
	if err != nil {
		return false
	}
	h, ok := lookupHost(u.Hostname())
	if !ok {
		return true
	}
	return h == secureAgentryDomain || strings.HasSuffix(h, "."+secureAgentryDomain)
}

// lookupHost is host as a request resolves it: lower-cased, without
// trailing dots, and a non-ASCII name in its ASCII (punycode) form, so
// that two spellings of one host compare equal. ok is false for a name
// that has no such form.
func lookupHost(host string) (string, bool) {
	h := strings.TrimRight(strings.ToLower(host), ".")
	for i := 0; i < len(h); i++ {
		if h[i] >= 0x80 {
			a, err := idna.Lookup.ToASCII(h)
			if err != nil {
				return "", false
			}
			return strings.TrimRight(a, "."), true
		}
	}
	return h, true
}

// registryOrigin is regURL's scheme, host and port, lower-cased, with the
// scheme's default port dropped; "" for a URL with no host.
func registryOrigin(regURL string) string {
	u, err := url.Parse(strings.TrimSpace(regURL))
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	host, ok := lookupHost(u.Hostname())
	if !ok {
		return ""
	}
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host
}

// sameRegistry reports whether a and b share an origin.
func sameRegistry(a, b string) bool {
	oa := registryOrigin(a)
	return oa != "" && oa == registryOrigin(b)
}

// secureAgentryRegistry is the SecureAgentry registry this build is for,
// or "" when its baked default registry is not a SecureAgentry address.
func secureAgentryRegistry() string {
	if isSecureAgentry(defaultRegistryURL) {
		return strings.TrimRight(defaultRegistryURL, "/")
	}
	return ""
}

// envRegistryURL is AGENTRY_REGISTRY without a trailing slash, "" when
// unset.
func envRegistryURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv(envRegistry)), "/")
}

// selfHostedRegistry is the self-hosted registry the person configured
// and where they configured it; "" when there is none. AGENTRY_REGISTRY
// decides when it is set, and the config file's defaultRegistry only when
// it is not. A config file that cannot be read is an error, not "none".
func selfHostedRegistry() (reg, source string, err error) {
	if v := envRegistryURL(); v != "" {
		if isSecureAgentry(v) {
			return "", "", nil
		}
		return v, envRegistry, nil
	}
	cfg, err := loadCLIConfig()
	if err != nil {
		return "", "", err
	}
	if cfg.DefaultRegistry == "" || isSecureAgentry(cfg.DefaultRegistry) {
		return "", "", nil
	}
	return strings.TrimRight(cfg.DefaultRegistry, "/"), "the config file's defaultRegistry", nil
}

// errNoSelfHostedRegistry refuses an AGENTRY_TOKEN that has no registry to
// go to.
var errNoSelfHostedRegistry = errors.New(envToken + " is set, but no self-hosted registry is configured, so it is sent nowhere: " +
	envToken + " authenticates only to the registry " + envRegistry + " names (else the config file's defaultRegistry), " +
	"and never to SecureAgentry. For SecureAgentry set " + envSecureAgentryToken + " instead, or unset " + envToken)

// envTokenFor resolves which environment token, if any, is sent to
// regURL: its variable's name and value, empty when none applies. notes
// holds one line for each token that is set and not sent, saying why.
func envTokenFor(regURL string) (name, token string, notes []string, err error) {
	if t := strings.TrimSpace(os.Getenv(envToken)); t != "" {
		self, source, err := selfHostedRegistry()
		if err != nil {
			return "", "", nil, fmt.Errorf("%s is set, and the registry it is for cannot be read: %w", envToken, err)
		}
		switch {
		case self == "":
			if v := envRegistryURL(); v != "" {
				return "", "", nil, fmt.Errorf("%w (%s is %s, a SecureAgentry address, which is not a self-hosted registry)", errNoSelfHostedRegistry, envRegistry, displayOrigin(v))
			}
			return "", "", nil, errNoSelfHostedRegistry
		case sameRegistry(regURL, self):
			name, token = envToken, t
		default:
			notes = append(notes, fmt.Sprintf("%s is not sent to %s: it authenticates only to %s, the self-hosted registry %s names",
				envToken, displayOrigin(regURL), displayOrigin(self), source))
		}
	}
	if t := strings.TrimSpace(os.Getenv(envSecureAgentryToken)); t != "" {
		home := secureAgentryRegistry()
		switch {
		case home == "":
			notes = append(notes, fmt.Sprintf("%s is not sent to %s: this build is for no SecureAgentry registry (its default registry is %s); sign in with `agentry login`",
				envSecureAgentryToken, displayOrigin(regURL), displayOrigin(defaultRegistryURL)))
		case sameRegistry(regURL, home):
			name, token = envSecureAgentryToken, t
		default:
			notes = append(notes, fmt.Sprintf("%s is not sent to %s: it authenticates only to %s",
				envSecureAgentryToken, displayOrigin(regURL), displayOrigin(home)))
		}
	}
	return name, token, notes, nil
}

// displayOrigin names a target in a note: its origin, never a path that
// could carry more than the host.
func displayOrigin(regURL string) string {
	if o := registryOrigin(regURL); o != "" {
		return o
	}
	return "this registry"
}

// envTokenHint names the variable a script would set for regURL, for a
// "no credential" message: "" when neither may go there.
func envTokenHint(regURL string) string {
	if home := secureAgentryRegistry(); home != "" && sameRegistry(regURL, home) {
		return envSecureAgentryToken
	}
	if isSecureAgentry(regURL) {
		return ""
	}
	return envToken
}

// noCredentialError is the refusal of a command that needs a sign-in
// for reg and has none.
func noCredentialError(reg string) error {
	if hint := envTokenHint(reg); hint != "" {
		return fmt.Errorf("no credential for %s — run `agentry login` or set %s", reg, hint)
	}
	return fmt.Errorf("no credential for %s — run `agentry login`", reg)
}

// envNotes is where envTokenFor's notes are written; said once each.
var (
	envNotes     io.Writer = os.Stderr
	envNotesSaid sync.Map
)

func sayEnvNotes(notes []string) {
	for _, n := range notes {
		if _, said := envNotesSaid.LoadOrStore(n, true); !said {
			_, _ = fmt.Fprintln(envNotes, "agentry: note: "+n)
		}
	}
}
