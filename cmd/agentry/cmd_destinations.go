package main

// Destinations: the places a run's deliverables may be sent. `agentry
// destinations add|list|remove|default` keeps them (the fields in the config
// folder's destinations.yaml, the credentials in the system keychain), `agentry
// run --destination NAME` and a QuickAgent's default set choose where a run
// delivers, and `agentry deliver RUN-FOLDER --destination NAME` sends a finished
// run again. A destination is the person's: nothing a release says names one.

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/blaktron/agentry-cli/pkg/runner/destination"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/runnerimage/send"
)

func newDestinationsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "destinations",
		Aliases: []string{"destination"},
		Short:   "Places a run's deliverables are sent: webhooks you save by name",
		Long: "A destination is a place you choose to send a run's result and deliverables, saved under a name. A webhook\n" +
			"receives one HTTPS POST with the run's id, release and digest, its result, and each deliverable's path, title,\n" +
			"digest and content, signed so the receiver can check who sent it. Choose where a run delivers with\n" +
			"agentry run --destination NAME (repeatable), or give a QuickAgent a default set with agentry destinations\n" +
			"default. The delivery runs after the agent has ended, in a container of its own that reaches the\n" +
			"destination's host alone, and it never changes whether the run succeeded. The signing secret and any\n" +
			"Authorization value are kept in the system keychain and go to the delivery step alone.",
	}
	cmd.AddCommand(newDestinationsAddCmd(), newDestinationsListCmd(), newDestinationsRemoveCmd(), newDestinationsDefaultCmd())
	return cmd
}

func newDestinationsAddCmd() *cobra.Command {
	var (
		url, secretEnv, authEnv string
		onMyNetwork, secretIn   bool
		generate                bool
	)
	cmd := &cobra.Command{
		Use:   "add NAME --url URL",
		Short: "Save a webhook destination, or change a saved one",
		Long: "Save a webhook under NAME (1 to 40 lower-case letters, digits, '.', '-' or '_'). The address must be https and\n" +
			"on port 443, and reached at a public address: a destination that names an address on your own network (a LAN\n" +
			"n8n, a local server) is saved with --on-my-network, which also allows http and another port, and is recorded in\n" +
			"the receipt. Link-local space (169.254.0.0/16, fe80::/10) is never reached, and localhost is the delivery\n" +
			"step's own container: name this computer's address on your network.\n\n" +
			"Every body is signed: the X-Agentry-Signature header is sha256=<hex HMAC-SHA256 keyed by the secret of the\n" +
			"X-Agentry-Timestamp header, a full stop and the raw body>. Give the secret in the environment variable\n" +
			"--secret-env names, on stdin with --secret-stdin, by typing it when asked on a terminal, or let --generate-secret\n" +
			"make one and print it once, for you to give the receiver. --authorization-env names a variable holding the value\n" +
			"of an Authorization header the receiver wants. Saving again without a secret keeps the saved one.",
		Example: "  agentry destinations add n8n --url https://hooks.example.com/webhook/abc --generate-secret\n" +
			"  AGENTRY_HOOK_SECRET=... agentry destinations add lan --url http://192.168.1.20:5678/webhook/x --on-my-network --secret-env AGENTRY_HOOK_SECRET",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			usage := func(err error) error { return engine.Classify(engine.ExitUsage, err) }
			d := destination.Destination{Name: name, Kind: destination.KindWebhook, URL: strings.TrimSpace(url), OnMyNetwork: onMyNetwork}
			if err := d.Check(); err != nil {
				return usage(err)
			}
			n := 0
			for _, b := range []bool{secretEnv != "", secretIn, generate} {
				if b {
					n++
				}
			}
			if n > 1 {
				return usage(errors.New("give the secret one way: --secret-env, --secret-stdin or --generate-secret"))
			}
			secret, generated, err := readDestinationSecret(cmd, secretEnv, secretIn, generate)
			if err != nil {
				return usage(err)
			}
			auth := ""
			if authEnv != "" {
				auth = strings.TrimSpace(os.Getenv(authEnv))
				if auth == "" {
					return usage(fmt.Errorf("--authorization-env %s: the variable is empty or not set", authEnv))
				}
			}
			s, err := destination.Load()
			if err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			prev, existed := s.Find(name)
			if secret == "" && (!existed || !destination.HasSecret(name)) {
				if !stdinIsTTY(cmd.InOrStdin()) {
					return usage(errors.New("a destination needs a signing secret: give --secret-env, --secret-stdin or --generate-secret"))
				}
				if secret, err = askHidden(cmd, "Signing secret for the receiver (typed, not shown): "); err != nil || secret == "" {
					return usage(errors.New("a destination needs a signing secret"))
				}
			}
			if existed && authEnv == "" {
				// A change that names no Authorization value leaves the saved
				// one as it is, unless the address now names another host: a
				// value saved for one receiver is never sent to another.
				_, keep, kerr := destination.Secrets(name)
				if kerr != nil {
					return engine.Classify(engine.ExitCLI, kerr)
				}
				if keep != "" && sameHost(prev, d) {
					auth = keep
				} else if keep != "" {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "the address names another host, so the saved Authorization value is dropped: give --authorization-env to set one\n")
				}
			}
			if !destination.ValidSecretValues(secret, auth) {
				return usage(errors.New("a credential holds a control character or is not valid text"))
			}
			// The file's rules first, then the keychain, then the file; a
			// keychain or file write that fails leaves no credential of a
			// destination that was never saved.
			if err := s.Upsert(d); err != nil {
				return usage(err)
			}
			if err := destination.SaveSecrets(name, secret, auth); err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			if err := s.Save(); err != nil {
				if !existed {
					_ = destination.DeleteSecrets(name)
				}
				return engine.Classify(engine.ExitCLI, err)
			}
			// A signed-out schedule's fires read the keys sealed beside them:
			// a changed secret reaches them only by sealing again.
			resealSignedOut(cmd.ErrOrStderr())
			out := cmd.OutOrStdout()
			verb := "saved"
			if existed {
				verb = "updated"
			}
			_, _ = fmt.Fprintf(out, "%s destination %s: webhook to %s", verb, name, d.Where())
			if onMyNetwork {
				_, _ = fmt.Fprint(out, " (on my network)")
			}
			_, _ = fmt.Fprintln(out)
			if generated {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "signing secret (shown once; give it to the receiver, which verifies X-Agentry-Signature with it): %s\n", secret)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&url, "url", "", "the webhook's https address (required)")
	f.BoolVar(&onMyNetwork, "on-my-network", false, "the destination is on your own network: allow a private-network address, http and a port other than 443")
	f.StringVar(&secretEnv, "secret-env", "", "the name of an environment variable holding the signing secret")
	f.BoolVar(&secretIn, "secret-stdin", false, "read the signing secret from the first line of stdin")
	f.BoolVar(&generate, "generate-secret", false, "make a signing secret and print it once")
	f.StringVar(&authEnv, "authorization-env", "", "the name of an environment variable holding the value of the Authorization header the receiver wants")
	_ = cmd.MarkFlagRequired("url")
	return cmd
}

// readDestinationSecret reads the signing secret as the flags say; none when
// the person gave no way.
func readDestinationSecret(cmd *cobra.Command, env string, stdin, generate bool) (secret string, generated bool, err error) {
	switch {
	case env != "":
		v := strings.TrimSpace(os.Getenv(env))
		if v == "" {
			return "", false, fmt.Errorf("--secret-env %s: the variable is empty or not set", env)
		}
		return v, false, nil
	case stdin:
		line, rerr := bufio.NewReader(io.LimitReader(cmd.InOrStdin(), 64<<10)).ReadString('\n')
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return "", false, rerr
		}
		v := strings.TrimSpace(line)
		if v == "" {
			return "", false, errors.New("--secret-stdin: stdin held no secret")
		}
		return v, false, nil
	case generate:
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", false, err
		}
		return "whsec_" + base64.RawURLEncoding.EncodeToString(b), true, nil
	}
	return "", false, nil
}

// askHidden asks for a value on the terminal without echoing it.
func askHidden(cmd *cobra.Command, prompt string) (string, error) {
	f, ok := cmd.InOrStdin().(*os.File)
	if !ok {
		return "", errors.New("no terminal")
	}
	_, _ = fmt.Fprint(cmd.ErrOrStderr(), prompt)
	b, err := term.ReadPassword(int(f.Fd()))
	_, _ = fmt.Fprintln(cmd.ErrOrStderr())
	return strings.TrimSpace(string(b)), err
}

type destinationRow struct {
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Where         string   `json:"where"`
	OnMyNetwork   bool     `json:"onMyNetwork"`
	Secret        bool     `json:"secret"`
	Authorization bool     `json:"authorization"`
	DefaultFor    []string `json:"defaultFor"`
}

// sameHost says two destinations reach the same host and port.
func sameHost(a, b destination.Destination) bool {
	ta, err1 := send.CheckURL(a.URL, a.OnMyNetwork)
	tb, err2 := send.CheckURL(b.URL, b.OnMyNetwork)
	return err1 == nil && err2 == nil && ta.HostPort == tb.HostPort
}

func newDestinationsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the saved destinations",
		Long: "List each destination's name, kind, host (never its path or query, which may hold the receiver's own token) and\n" +
			"whether it is on your network, and the QuickAgents it is a default for. No secret is shown.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := destination.Load()
			if err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			rows := []destinationRow{}
			for _, d := range s.Destinations {
				row := destinationRow{Name: d.Name, Kind: d.Kind, Where: d.Where(), OnMyNetwork: d.OnMyNetwork,
					Secret: destination.HasSecret(d.Name), Authorization: destination.HasAuthorization(d.Name), DefaultFor: []string{}}
				for agent, names := range s.Defaults {
					for _, n := range names {
						if n == d.Name {
							row.DefaultFor = append(row.DefaultFor, agent)
						}
					}
				}
				slices.Sort(row.DefaultFor)
				rows = append(rows, row)
			}
			out := cmd.OutOrStdout()
			if flags.jsonOut {
				return emitJSON(out, map[string]any{"destinations": rows})
			}
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(out, "no destinations saved (agentry destinations add)")
				return nil
			}
			for _, r := range rows {
				extra := ""
				if r.OnMyNetwork {
					extra += "  on my network"
				}
				if !r.Secret {
					extra += "  NO SIGNING SECRET"
				}
				if len(r.DefaultFor) > 0 {
					extra += "  default for " + strings.Join(r.DefaultFor, ", ")
				}
				_, _ = fmt.Fprintf(out, "%s  %s  %s%s\n", r.Name, r.Kind, r.Where, extra)
			}
			return nil
		},
	}
}

func newDestinationsRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove NAME",
		Short: "Remove a saved destination and its credentials",
		Long:  "Remove the destination, its signing secret and Authorization value from the keychain, and its place in every QuickAgent's default set. Nothing already delivered is touched.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := destination.Load()
			if err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			if !s.Delete(args[0]) {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("no destination is saved under %q", args[0]))
			}
			if err := s.Save(); err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			if err := destination.DeleteSecrets(args[0]); err != nil {
				return engine.Classify(engine.ExitCLI, fmt.Errorf("the destination is removed, but its credentials could not be: %w", err))
			}
			resealSignedOut(cmd.ErrOrStderr())
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed destination %s\n", args[0])
			return nil
		},
	}
}

func newDestinationsDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "default NAMESPACE/NAME [DESTINATION...]",
		Short: "Set the destinations a QuickAgent's runs deliver to by default",
		Long: "A run of the QuickAgent that names no --destination delivers to this set, whichever version runs. With no\n" +
			"DESTINATION the set is cleared. The set is yours, kept on this computer: a release never names a destination.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := destination.Load()
			if err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			if err := s.SetDefault(args[0], args[1:]); err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			if err := s.Save(); err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			if len(args) == 1 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s has no default destinations\n", args[0])
			} else {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s delivers to %s by default\n", args[0], strings.Join(args[1:], ", "))
			}
			return nil
		},
	}
}

// chooseDestinations is the destinations a run delivers to: those named by
// --destination, else the QuickAgent's default set, each read with its
// credentials. A name that is not saved or has no signing secret refuses the run
// before it starts. A default set is the person's choice for a QuickAgent from
// the registry; a folder on disk names itself in its own manifest, so it gets no
// default set and delivers only where --destination says. The second answer says
// which names came from a default set.
func chooseDestinations(names []string, ref string, local bool) (dests []destination.Resolved, fromDefault bool, err error) {
	if len(names) == 0 && !local {
		s, err := destination.Load()
		if err != nil {
			return nil, false, engine.Classify(engine.ExitUsage, err)
		}
		names = s.DefaultsFor(ref)
		fromDefault = len(names) > 0
	}
	out, err := destination.Resolve(names)
	if err != nil {
		return nil, false, engine.Classify(engine.ExitUsage, err)
	}
	return out, fromDefault, nil
}

// destinationsFromHostFile reads the destinations a host handed the run in a
// file and takes each credential's variable out of this process's environment,
// so that no process started from here on inherits one: the credentials are the
// delivery step's alone.
func destinationsFromHostFile(path string) ([]destination.Resolved, error) {
	dests, names, err := destination.FromFile(path, os.Getenv)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		_ = os.Unsetenv(n)
	}
	return dests, nil
}
