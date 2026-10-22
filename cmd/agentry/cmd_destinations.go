package main

// Destinations: the places a run's deliverables may be sent (a webhook, or a
// bucket of an S3 API). `agentry destinations add|list|remove|default` keeps them (the fields in the config
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
)

func newDestinationsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "destinations",
		Aliases: []string{"destination"},
		Short:   "Places a run's deliverables are sent: webhooks and S3 buckets you save by name",
		Long: "A destination is a place you choose to send a run's result and deliverables, saved under a name. A webhook\n" +
			"receives one HTTPS POST with the run's id, release and digest, its result, and each deliverable's path, title,\n" +
			"digest and content, signed so the receiver can check who sent it. An S3 bucket (Amazon's, or any service that\n" +
			"speaks its API: Cloudflare R2, MinIO, Backblaze B2) receives each deliverable as an object at\n" +
			"<bucket>/<prefix>/<run id>/<file>, signed with your access key. Choose where a run delivers with\n" +
			"agentry run --destination NAME (repeatable), or give a QuickAgent a default set with agentry destinations\n" +
			"default. The delivery runs after the agent has ended, in a container of its own that reaches the\n" +
			"destination's host alone, and it never changes whether the run succeeded. The signing secret and any\n" +
			"Authorization value, or the bucket's access key id and secret access key, are kept in the system keychain\n" +
			"and go to the delivery step alone.",
	}
	cmd.AddCommand(newDestinationsAddCmd(), newDestinationsListCmd(), newDestinationsRemoveCmd(), newDestinationsDefaultCmd())
	return cmd
}

func newDestinationsAddCmd() *cobra.Command {
	var (
		url, secretEnv, authEnv string
		onMyNetwork, secretIn   bool
		generate                bool
		b                       s3Flags
	)
	cmd := &cobra.Command{
		Use:   "add NAME (--url URL | --bucket BUCKET)",
		Short: "Save a webhook or an S3 bucket as a destination, or change a saved one",
		Long: "Save a webhook (--url) or an S3 bucket (--bucket) under NAME. A bucket is described after the webhook.\n\n" +
			"A webhook is saved under NAME (1 to 40 lower-case letters, digits, '.', '-' or '_'). The address must be https and\n" +
			"on port 443, and reached at a public address: a destination that names an address on your own network (a LAN\n" +
			"n8n, a local server) is saved with --on-my-network, which also allows http and another port, and is recorded in\n" +
			"the receipt. Link-local space (169.254.0.0/16, fe80::/10) is never reached, and localhost is the delivery\n" +
			"step's own container: name this computer's address on your network.\n\n" +
			"Every body is signed: the X-Agentry-Signature header is sha256=<hex HMAC-SHA256 keyed by the secret of the\n" +
			"X-Agentry-Timestamp header, a full stop and the raw body>. Give the secret in the environment variable\n" +
			"--secret-env names, on stdin with --secret-stdin, by typing it when asked on a terminal, or let --generate-secret\n" +
			"make one and print it once, for you to give the receiver. --authorization-env names a variable holding the value\n" +
			"of an Authorization header the receiver wants. Saving again without a secret keeps the saved one; the saved\n" +
			"Authorization value is kept only while the address reaches the same receiver (the same scheme, host and port,\n" +
			"and the same On my network rule: a change of path keeps it), and is otherwise dropped.\n\n" +
			"A bucket (--bucket) takes --region (the signing region: us-east-1, eu-west-1, or auto for R2) and an optional\n" +
			"--prefix (folder/subfolder, letters, digits, '.', '_' and '-'). Each deliverable is put to\n" +
			"<bucket>/<prefix>/<run id>/<file> with a signature made from your access key; the file's name is made plain\n" +
			"so it can never leave that folder, a file may be 64 MiB at most, and an object already at a key is replaced.\n" +
			"Without --endpoint the bucket is Amazon S3 (reached at <bucket>.s3.<region>.amazonaws.com, so its name has no\n" +
			"'.'); another service's address goes in --endpoint (https://<account>.r2.cloudflarestorage.com), which\n" +
			"defaults --region to us-east-1 when it is not given. A service on your own network (a MinIO on the LAN) is\n" +
			"saved with --on-my-network, which allows its private address, http and another port, and is recorded in the\n" +
			"receipt. Give the access key id in the variable --access-key-id-env names and the secret access key in\n" +
			"--secret-env or on stdin with --secret-stdin, or type either when asked on a terminal. Saving again without\n" +
			"them keeps the saved pair, unless the bucket is now at another service.",
		Example: "  agentry destinations add n8n --url https://hooks.example.com/webhook/abc --generate-secret\n" +
			"  AGENTRY_HOOK_SECRET=... agentry destinations add lan --url http://192.168.1.20:5678/webhook/x --on-my-network --secret-env AGENTRY_HOOK_SECRET\n" +
			"  agentry destinations add archive --bucket my-reports --prefix agentry --region eu-west-1 --access-key-id-env AWS_ACCESS_KEY_ID --secret-env AWS_SECRET_ACCESS_KEY\n" +
			"  agentry destinations add minio --bucket reports --endpoint http://192.168.1.20:9000 --on-my-network --access-key-id-env MINIO_USER --secret-env MINIO_PASSWORD",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			usage := func(err error) error { return engine.Classify(engine.ExitUsage, err) }
			if (url == "") == (b.bucket == "") {
				return usage(errors.New("name what to save: --url for a webhook, or --bucket for an S3 bucket"))
			}
			if b.bucket != "" {
				if generate || authEnv != "" {
					return usage(errors.New("--generate-secret and --authorization-env are for a webhook: a bucket is signed with your access key"))
				}
				return addBucket(cmd, name, b, onMyNetwork, secretEnv, secretIn)
			}
			if b.set() {
				return usage(errors.New("--prefix, --region, --endpoint and --access-key-id-env are for a bucket (--bucket)"))
			}
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
			// A secret saved for a bucket is the secret access key, never a
			// webhook's signing secret.
			if secret == "" && (!existed || prev.Kind != destination.KindWebhook || !destination.HasSecret(name)) {
				if !stdinIsTTY(cmd.InOrStdin()) {
					return usage(errors.New("a destination needs a signing secret: give --secret-env, --secret-stdin or --generate-secret"))
				}
				if secret, err = askHidden(cmd, "Signing secret for the receiver (typed, not shown): "); err != nil || secret == "" {
					return usage(errors.New("a destination needs a signing secret"))
				}
			}
			if existed && authEnv == "" {
				// A change that names no Authorization value leaves the saved
				// one as it is, unless it now reaches another receiver (another
				// scheme, host or port, or the On my network rule changed): a
				// value saved for one receiver is never sent to another.
				_, keep, kerr := destination.Secrets(name)
				if kerr != nil {
					return engine.Classify(engine.ExitCLI, kerr)
				}
				if keep != "" && prev.SameReceiver(d) {
					auth = keep
				} else if keep != "" {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "the address now reaches another receiver (its scheme, host, port or the On my network rule changed), so the saved Authorization value is dropped: give --authorization-env to set one\n")
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
			before := destination.Snapshot(name)
			if err := destination.SaveSecrets(name, secret, auth); err != nil {
				destination.Restore(name, before)
				return engine.Classify(engine.ExitCLI, err)
			}
			if err := s.Save(); err != nil {
				destination.Restore(name, before)
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
	f.StringVar(&b.bucket, "bucket", "", "an S3 bucket to put each deliverable in (instead of --url)")
	f.StringVar(&b.prefix, "prefix", "", "the bucket's key prefix, folder/subfolder (default: none)")
	f.StringVar(&b.region, "region", "", "the bucket's signing region (us-east-1, eu-west-1, auto for R2); us-east-1 with --endpoint when not given")
	f.StringVar(&b.endpoint, "endpoint", "", "the S3 service's address when it is not Amazon's (R2, MinIO), https://host[:port] with no path")
	f.StringVar(&b.keyIDEnv, "access-key-id-env", "", "the name of an environment variable holding the bucket's access key id")
	return cmd
}

// s3Flags are the flags that describe a bucket.
type s3Flags struct {
	bucket, prefix, region, endpoint, keyIDEnv string
}

// set says whether a flag only a bucket takes was given.
func (b s3Flags) set() bool {
	return b.prefix != "" || b.region != "" || b.endpoint != "" || b.keyIDEnv != ""
}

// addBucket saves a bucket destination: its fields in the destinations file and
// its access key id and secret access key in the keychain. Credentials saved for
// another service are not kept for a changed one.
func addBucket(cmd *cobra.Command, name string, b s3Flags, onMyNetwork bool, secretEnv string, secretIn bool) error {
	usage := func(err error) error { return engine.Classify(engine.ExitUsage, err) }
	region := strings.TrimSpace(b.region)
	if region == "" && strings.TrimSpace(b.endpoint) != "" {
		region = "us-east-1"
	}
	d := destination.Destination{Name: name, Kind: destination.KindS3, Bucket: strings.TrimSpace(b.bucket), Prefix: strings.Trim(strings.TrimSpace(b.prefix), "/"),
		Region: region, Endpoint: strings.TrimSpace(b.endpoint), OnMyNetwork: onMyNetwork}
	if err := d.Check(); err != nil {
		return usage(err)
	}
	if secretEnv != "" && secretIn {
		return usage(errors.New("give the secret access key one way: --secret-env or --secret-stdin"))
	}
	var keyID, secret string
	if b.keyIDEnv != "" {
		if keyID = strings.TrimSpace(os.Getenv(b.keyIDEnv)); keyID == "" {
			return usage(fmt.Errorf("--access-key-id-env %s: the variable is empty or not set", b.keyIDEnv))
		}
	}
	if secretEnv != "" || secretIn {
		var err error
		if secret, _, err = readDestinationSecret(cmd, secretEnv, secretIn, false); err != nil {
			return usage(err)
		}
	}
	s, err := destination.Load()
	if err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	prev, existed := s.Find(name)
	// A pair saved for one service is kept for a changed one only when it is the
	// same service: an access key saved for one is never sent to another.
	keep := existed && prev.SameReceiver(d)
	haveKeyID, haveSecret := keep && destination.HasAccessKey(name), keep && destination.HasSecret(name)
	if existed && !keep && (keyID == "" || secret == "") {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "the destination is now at another service, so its saved credentials are not kept: give the access key id and secret access key\n")
	}
	if keyID == "" && !haveKeyID {
		if !stdinIsTTY(cmd.InOrStdin()) {
			return usage(errors.New("a bucket needs an access key id: give --access-key-id-env"))
		}
		if keyID, err = askHidden(cmd, "Access key id (typed, not shown): "); err != nil || keyID == "" {
			return usage(errors.New("a bucket needs an access key id"))
		}
	}
	if secret == "" && !haveSecret {
		if !stdinIsTTY(cmd.InOrStdin()) {
			return usage(errors.New("a bucket needs a secret access key: give --secret-env or --secret-stdin"))
		}
		if secret, err = askHidden(cmd, "Secret access key (typed, not shown): "); err != nil || secret == "" {
			return usage(errors.New("a bucket needs a secret access key"))
		}
	}
	if !destination.ValidS3Credentials(keyID, secret) {
		return usage(errors.New("the access key id or secret access key is not one a request can be signed with"))
	}
	if err := s.Upsert(d); err != nil {
		return usage(err)
	}
	before := destination.Snapshot(name)
	if err := destination.SaveS3Credentials(name, keyID, secret); err != nil {
		destination.Restore(name, before)
		return engine.Classify(engine.ExitCLI, err)
	}
	if err := s.Save(); err != nil {
		destination.Restore(name, before)
		return engine.Classify(engine.ExitCLI, err)
	}
	resealSignedOut(cmd.ErrOrStderr())
	verb := "saved"
	if existed {
		verb = "updated"
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "%s destination %s: bucket %s", verb, name, d.Where())
	if onMyNetwork {
		_, _ = fmt.Fprint(out, " (on my network)")
	}
	_, _ = fmt.Fprintln(out)
	return nil
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

// askHidden asks for a value on the terminal without echoing it. An
// interrupt (Ctrl-C, which cancels the command's context) ends the read
// with nothing read and the terminal's echo back on.
func askHidden(cmd *cobra.Command, prompt string) (string, error) {
	f, ok := cmd.InOrStdin().(*os.File)
	if !ok {
		return "", errors.New("no terminal")
	}
	fd := int(f.Fd())
	state, err := term.GetState(fd)
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprint(cmd.ErrOrStderr(), prompt)
	type answer struct {
		b   []byte
		err error
	}
	got := make(chan answer, 1)
	go func() {
		b, err := term.ReadPassword(fd)
		got <- answer{b, err}
	}()
	select {
	case a := <-got:
		_, _ = fmt.Fprintln(cmd.ErrOrStderr())
		return strings.TrimSpace(string(a.b)), a.err
	case <-cmd.Context().Done():
		// The read is abandoned: the process ends with the command.
		_ = term.Restore(fd, state)
		_, _ = fmt.Fprintln(cmd.ErrOrStderr())
		return "", errors.New("interrupted: nothing was read")
	}
}

type destinationRow struct {
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Where         string   `json:"where"`
	OnMyNetwork   bool     `json:"onMyNetwork"`
	Secret        bool     `json:"secret"`
	AccessKey     bool     `json:"accessKey"`
	Authorization bool     `json:"authorization"`
	DefaultFor    []string `json:"defaultFor"`
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
					Secret: destination.HasSecret(d.Name), AccessKey: destination.HasAccessKey(d.Name), Authorization: destination.HasAuthorization(d.Name), DefaultFor: []string{}}
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
				switch {
				case r.Kind == destination.KindS3 && (!r.Secret || !r.AccessKey):
					extra += "  NO ACCESS KEY"
				case r.Kind != destination.KindS3 && !r.Secret:
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
	// And every other variable of the credentials' prefix, named or not.
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "AGENTRY_DEST_") {
			_ = os.Unsetenv(k)
		}
	}
	return dests, nil
}
