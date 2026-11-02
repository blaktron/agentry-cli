package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/clirelease"
	rhsetup "github.com/blaktron/agentry-cli/pkg/runhost/setup"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
)

// newRunhostCmd builds `agentry runhost`: remote runhosts, the person's own
// servers set up over SSH to run their runs.
func newRunhostCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runhost",
		Short: "Set up and manage remote runhosts: your own Linux servers that run your runs",
		Long: "A remote runhost is a Linux server you own that this CLI sets up over SSH to run\n" +
			"your runs. Setup signs in with an admin account (root, or one that can sudo)\n" +
			"once and keeps that sign-in nowhere. It installs a dedicated agentry account,\n" +
			"Docker Engine where there is none, gVisor for every container, this CLI's own\n" +
			"signed release, and the runhost's service, which listens on an owner-only\n" +
			"socket and opens no network port. This computer is paired with its own run\n" +
			"key, held to that service: setup checks that it is refused a shell, a\n" +
			"terminal, any other command and forwarding.\n\n" +
			"Servers: Debian 12 and 13, Ubuntu 22.04 and 24.04, on amd64 or arm64.",
	}
	cmd.AddCommand(newRunhostAddCmd(), newRunhostListCmd(), newRunhostStatusCmd(), newRunhostDoctorCmd(), newRunhostRemoveCmd(),
		newRunhostJobsCmd(), newRunhostAttachCmd(), newRunhostCollectCmd(), newRunhostCancelCmd(),
		newRunhostServeCmd(), newRunhostServiceCmd())
	return cmd
}

// adminFlags name the admin sign-in: files and environment variables, never
// a value on the command line.
type adminFlags struct {
	keyFile       string
	passphraseEnv string
	passwordEnv   string
	passwordFile  string
	agent         bool
	sudoEnv       string
	sudoFile      string
}

func (f *adminFlags) bind(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.keyFile, "key", "", "the admin account's private key FILE")
	fl.StringVar(&f.passphraseEnv, "passphrase-env", "", "environment variable holding the key's passphrase (asked at a terminal otherwise)")
	fl.BoolVar(&f.agent, "agent", false, "sign in with the SSH agent (SSH_AUTH_SOCK)")
	fl.StringVar(&f.passwordEnv, "password-env", "", "environment variable holding the admin account's password")
	fl.StringVar(&f.passwordFile, "password-file", "", "FILE holding the admin account's password")
	fl.StringVar(&f.sudoEnv, "sudo-password-env", "", "environment variable holding the account's sudo password (asked at a terminal otherwise)")
	fl.StringVar(&f.sudoFile, "sudo-password-file", "", "FILE holding the account's sudo password")
}

// resolve reads the sign-in the flags name. With none named it uses the SSH
// agent when there is one, else asks for the password at a terminal.
func (f *adminFlags) resolve(ask targethost.Asker, who string) (rhsetup.AdminAuth, error) {
	var a rhsetup.AdminAuth
	readFile := func(p string) (string, error) {
		b, err := os.ReadFile(p) //nolint:gosec // a file the person named
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	readEnv := func(name string) (string, error) {
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			return "", fmt.Errorf("the environment variable %s is not set", name)
		}
		return v, nil
	}
	if f.passwordEnv != "" && f.passwordFile != "" {
		return a, errors.New("give the password one way: --password-env or --password-file")
	}
	if f.keyFile != "" {
		b, err := os.ReadFile(expandHome(f.keyFile)) //nolint:gosec // a file the person named
		if err != nil {
			return a, fmt.Errorf("--key: %w", err)
		}
		a.Key = b
		if f.passphraseEnv != "" {
			v, err := readEnv(f.passphraseEnv)
			if err != nil {
				return a, fmt.Errorf("--passphrase-env: %w", err)
			}
			a.Passphrase = v
		} else if needsPassphrase(b) {
			if ask == nil {
				return a, errors.New("the key file needs its passphrase: give it with --passphrase-env")
			}
			v, err := ask.AskSecret("passphrase for " + f.keyFile)
			if err != nil {
				return a, err
			}
			a.Passphrase = v
		}
	}
	switch {
	case f.passwordEnv != "":
		v, err := readEnv(f.passwordEnv)
		if err != nil {
			return a, fmt.Errorf("--password-env: %w", err)
		}
		a.Password = v
	case f.passwordFile != "":
		v, err := readFile(expandHome(f.passwordFile))
		if err != nil {
			return a, fmt.Errorf("--password-file: %w", err)
		}
		a.Password = v
	}
	a.Agent = f.agent
	if len(a.Key) == 0 && a.Password == "" && !a.Agent {
		switch {
		case os.Getenv("SSH_AUTH_SOCK") != "":
			a.Agent = true
		case ask != nil:
			v, err := ask.AskSecret("password for " + who)
			if err != nil {
				return a, err
			}
			a.Password = v
		default:
			return a, errors.New("no admin sign-in: give --key FILE, --agent, --password-env or --password-file")
		}
	}
	switch {
	case f.sudoEnv != "" && f.sudoFile != "":
		return a, errors.New("give the sudo password one way: --sudo-password-env or --sudo-password-file")
	case f.sudoEnv != "":
		v, err := readEnv(f.sudoEnv)
		if err != nil {
			return a, fmt.Errorf("--sudo-password-env: %w", err)
		}
		a.Sudo = v
	case f.sudoFile != "":
		v, err := readFile(expandHome(f.sudoFile))
		if err != nil {
			return a, fmt.Errorf("--sudo-password-file: %w", err)
		}
		a.Sudo = v
	}
	return a, nil
}

func needsPassphrase(key []byte) bool {
	_, err := ssh.ParsePrivateKey(key)
	var missing *ssh.PassphraseMissingError
	return errors.As(err, &missing)
}

// parseSSHTarget reads ssh://[user@]host[:port].
func parseSSHTarget(s string) (targethost.Answer, error) {
	if !strings.HasPrefix(s, "ssh://") {
		return targethost.Answer{}, fmt.Errorf("%q: name the server as ssh://[user@]host[:port]", s)
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return targethost.Answer{}, fmt.Errorf("%q: name the server as ssh://[user@]host[:port]", s)
	}
	if _, has := u.User.Password(); has {
		return targethost.Answer{}, errors.New("a password in the address is refused: give it with --password-env or --password-file")
	}
	ans := u.Host
	if u.User != nil && u.User.Username() != "" {
		ans = u.User.Username() + "@" + ans
	}
	return targethost.ParseAnswer(ans)
}

func terminalAsker(cmd *cobra.Command) targethost.Asker {
	if stdinIsTTY(cmd.InOrStdin()) {
		return &setup.TTYPrompter{In: cmd.InOrStdin(), Err: cmd.ErrOrStderr()}
	}
	return nil
}

// runhostOwner is who a runhost set up from here belongs to: the account
// this computer is signed in to on the registry, or, signed out, the paired
// devices.
func runhostOwner(cmd *cobra.Command) runhost.Owner {
	reg := registryURL()
	token, err := credentialFor(reg)
	if err != nil || token == "" {
		return runhost.Owner{Kind: runhost.OwnerDevices}
	}
	client, err := newAPIClientChecked(cmd, reg, token)
	if err != nil {
		return runhost.Owner{Kind: runhost.OwnerDevices}
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
	defer cancel()
	resp, err := client.GetCurrentUserWithResponse(ctx)
	if err != nil || resp.JSON200 == nil {
		return runhost.Owner{Kind: runhost.OwnerDevices}
	}
	return runhost.Owner{Kind: runhost.OwnerAccount, Registry: reg, Handle: resp.JSON200.Handle}
}

type runhostAddOutput struct {
	Runhost      runhost.Record        `json:"runhost"`
	Findings     []rhsetup.Finding     `json:"preflight"`
	Restrictions []runhost.Restriction `json:"runKeyRefused"`
	Status       json.RawMessage       `json:"status,omitempty"`
	Doctor       json.RawMessage       `json:"doctor,omitempty"`
}

func newRunhostAddCmd() *cobra.Command {
	var af adminFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   "add <name> ssh://[user@]host[:port]",
		Short: "Set up a Linux server you own as a runhost, over SSH, and pair this computer with it",
		Long: "Signs in to the server with the admin sign-in, checks it without changing\n" +
			"anything, shows what it found, and then installs (each step can be run again):\n" +
			"  - an agentry account with no password and no shell of its own;\n" +
			"  - Docker Engine from Docker's repository, when the server has no engine (the\n" +
			"    agentry account joins the docker group, which makes it root-equivalent on\n" +
			"    the server; its run key can reach only the runhost's service);\n" +
			"  - gVisor (runsc), registered for every container and for the gateway;\n" +
			"  - this CLI's own signed release for the server's architecture: its signature\n" +
			"    checked here, its bytes checked again on the server;\n" +
			"  - the images runs need, and the runhost's service.\n" +
			"Then it pairs this computer: a run key and a sealing key made here and kept in\n" +
			"the system keychain, the run key installed to run nothing but the service. It\n" +
			"checks the result over the run key alone: the service's version and doctor,\n" +
			"the key's refusals, and that no network listener was added. The admin\n" +
			"sign-in is kept nowhere.\n\n" +
			"The server's host key is checked as a target host's is: a key in your\n" +
			"known_hosts or Agentry's is accepted, an unknown one is asked about at a\n" +
			"terminal, and a changed one is refused.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRunhostAdd(cmd, args[0], args[1], &af, yes)
		},
	}
	af.bind(cmd)
	cmd.Flags().BoolVar(&yes, "yes", false, "go on after the server's checks without asking")
	return cmd
}

func runRunhostAdd(cmd *cobra.Command, name, target string, af *adminFlags, yes bool) error {
	if err := runhost.ValidName(name); err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	answer, err := parseSSHTarget(target)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	home, err := runnerHome()
	if err != nil {
		return err
	}
	recs, err := runhost.LoadRecords(home)
	if err != nil {
		return err
	}
	if old, ok := recs.Find(name); ok && (old.Host != answer.Host || old.Port != answer.Port) {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("a runhost named %s is already %s:%d: remove it first, or choose another name", name, old.Host, old.Port))
	}
	ask := terminalAsker(cmd)
	auth, err := af.resolve(ask, answer.String())
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	keys, err := clirelease.Carried()
	if err != nil {
		return err
	}
	deviceID, err := runhost.DeviceID(home)
	if err != nil {
		return err
	}
	label, _ := os.Hostname()
	reach := []string{"ghcr.io", rhsetup.DockerRepoHost}
	if u, err := url.Parse(registryURL()); err == nil && u.Hostname() != "" && u.Hostname() != "localhost" {
		reach = append(reach, u.Hostname())
	}
	say := func(s string) { _, _ = fmt.Fprintln(narration(cmd), s) }
	o := rhsetup.Options{
		Name: name, Answer: answer, Auth: auth,
		KnownHosts: targethost.DefaultKnownHostsFiles(home),
		Ask:        ask, Say: say,
		Version: version.Version, Keys: keys,
		Owner:    runhostOwner(cmd),
		DeviceID: deviceID, DeviceLabel: label,
		Reach: reach,
	}
	if !yes {
		if ask == nil {
			return engine.Classify(engine.ExitUsage, errors.New("no one is at a terminal to confirm the install: pass --yes"))
		}
		o.Confirm = func(p rhsetup.Plan) (bool, error) {
			what := "the agentry account, gVisor, this CLI and the runhost's service"
			if p.InstallDocker {
				what = "Docker Engine, " + what
			}
			return ask.Confirm(fmt.Sprintf("Install %s on %s?", what, answer.Host), false)
		}
	}
	out, err := rhsetup.Add(cmd.Context(), o)
	if out != nil && out.Record.Name != "" {
		recs.Put(out.Record)
		if serr := recs.Save(home); serr != nil {
			return errors.Join(err, serr)
		}
	}
	if err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	if flags.jsonOut {
		doc := runhostAddOutput{Runhost: out.Record, Findings: out.Plan.Findings, Restrictions: out.Restrictions, Status: out.Status, Doctor: out.Doctor.Result}
		return emitJSON(cmd.OutOrStdout(), doc)
	}
	w := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(w, "runhost %s is ready: %s, CLI %s, owned by %s\n", name, out.Record.OS, out.Record.CLIVersion, out.Record.Owner)
	for _, r := range out.Restrictions {
		_, _ = fmt.Fprintf(w, "  the run key is refused %s\n", r.What)
	}
	printNextSteps(cmd.ErrOrStderr(),
		nextStep{command: "agentry runhost status " + name, notes: []string{"the runhost's state, over this computer's run key"}},
		nextStep{command: "agentry runhost doctor " + name, notes: []string{"its engine, gVisor, images and service, checked on the server"}})
	return nil
}

func newRunhostListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List this computer's runhosts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := runnerHome()
			if err != nil {
				return err
			}
			recs, err := runhost.LoadRecords(home)
			if err != nil {
				return err
			}
			if recs.Runhosts == nil {
				recs.Runhosts = []runhost.Record{}
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), map[string]any{"runhosts": recs.Runhosts})
			}
			if len(recs.Runhosts) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no runhosts yet: `agentry runhost add <name> ssh://user@host` sets one up")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "NAME\tADDRESS\tSERVER\tCLI\tOWNER")
			for _, r := range recs.Runhosts {
				_, _ = fmt.Fprintf(tw, "%s\t%s:%d\t%s (%s)\t%s\t%s\n", r.Name, r.Host, r.Port, r.OS, r.Arch, r.CLIVersion, r.Owner)
			}
			return tw.Flush()
		},
	}
}

// askRunhost asks the runhost name's service verb over this device's run
// key.
func askRunhost(cmd *cobra.Command, name, verb string) (runhost.Answer, *runhost.Record, error) {
	home, err := runnerHome()
	if err != nil {
		return runhost.Answer{}, nil, err
	}
	recs, err := runhost.LoadRecords(home)
	if err != nil {
		return runhost.Answer{}, nil, err
	}
	rec, ok := recs.Find(name)
	if !ok {
		return runhost.Answer{}, nil, engine.Classify(engine.ExitUsage, fmt.Errorf("no runhost named %s on this computer (`agentry runhost list`)", name))
	}
	keys, err := runhost.LoadKeys(name)
	if err != nil {
		return runhost.Answer{}, rec, err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
	defer cancel()
	c, err := runhost.DialRunhost(ctx, *rec, keys)
	if err != nil {
		return runhost.Answer{}, rec, err
	}
	defer func() { _ = c.Close() }()
	a, err := c.Ask(ctx, verb, nil, func(s string) { _, _ = fmt.Fprintln(narration(cmd), s) })
	return a, rec, err
}

func newRunhostStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <name>",
		Short: "Show a runhost's state, over this computer's run key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, _, err := askRunhost(cmd, args[0], "status")
			if err != nil {
				return err
			}
			if a.Code != 0 {
				return engine.Classify(engine.ExitCLI, fmt.Errorf("%s: %s", args[0], a.Error))
			}
			if flags.jsonOut {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), string(a.Result))
				return err
			}
			var st runhostStatus
			if err := json.Unmarshal(a.Result, &st); err != nil {
				return err
			}
			printRunhostStatus(cmd.OutOrStdout(), st)
			return nil
		},
	}
}

func printRunhostStatus(w io.Writer, st runhostStatus) {
	_, _ = fmt.Fprintf(w, "runhost:   %s\nserver:    %s (%s)\nCLI:       %s\ngVisor:    %s\nowner:     %s\nUnsafe Mode: %s\n", st.Name, st.OS, st.Arch, st.Version, st.GVisor, st.Owner, onOff(st.UnsafeMode))
	_, _ = fmt.Fprintf(w, "runtimes:  runs under %s, the gateway under %s\n", nonEmptyOr(st.Runtime, "(none)"), nonEmptyOr(st.GatewayRuntime, "(none)"))
	_, _ = fmt.Fprintf(w, "devices:   %d paired\n", len(st.Devices))
	for _, d := range st.Devices {
		_, _ = fmt.Fprintf(w, "  %s (%s), paired %s\n", d.Label, d.ID, d.PairedAt)
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func nonEmptyOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func newRunhostDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor <name>",
		Short: "Check a runhost: its engine, gVisor for every container, its images and its service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, _, err := askRunhost(cmd, args[0], "doctor")
			if err != nil {
				return err
			}
			rep := doctorReport{Ref: "runhost " + args[0]}
			if len(a.Result) > 0 {
				if err := json.Unmarshal(a.Result, &rep); err != nil {
					return err
				}
			}
			if flags.jsonOut {
				if err := emitJSON(cmd.OutOrStdout(), rep); err != nil {
					return err
				}
			} else {
				printDoctorReport(cmd.OutOrStdout(), &rep)
			}
			if a.Code != 0 {
				err := engine.Classify(engine.ExitCLI, fmt.Errorf("runhost %s: %s", args[0], nonEmptyOr(a.Error, "doctor failed")))
				if flags.jsonOut {
					return err
				}
				return &reportedError{err: err}
			}
			return nil
		},
	}
}

func newRunhostRemoveCmd() *cobra.Command {
	var af adminFlags
	var user string
	var removeImages, removeDocker, yes bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Take a runhost off its server, with the admin sign-in, and forget it here",
		Long: "Stops the runhost's service and removes the agentry account with everything\n" +
			"in its home, and takes gVisor's runtimes out of the Docker daemon. Docker\n" +
			"Engine and the images stay unless you choose otherwise: --remove-images, and\n" +
			"--remove-docker, which removes Docker Engine only when setup installed it.\n" +
			"Then this computer's keys for the runhost and its record are removed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			home, err := runnerHome()
			if err != nil {
				return err
			}
			recs, err := runhost.LoadRecords(home)
			if err != nil {
				return err
			}
			rec, ok := recs.Find(name)
			if !ok {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("no runhost named %s on this computer", name))
			}
			if user == "" {
				return engine.Classify(engine.ExitUsage, errors.New("name the admin account with --user (root, or one that can sudo)"))
			}
			ask := terminalAsker(cmd)
			if !yes {
				if ask == nil {
					return engine.Classify(engine.ExitUsage, errors.New("no one is at a terminal to confirm: pass --yes"))
				}
				ok, err := ask.Confirm(fmt.Sprintf("Remove runhost %s from %s?", name, rec.Host), false)
				if err != nil || !ok {
					return errors.Join(err, errors.New("nothing removed"))
				}
				if !cmd.Flags().Changed("remove-images") {
					if removeImages, err = ask.Confirm("Remove the images runs used as well?", false); err != nil {
						return err
					}
				}
				if !cmd.Flags().Changed("remove-docker") {
					if removeDocker, err = ask.Confirm("Remove Docker Engine as well, if setup installed it?", false); err != nil {
						return err
					}
				}
			}
			auth, err := af.resolve(ask, user+"@"+rec.Host)
			if err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			say := func(s string) { _, _ = fmt.Fprintln(narration(cmd), s) }
			err = rhsetup.Remove(cmd.Context(), rhsetup.RemoveOptions{
				Record: *rec, Auth: auth, Answer: targethost.Answer{User: user, Host: rec.Host, Port: rec.Port},
				Ask: ask, Say: say, RemoveImages: removeImages, RemoveDocker: removeDocker,
			})
			if err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			if err := runhost.DeleteKeys(name); err != nil {
				say("warning: this computer's keys for " + name + " could not be removed from the system keychain: " + err.Error())
			}
			recs.Drop(name)
			if err := recs.Save(home); err != nil {
				return err
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), map[string]any{"removed": name, "images": removeImages, "docker": removeDocker})
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "runhost %s removed from %s\n", name, rec.Host)
			return nil
		},
	}
	af.bind(cmd)
	cmd.Flags().StringVar(&user, "user", "", "the admin account to sign in as (root, or one that can sudo)")
	cmd.Flags().BoolVar(&removeImages, "remove-images", false, "remove the images runs used")
	cmd.Flags().BoolVar(&removeDocker, "remove-docker", false, "remove Docker Engine, when setup installed it")
	cmd.Flags().BoolVar(&yes, "yes", false, "remove without asking")
	return cmd
}

// newRunhostServeCmd is the run key's forced command on the server.
func newRunhostServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "serve",
		Short:  "The run key's forced command on a runhost (run by sshd)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The answer's exit frame has said why; the status is the frame's.
			os.Exit(runhost.Serve(cmd.Context(), os.Getenv("SSH_ORIGINAL_COMMAND"), cmd.InOrStdin(), cmd.OutOrStdout(), runhost.ServeSocket()))
			return nil
		},
	}
}

// runServeAsLoginShell is serve when sshd runs the CLI as the agentry
// account's login shell: it never returns.
func runServeAsLoginShell(ctx context.Context) {
	os.Exit(runhost.Serve(ctx, os.Getenv("SSH_ORIGINAL_COMMAND"), bufio.NewReader(os.Stdin), os.Stdout, runhost.ServeSocket()))
}
