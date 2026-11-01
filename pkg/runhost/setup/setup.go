// Package setup makes a Linux server the person owns into a remote runhost,
// over SSH, and takes it away again. The admin sign-in is used for this
// alone and kept nowhere. Setup reads the server first and changes nothing
// until the person has seen what it found; then it installs, idempotently, a
// dedicated account, Docker Engine where there is none, gVisor's runsc
// registered for every container, this device's own CLI release, and the
// runhost's service; pairs this device with a run key held to the service by
// a forced command; and checks the result over that run key alone.
package setup

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/clirelease"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
)

var (
	//go:embed assets/preflight.sh
	preflightScript []byte
	//go:embed assets/install.sh
	installScript []byte
	//go:embed assets/remove.sh
	removeScript []byte
	//go:embed assets/docker.asc
	dockerKey []byte
)

// unitFile is the service's systemd user unit.
const unitFile = `[Unit]
Description=Agentry remote runhost service

[Service]
ExecStart=` + runhost.CLIPath + ` runhost service
Restart=on-failure
RestartSec=5
NoNewPrivileges=yes

[Install]
WantedBy=default.target
`

// releaseVersionRE is the shape of a released CLI version: what a device
// can find signed sums for.
var releaseVersionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-dev\.[0-9]+)?$`)

// ErrNotARelease means this CLI is not a release, so there is no signed
// build of it to put on the server.
var ErrNotARelease = errors.New("this CLI is not a signed release, so there is no build of its version to put on a server: set up a runhost with a released CLI")

// Options are what Add needs.
type Options struct {
	// Name is the runhost's name on this device.
	Name string
	// Answer is the server's SSH address and admin account.
	Answer targethost.Answer
	// Auth is the admin sign-in.
	Auth AdminAuth
	// KnownHosts are where the server's host key is looked up.
	KnownHosts targethost.KnownHostsFiles
	// Ask asks the person at a terminal; nil is no one to ask, so an unknown
	// host key or a missing sudo password refuses.
	Ask targethost.Asker
	// Say narrates, one line at a time.
	Say func(string)
	// Confirm is shown the preflight's plan before anything on the server
	// changes; false stops setup there. Nil goes on.
	Confirm func(Plan) (bool, error)
	// Version is this device's CLI version, which the server gets.
	Version string
	// Keys are the update-signing keys the CLI release is checked against.
	Keys *clirelease.KeySet
	// HTTP downloads the CLI release and gVisor.
	HTTP *http.Client
	// Owner is who the runhost will belong to.
	Owner runhost.Owner
	// DeviceID and DeviceLabel name this device to the server.
	DeviceID, DeviceLabel string
	// Reach are the hosts whose port 443 the server must reach: the image
	// registry and the Agentry registry.
	Reach []string

	// FetchCLI and FetchGVisor default to the real downloads; tests replace
	// them.
	FetchCLI    func(ctx context.Context, version, arch string) (*clirelease.Binary, error)
	FetchGVisor func(ctx context.Context, arch string) ([]byte, error)
	// Now is the clock, for the record's dates.
	Now func() time.Time
}

func (o *Options) say(format string, args ...any) {
	if o.Say != nil {
		o.Say(fmt.Sprintf(format, args...))
	}
}

// Outcome is a finished setup.
type Outcome struct {
	Record       runhost.Record
	Plan         Plan
	Restrictions []runhost.Restriction
	Status       json.RawMessage
	Doctor       runhost.Answer
}

// Add sets the server up as a runhost and pairs this device with it. The
// record it returns is not yet saved: the caller saves it.
func Add(ctx context.Context, o Options) (*Outcome, error) {
	if err := runhost.ValidName(o.Name); err != nil {
		return nil, err
	}
	if !releaseVersionRE.MatchString(o.Version) {
		return nil, ErrNotARelease
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTP == nil {
		o.HTTP = http.DefaultClient
	}
	if o.FetchCLI == nil {
		o.FetchCLI = func(ctx context.Context, v, arch string) (*clirelease.Binary, error) {
			return clirelease.Fetch(ctx, o.HTTP, o.Keys, v, arch)
		}
	}
	if o.FetchGVisor == nil {
		o.FetchGVisor = func(ctx context.Context, arch string) ([]byte, error) { return FetchGVisor(ctx, o.HTTP, arch) }
	}
	if o.Keys.Empty() {
		return nil, clirelease.ErrNoKeys
	}

	addr, err := targethost.ResolveAddress(ctx, o.Name, o.Answer, policy.HostsPolicy{})
	if err != nil {
		return nil, err
	}
	hk, err := targethost.CheckHostKey(ctx, o.Name, o.Answer, addr, o.KnownHosts, o.Ask, func(s string) { o.say("%s", s) })
	if err != nil {
		return nil, err
	}
	o.say("host key: %s (%s)", targethost.Fingerprint(hk.Key), hk.From)

	conn, err := Dial(ctx, addr, o.Answer.Port, o.Answer.User, hk.Key, o.Auth)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.FindRoot(ctx); errors.Is(err, ErrSudoPassword) {
		if o.Ask == nil {
			return nil, fmt.Errorf("%w for %s, and no one is at a terminal to type it: give it with --sudo-password-env or --sudo-password-file", err, o.Answer.String())
		}
		pw, aerr := o.Ask.AskSecret(fmt.Sprintf("sudo password for %s: ", o.Answer.String()))
		if aerr != nil {
			return nil, aerr
		}
		if err := conn.SetSudo(ctx, pw); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	stage, err := conn.Stage(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Unstage(context.WithoutCancel(ctx), stage)

	o.say("checking the server (nothing changes yet)")
	facts, err := runPreflight(ctx, conn, stage, o.Reach)
	if err != nil {
		return nil, err
	}
	plan := Evaluate(facts, o.Reach)
	for _, f := range plan.Findings {
		o.say("  %-5s %-9s %s", f.Level, f.Name, f.Detail)
	}
	if !plan.Supported {
		return &Outcome{Plan: plan}, fmt.Errorf("%s cannot be a runhost: %s", o.Answer.Host, stops(plan))
	}
	if o.Confirm != nil {
		ok, err := o.Confirm(plan)
		if err != nil {
			return &Outcome{Plan: plan}, err
		}
		if !ok {
			return &Outcome{Plan: plan}, errors.New("setup stopped before changing anything on the server")
		}
	}

	owner, err := ownerFor(ctx, conn, o.Owner)
	if err != nil {
		return &Outcome{Plan: plan}, err
	}

	o.say("downloading this CLI's release for %s and gVisor %s, and checking both", plan.Arch, GVisorRelease)
	cli, err := o.FetchCLI(ctx, o.Version, plan.Arch)
	if err != nil {
		return &Outcome{Plan: plan}, err
	}
	gv, err := o.FetchGVisor(ctx, facts["arch"])
	if err != nil {
		return &Outcome{Plan: plan}, err
	}

	keys, err := runhost.NewDeviceKeys()
	if err != nil {
		return &Outcome{Plan: plan}, err
	}
	runPub, err := keys.RunPublic()
	if err != nil {
		return &Outcome{Plan: plan}, err
	}

	files := map[string][]byte{
		"install.sh":              installScript,
		"agentry":                 cli.Bytes,
		"gvisor.tar.gz":           gv,
		"docker.asc":              dockerKey,
		"agentry-runhost.service": []byte(unitFile),
	}
	existing, err := conn.ReadRoot(ctx, "/etc/docker/daemon.json")
	if err != nil {
		return &Outcome{Plan: plan}, err
	}
	if merged, changed, err := MergeRuntimes(existing); err != nil {
		return &Outcome{Plan: plan}, err
	} else if changed {
		files["daemon.json"] = merged
	}
	oldKeys, err := conn.ReadRoot(ctx, runhost.AuthorizedKeys)
	if err != nil {
		return &Outcome{Plan: plan}, err
	}
	files["authorized_keys"] = mergeAuthorizedKeys(oldKeys, runhost.AuthorizedKeyLine(runPub, o.DeviceID), o.DeviceID)
	oldDevices, err := conn.ReadRoot(ctx, runhost.DevicesFile)
	if err != nil {
		return &Outcome{Plan: plan}, err
	}
	now := o.Now().UTC().Format(time.RFC3339)
	devices, err := mergeDevices(oldDevices, runhost.Device{ID: o.DeviceID, Label: o.DeviceLabel, RunKey: runPub, SealKey: keys.SealPublic(), PairedAt: now})
	if err != nil {
		return &Outcome{Plan: plan}, err
	}
	files["devices.json"] = devices
	cfg, _ := json.MarshalIndent(runhost.ServerConfig{Name: o.Name, Owner: owner, InstalledDocker: plan.InstallDocker, GVisor: GVisorRelease, SetUpAt: now}, "", "  ")
	files["config.json"] = cfg
	env := envFile(map[string]string{
		"STAGE":           stage,
		"AGENTRY_VERSION": o.Version,
		"AGENTRY_SHA256":  cli.SHA256,
		"GVISOR_RELEASE":  GVisorRelease,
		"INSTALL_DOCKER":  yesNo(plan.InstallDocker),
		"OS_ID":           facts["os_id"],
		"OS_CODENAME":     facts["os_codename"],
	})
	files["env"] = env
	for _, name := range sortedKeys(files) {
		if err := conn.Upload(ctx, stage, name, files[name]); err != nil {
			return &Outcome{Plan: plan}, err
		}
	}

	if err := runhost.SaveKeys(o.Name, keys); err != nil {
		return &Outcome{Plan: plan}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = runhost.DeleteKeys(o.Name)
		}
	}()

	o.say("installing")
	after, err := runScript(ctx, conn, "sh "+shellQuote(stage+"/install.sh")+" "+shellQuote(stage+"/env"), o.say)
	if err != nil {
		return &Outcome{Plan: plan}, fmt.Errorf("the install stopped: %w", err)
	}
	if added := NewListeners(facts.Listening(), strings.Fields(after["listening_after"])); len(added) > 0 {
		return &Outcome{Plan: plan}, fmt.Errorf("the install opened a network listener it should not have: %s", strings.Join(added, " "))
	}
	conn.Unstage(ctx, stage)
	_ = conn.Close()

	rec := runhost.Record{
		Name:               o.Name,
		Host:               o.Answer.Host,
		Port:               o.Answer.Port,
		HostKey:            strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hk.Key))),
		HostKeyFingerprint: targethost.Fingerprint(hk.Key),
		Owner:              owner,
		DeviceID:           o.DeviceID,
		CLIVersion:         o.Version,
		Arch:               plan.Arch,
		OS:                 facts["os_pretty"],
		GVisor:             GVisorRelease,
		AddedAt:            now,
	}
	out := &Outcome{Record: rec, Plan: plan}

	o.say("checking the runhost over this device's run key alone")
	rc, err := runhost.DialRunhost(ctx, rec, keys)
	if err != nil {
		return out, err
	}
	defer func() { _ = rc.Close() }()
	v, err := rc.Ask(ctx, "version", nil, nil)
	if err != nil {
		return out, err
	}
	var ver struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(v.Result, &ver); err != nil || ver.Version != o.Version {
		return out, fmt.Errorf("the runhost's service reports version %q, not %s", ver.Version, o.Version)
	}
	st, err := rc.Ask(ctx, "status", nil, nil)
	if err != nil {
		return out, err
	}
	out.Status = st.Result
	out.Restrictions = rc.CheckRestrictions(ctx)
	for _, r := range out.Restrictions {
		if !r.Refused {
			return out, fmt.Errorf("the run key was not refused %s: %s", r.What, r.Detail)
		}
	}
	doc, err := rc.Ask(ctx, "doctor", nil, nil)
	if err != nil {
		return out, err
	}
	out.Doctor = doc
	keep = true
	if doc.Code != 0 {
		return out, fmt.Errorf("the runhost is installed, but its doctor is not green: %s", nonEmpty(doc.Error, "see its checks"))
	}
	return out, nil
}

func stops(p Plan) string {
	var s []string
	for _, f := range p.Findings {
		if f.Level == LevelStop {
			s = append(s, f.Detail)
		}
	}
	return strings.Join(s, "; ")
}

func runPreflight(ctx context.Context, conn *Conn, stage string, reach []string) (Facts, error) {
	if err := conn.Upload(ctx, stage, "preflight.sh", preflightScript); err != nil {
		return nil, err
	}
	for _, h := range reach {
		if !hostRE.MatchString(h) {
			return nil, fmt.Errorf("an unsafe host name %q", h)
		}
	}
	cmd := "AGENTRY_PREFLIGHT_HOSTS=" + shellQuote(strings.Join(reach, " ")) + " sh " + shellQuote(stage+"/preflight.sh")
	r, err := conn.RunRoot(ctx, cmd, nil)
	if err != nil {
		return nil, err
	}
	if r.Code != 0 {
		return nil, fmt.Errorf("the preflight did not run on the server: %s", strings.TrimSpace(string(r.Stderr)))
	}
	return ParseFacts(r.Stdout), nil
}

var hostRE = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)

// runScript runs a root script and hands its `step:` lines to say. It
// returns the `fact:` lines it printed.
func runScript(ctx context.Context, conn *Conn, cmd string, say func(string, ...any)) (Facts, error) {
	pr, pw := io.Pipe()
	facts := Facts{}
	var lastErr string
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "step: "):
				say("  %s", strings.TrimPrefix(line, "step: "))
			case strings.HasPrefix(line, "error: "):
				lastErr = strings.TrimPrefix(line, "error: ")
			case strings.HasPrefix(line, "fact: "):
				for k, v := range ParseFacts([]byte(line)) {
					facts[k] = v
				}
			}
		}
		_, _ = io.Copy(io.Discard, pr)
	}()
	r, err := conn.RunRoot(ctx, cmd, pw)
	_ = pw.Close()
	<-done
	if err != nil {
		return facts, err
	}
	if r.Code != 0 {
		detail := lastErr
		if detail == "" {
			detail = lastLines(string(r.Stderr), 5)
		}
		return facts, fmt.Errorf("exit %d: %s", r.Code, detail)
	}
	return facts, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

// ownerFor is the owner the runhost keeps. A runhost set up before keeps its
// owner, and one owned by its devices alone is bound to the account this
// device is signed in to; one owned by another account is refused.
func ownerFor(ctx context.Context, conn *Conn, want runhost.Owner) (runhost.Owner, error) {
	b, err := conn.ReadRoot(ctx, runhost.ConfigFile)
	if err != nil || b == nil {
		return want, err
	}
	var cfg runhost.ServerConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return want, fmt.Errorf("the runhost's configuration on the server is not readable: %w", err)
	}
	if cfg.Owner.Kind == runhost.OwnerAccount {
		if want.Kind != runhost.OwnerAccount || want.Registry != cfg.Owner.Registry || want.Handle != cfg.Owner.Handle {
			return want, fmt.Errorf("this runhost belongs to %s: pair it signed in to that account", cfg.Owner)
		}
	}
	return want, nil
}

// mergeAuthorizedKeys replaces this device's line in the account's
// authorized_keys and keeps every other device's.
func mergeAuthorizedKeys(existing []byte, line, deviceID string) []byte {
	var out bytes.Buffer
	comment := runhost.DeviceComment(deviceID)
	for _, l := range strings.Split(string(existing), "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasSuffix(t, " "+comment) {
			continue
		}
		out.WriteString(t + "\n")
	}
	out.WriteString(line + "\n")
	return out.Bytes()
}

// mergeDevices puts d into the devices file, replacing its earlier entry.
func mergeDevices(existing []byte, d runhost.Device) ([]byte, error) {
	var ds runhost.Devices
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := json.Unmarshal(existing, &ds); err != nil {
			return nil, fmt.Errorf("the runhost's devices file on the server is not readable: %w", err)
		}
	}
	kept := ds.Devices[:0]
	for _, old := range ds.Devices {
		if old.ID != d.ID {
			kept = append(kept, old)
		}
	}
	ds.Devices = append(kept, d)
	return json.MarshalIndent(ds, "", "  ")
}

func envFile(vars map[string]string) []byte {
	var b bytes.Buffer
	for _, k := range sortedKeys(vars) {
		b.WriteString(k + "=" + shellQuote(vars[k]) + "\n")
	}
	return b.Bytes()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// RemoveOptions are what Remove needs.
type RemoveOptions struct {
	Record runhost.Record
	Auth   AdminAuth
	// Answer names the admin account; its host and port are the record's.
	Answer targethost.Answer
	Ask    targethost.Asker
	Say    func(string)
	// RemoveImages and RemoveDocker take away the images runs used and,
	// when setup installed it, Docker Engine.
	RemoveImages, RemoveDocker bool
}

// Remove takes the runhost away from the server. The host key must still be
// the one pinned at setup.
func Remove(ctx context.Context, o RemoveOptions) error {
	say := func(format string, args ...any) {
		if o.Say != nil {
			o.Say(fmt.Sprintf(format, args...))
		}
	}
	pinned, _, _, _, err := ssh.ParseAuthorizedKey([]byte(o.Record.HostKey))
	if err != nil {
		return fmt.Errorf("the record of %s holds no readable host key: %w", o.Record.Name, err)
	}
	addr, err := targethost.ResolveAddress(ctx, o.Record.Name, targethost.Answer{User: o.Answer.User, Host: o.Record.Host, Port: o.Record.Port}, policy.HostsPolicy{})
	if err != nil {
		return err
	}
	conn, err := Dial(ctx, addr, o.Record.Port, o.Answer.User, pinned, o.Auth)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.FindRoot(ctx); errors.Is(err, ErrSudoPassword) {
		if o.Ask == nil {
			return fmt.Errorf("%w, and no one is at a terminal to type it", err)
		}
		pw, aerr := o.Ask.AskSecret(fmt.Sprintf("sudo password for %s@%s: ", o.Answer.User, o.Record.Host))
		if aerr != nil {
			return aerr
		}
		if err := conn.SetSudo(ctx, pw); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	stage, err := conn.Stage(ctx)
	if err != nil {
		return err
	}
	defer conn.Unstage(context.WithoutCancel(ctx), stage)
	files := map[string][]byte{"remove.sh": removeScript}
	existing, err := conn.ReadRoot(ctx, "/etc/docker/daemon.json")
	if err != nil {
		return err
	}
	if out, changed, err := RemoveRuntimes(existing); err != nil {
		return err
	} else if changed {
		files["daemon.json"] = out
	}
	removeDocker := false
	if o.RemoveDocker {
		b, err := conn.ReadRoot(ctx, runhost.ConfigFile)
		if err != nil {
			return err
		}
		var cfg runhost.ServerConfig
		if b != nil && json.Unmarshal(b, &cfg) == nil && cfg.InstalledDocker {
			removeDocker = true
		} else {
			say("Docker Engine stays: setup did not install it")
		}
	}
	files["env"] = envFile(map[string]string{"STAGE": stage, "REMOVE_IMAGES": yesNo(o.RemoveImages), "REMOVE_DOCKER": yesNo(removeDocker)})
	for _, name := range sortedKeys(files) {
		if err := conn.Upload(ctx, stage, name, files[name]); err != nil {
			return err
		}
	}
	say("removing")
	if _, err := runScript(ctx, conn, "sh "+shellQuote(stage+"/remove.sh")+" "+shellQuote(stage+"/env"), say); err != nil {
		return fmt.Errorf("the removal stopped: %w", err)
	}
	return nil
}
