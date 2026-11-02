package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/payload"
	"github.com/blaktron/agentry-cli/pkg/runner/deliver"
	"github.com/blaktron/agentry-cli/pkg/runner/destination"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/intake"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/profile"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
	"github.com/blaktron/agentry/pkg/manifest"
)

// What a run on a remote runhost carries beside its release, gathered on the
// device exactly as a local run gathers it: the MCP profile, the intake's
// answers and its sources' tokens, the declared secrets, the OAuth servers'
// connections, the files, the Writing Voice, the report design and the
// destinations. The values travel in the job document and the files in its
// payload; nothing of them is kept on the runhost once the run ends.

// runOnCarried is the job's share of what the device gathered, the payload's
// files, and what the device does once the output is back.
type runOnCarried struct {
	run       job.Run
	entries   []payload.Entry
	deliverTo string
	prof      *profile.Profile
	profDir   string
	keep      bool
	cleanups  []func()
}

// done removes every file the device wrote for the payload, and the packs
// it opened from its store.
func (c *runOnCarried) done() {
	for i := len(c.cleanups) - 1; i >= 0; i-- {
		c.cleanups[i]()
	}
}

// runOnCarry gathers what the run carries. A secret the release needs and
// this device has no value for is refused here, before anything is sent, as
// a local run refuses it before any container starts.
func runOnCarry(cmd *cobra.Command, home string, meta *installdir.Meta, m *manifest.Manifest, pol *policy.Policy, opts runOpts) (_ *runOnCarried, err error) {
	c := &runOnCarried{}
	defer func() {
		if err != nil {
			c.done()
		}
	}()
	scratch, err := os.MkdirTemp("", "agentry-runhost-carry-*")
	if err != nil {
		return nil, err
	}
	c.cleanups = append(c.cleanups, func() { _ = os.RemoveAll(scratch) })

	cfg, cerr := loadCLIConfig()
	if cerr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "agentry: warning: %v; its report settings are not read\n", cerr)
	}
	theme, err := chooseReportTheme(cmd.Context(), reportFlags{theme: opts.reportTheme, design: opts.reportDesign}, cfg, readAccountTheme(registryURL()), cmd.ErrOrStderr())
	if err != nil {
		return nil, err
	}
	switch {
	case len(theme.Design) > 0:
		p := filepath.Join(scratch, "report-design.json")
		if err := os.WriteFile(p, theme.Design, 0o600); err != nil {
			return nil, err
		}
		c.run.ReportDesign = payload.KindDesign + "/report-design.json"
		c.entries = append(c.entries, payload.Entry{Name: c.run.ReportDesign, Src: p})
	case theme.Name != "":
		c.run.ReportTheme = theme.Name
	}
	if c.deliverTo, err = chooseDeliverFolder(opts.deliverTo, cfg); err != nil {
		return nil, err
	}

	if err := c.carryFiles(opts.files); err != nil {
		return nil, err
	}
	packs, detach, err := attachKnowledge(cmd, opts.knowledge)
	c.cleanups = append(c.cleanups, detach)
	if err != nil {
		return nil, err
	}
	for _, p := range packs {
		name := payload.KindKnowledge + "/" + p.Name
		c.run.Knowledge = append(c.run.Knowledge, name)
		c.entries = append(c.entries, payload.Entry{Name: name, Src: p.Dir})
	}
	voice, err := attachVoice(cmd, opts)
	if err != nil {
		return nil, err
	}
	if voice != nil {
		p := filepath.Join(scratch, "VOICE.md")
		if err := os.WriteFile(p, voice.Text, 0o600); err != nil {
			return nil, err
		}
		c.run.Voice = payload.KindVoice + "/VOICE.md"
		c.entries = append(c.entries, payload.Entry{Name: c.run.Voice, Src: p})
	}

	if err := c.carryDestinations(cmd, opts, meta.Ref); err != nil {
		return nil, err
	}

	prof, profDir, keep, err := ensureProfile(cmd, home, meta, m, pol, opts)
	if err != nil {
		return nil, engine.Classify(engine.ExitPolicy, err)
	}
	c.prof, c.profDir, c.keep = prof, profDir, keep
	var profSecrets map[string]string
	if prof != nil {
		profSecrets = prof.Secrets
	}
	conns, err := ensureConnections(cmd, home, meta, m)
	if err != nil {
		return nil, err
	}
	for server, t := range conns.tokens {
		if c.run.Connections == nil {
			c.run.Connections = map[string]job.Connection{}
		}
		c.run.Connections[server] = job.Connection{AccessToken: t.AccessToken, ExpiresAt: t.ExpiresAt, Issuer: conns.issuers[server], Scopes: conns.scopes[server]}
	}

	answers, err := collectIntake(cmd, m, opts, func(name string) bool {
		return engine.SecretResolvable(name, profSecrets, pol.SecretSource)
	})
	if err != nil {
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	if err := resolveSourceTokens(cmd, answers, opts); err != nil {
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	if err := c.carryIntake(answers); err != nil {
		return nil, err
	}
	if err := c.carryTargetHosts(cmd, home, answers, m, opts); err != nil {
		return nil, err
	}
	c.run.HostsAllow, c.run.HostsDeny = pol.Hosts.Allow, pol.Hosts.Deny

	overrides := secretOverrides(profSecrets, answers)
	for _, name := range allSecretNames(m) {
		v, ok := engine.ResolveSecret(name, overrides, pol.SecretSource)
		switch {
		case ok:
			if !job.SecretValueOK(v) {
				return nil, engine.Classify(engine.ExitPolicy, fmt.Errorf("the secret %s is not one line with no space at either end, which a run on a runhost carries as it is", name))
			}
			if c.run.Secrets == nil {
				c.run.Secrets = map[string]string{}
			}
			c.run.Secrets[name] = v
		case m.Capabilities.IsOptionalSecret(name):
		default:
			return nil, engine.Classify(engine.ExitPolicy, fmt.Errorf("%s needs the secret %s, and this device has none: set $%s, or answer it in the MCP profile (--reconfigure-profile on a terminal)", meta.Ref, name, name))
		}
	}
	c.run.AllowVulnerableMCP = opts.allowVulnerableMCP
	return c, nil
}

// carryFiles adds each --file to the payload under inputs/, refusing two of
// one name, which a run stages side by side.
func (c *runOnCarried) carryFiles(files []string) error {
	seen := map[string]bool{}
	for _, f := range files {
		abs, err := filepath.Abs(f)
		if err != nil {
			return engine.Classify(engine.ExitCLI, err)
		}
		if _, err := os.Stat(abs); err != nil {
			return engine.Classify(engine.ExitCLI, fmt.Errorf("input file: %w", err))
		}
		base := filepath.Base(abs)
		if seen[base] {
			return engine.Classify(engine.ExitCLI, fmt.Errorf("two inputs share the name %q: rename one", base))
		}
		seen[base] = true
		name := payload.KindInputs + "/" + base
		if payload.CleanName(name) == "" {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("the input %q has a name a run on a runhost cannot carry", base))
		}
		c.run.Inputs = append(c.run.Inputs, name)
		c.entries = append(c.entries, payload.Entry{Name: name, Src: abs})
	}
	return nil
}

// carryIntake puts the intake's answers in the job as --intake takes them:
// a file answer's file in the payload, an input source's address with its
// ref, and its token beside it. A secret answer travels among the secrets.
func (c *runOnCarried) carryIntake(a *intake.Answers) error {
	if a == nil {
		return nil
	}
	put := func(id, v string) {
		if c.run.Intake == nil {
			c.run.Intake = map[string]string{}
		}
		c.run.Intake[id] = v
	}
	for id, v := range a.Values {
		put(id, v)
	}
	for id, h := range a.Hosts {
		put(id, h.Answer.String())
	}
	for id, src := range a.Sources {
		answer := src.URL
		if src.Ref != "" {
			answer += "#" + src.Ref
		}
		put(id, answer)
		if src.Token != "" {
			if c.run.SourceTokens == nil {
				c.run.SourceTokens = map[string]string{}
			}
			c.run.SourceTokens[id] = src.Token
		}
	}
	for id, path := range a.Files {
		name := payload.KindIntake + "/" + id + "/" + filepath.Base(path)
		if payload.CleanName(name) == "" {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("the file answering %s has a name a run on a runhost cannot carry", id))
		}
		if c.run.IntakeFiles == nil {
			c.run.IntakeFiles = map[string]string{}
		}
		c.run.IntakeFiles[id] = name
		c.entries = append(c.entries, payload.Entry{Name: name, Src: path})
	}
	return nil
}

// carryTargetHosts is each answered target host's sign-in, resolved as a
// local run resolves it (a flag, the keychain, a prompt on a terminal), and
// the known_hosts lines of the keys this device has accepted for it. The
// runhost resolves the address and reads the key itself, from its own
// network, and takes only a key this device knows: one it does not is
// refused here, since no one is at the runhost to accept it.
func (c *runOnCarried) carryTargetHosts(cmd *cobra.Command, home string, a *intake.Answers, m *manifest.Manifest, opts runOpts) error {
	ids := m.Capabilities.TargetHosts
	flags, err := targethost.ParseFlags(opts.hostFlags.key, opts.hostFlags.passphrase, opts.hostFlags.password, opts.hostFlags.sudo)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	if unknown := flags.Unknown(ids); len(unknown) > 0 {
		slices.Sort(unknown)
		return engine.Classify(engine.ExitUsage, fmt.Errorf("a --host-* flag names %v, which is none of this QuickAgent's target hosts (%v)", unknown, ids))
	}
	if a == nil || len(ids) == 0 {
		return nil
	}
	var ask targethost.Asker
	if stdinIsTTY(cmd.InOrStdin()) {
		ask = &setup.TTYPrompter{In: cmd.InOrStdin(), Err: cmd.ErrOrStderr()}
	}
	say := func(msg string) { _, _ = fmt.Fprintln(cmd.ErrOrStderr(), msg) }
	files := targethost.DefaultKnownHostsFiles(home)
	for _, id := range ids {
		h := a.Hosts[id]
		if h == nil {
			// Never answered: the runhost's run refuses it by name.
			continue
		}
		known, err := targethost.KnownLines(files, h.Answer)
		if err != nil {
			return engine.Classify(engine.ExitCLI, err)
		}
		if len(known) == 0 {
			return engine.Classify(engine.ExitPolicy, receipt.WithReason(fmt.Errorf(
				"target host %s: this device has not accepted a host key for %s, and the runhost, which reaches it, has no one to ask: run a QuickAgent on %s from this computer once, or add its key to your known_hosts", id, h.Answer.KnownHostsName(), h.Answer.String()),
				receipt.ReasonHostRefused, targethost.RefusedKeyUnknown, id))
		}
		if len(known) > job.MaxKnownLines {
			known = known[:job.MaxKnownLines]
		}
		cred, err := targethost.ResolveCredential(id, h.Answer, flags, os.Getenv, ask, say)
		if err != nil {
			var r *targethost.Refusal
			if errors.As(err, &r) {
				return engine.Classify(engine.ExitPolicy, receipt.WithReason(err, receipt.ReasonHostRefused, r.Detail, id))
			}
			return engine.Classify(engine.ExitCLI, err)
		}
		t := job.TargetHost{Known: known, Passphrase: cred.Passphrase, Password: cred.Password, Sudo: cred.Sudo}
		if cred.Auth == hostlink.AuthKey {
			t.Key, t.Password = string(cred.Key), ""
		}
		if c.run.TargetHosts == nil {
			c.run.TargetHosts = map[string]job.TargetHost{}
		}
		c.run.TargetHosts[id] = t
		say(fmt.Sprintf("target host %s: %s, %s; the runhost resolves its address and checks its key against the %d this device has accepted", id, h.Answer.String(), cred.Words(), len(known)))
	}
	return nil
}

// carryDestinations is the destinations the run delivers to, read with their
// credentials as a local run reads them. A destination on the person's own
// network is refused: the runhost delivers from its own network, which is not
// this computer's.
func (c *runOnCarried) carryDestinations(cmd *cobra.Command, opts runOpts, ref string) error {
	if opts.noDestination {
		return nil
	}
	dests, fromDefault, err := chooseDestinations(opts.destinations, ref, false)
	if err != nil {
		return err
	}
	for _, d := range dests {
		if d.OnMyNetwork {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("the destination %s is on your own network, and a runhost delivers from its own: a run on a runhost does not take it yet", d.Name))
		}
		if d.Kind != destination.KindWebhook && d.Kind != destination.KindS3 {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("the destination %s is a %s, and a runhost delivers to webhooks and buckets", d.Name, d.Kind))
		}
		c.run.Destinations = append(c.run.Destinations, job.Destination{Name: d.Name, Kind: d.Kind, URL: d.URL, Bucket: d.Bucket, Prefix: d.Prefix,
			Region: d.Region, Endpoint: d.Endpoint, Secret: d.Secret, Authorization: d.Authorization, AccessKeyID: d.AccessKeyID})
	}
	if fromDefault && len(dests) > 0 {
		names := make([]string, len(dests))
		for i, d := range dests {
			names[i] = d.Name
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "delivering to %s from the runhost when the run succeeds (the default set you chose for %s)\n", strings.Join(names, ", "), deliver.AgentName(ref))
	}
	return nil
}

// pack packs the payload, when the run carries any files, and names it in
// the job.
func (c *runOnCarried) pack() (*payload.Packed, error) {
	if len(c.entries) == 0 {
		return nil, nil
	}
	p, err := payload.Pack(c.entries)
	if err != nil {
		return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("pack the run's files: %w", err))
	}
	c.cleanups = append(c.cleanups, p.Remove)
	c.run.Payload = &job.Payload{Size: p.Size, Digest: p.Digest}
	return p, nil
}

// finishProfile settles the MCP profile once the job is sent: its secrets
// travelled with the job, and a profile the person did not keep goes.
func (c *runOnCarried) finishProfile(errOut io.Writer) {
	finishProfile(errOut, c.prof, c.profDir, c.keep)
}
