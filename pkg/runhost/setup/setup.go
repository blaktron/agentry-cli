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
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
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

// Outcome is a finished setup.
type Outcome struct {
	Record       runhost.Record
	Plan         Plan
	Restrictions []runhost.Restriction
	Status       json.RawMessage
	Doctor       runhost.Answer
}

// Add sets the server up as a runhost and pairs this device with it. The
// record it returns is not yet saved: the caller saves it. It is returned
// only once the server holds this device's run key, so a record always has
// its keys in the keychain. No error and no narration carries a secret of the
// admin sign-in, a sudo password typed at the prompt included.
func Add(ctx context.Context, o Options) (out *Outcome, err error) {
	if err := runhost.ValidName(o.Name); err != nil {
		return nil, err
	}
	if !releaseVersionRE.MatchString(o.Version) {
		return nil, ErrNotARelease
	}
	if o.Keys.Empty() {
		return nil, clirelease.ErrNoKeys
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
	red := newRedactor(o.Auth)
	say := red.say(o.Say)
	out = &Outcome{}
	defer func() { err = red.err(err) }()

	addr, err := targethost.ResolveAddress(ctx, o.Name, o.Answer, policy.HostsPolicy{})
	if err != nil {
		return out, asRunhost(err)
	}
	hk, err := targethost.CheckHostKey(ctx, o.Name, o.Answer, addr, o.KnownHosts, o.Ask, func(s string) { say("%s", s) })
	if err != nil {
		return out, asRunhost(err)
	}
	say("host key: %s (%s)", targethost.Fingerprint(hk.Key), hk.From)

	conn, err := Dial(ctx, addr, o.Answer.Port, o.Answer.User, hk.Key, o.Auth)
	if err != nil {
		return out, err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.EnsureRoot(ctx, o.Ask, o.Answer.String(), red); err != nil {
		return out, err
	}
	stage, err := conn.Stage(ctx)
	if err != nil {
		return out, err
	}
	defer conn.Unstage(context.WithoutCancel(ctx), stage)

	say("checking the server (nothing changes yet)")
	facts, err := runPreflight(ctx, conn, stage, o.Reach)
	if err != nil {
		return out, err
	}
	out.Plan = Evaluate(facts, o.Reach)
	plan := out.Plan
	for _, f := range plan.Findings {
		say("  %-5s %-9s %s", f.Level, f.Name, f.Detail)
	}
	if !plan.Supported {
		return out, fmt.Errorf("%s cannot be a runhost: %s", o.Answer.Host, stops(plan))
	}
	if o.Confirm != nil {
		ok, err := o.Confirm(plan)
		if err != nil {
			return out, err
		}
		if !ok {
			return out, errors.New("setup stopped before changing anything on the server")
		}
	}

	oldConfig, err := conn.ReadRoot(ctx, runhost.ConfigFile)
	if err != nil {
		return out, err
	}
	now := o.Now().UTC().Format(time.RFC3339)
	cfg, err := mergeConfig(oldConfig, runhost.ServerConfig{Name: o.Name, Owner: o.Owner, InstalledDocker: plan.InstallDocker, GVisor: GVisorRelease, SetUpAt: now})
	if err != nil {
		return out, err
	}

	say("downloading this CLI's release for %s and gVisor %s, and checking both", plan.Arch, GVisorRelease)
	cli, err := o.FetchCLI(ctx, o.Version, plan.Arch)
	if err != nil {
		return out, err
	}
	gv, err := o.FetchGVisor(ctx, facts["arch"])
	if err != nil {
		return out, err
	}
	keys, err := runhost.NewDeviceKeys()
	if err != nil {
		return out, err
	}
	runPub, err := keys.RunPublic()
	if err != nil {
		return out, err
	}

	files := map[string][]byte{
		"install.sh":              installScript,
		"agentry":                 cli.Bytes,
		"gvisor.tar.gz":           gv,
		"docker.asc":              dockerKey,
		"agentry-runhost.service": []byte(unitFile),
	}
	cfgJSON, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return out, err
	}
	files["config.json"] = cfgJSON
	// Each file the install overwrites is staged with the hash of the one
	// it was merged from: a file changed on the server meanwhile (another
	// device pairing, the person editing the daemon's configuration) stops
	// the install instead of being lost.
	env := map[string]string{
		"STAGE":           stage,
		"AGENTRY_VERSION": o.Version,
		"AGENTRY_SHA256":  cli.SHA256,
		"GVISOR_RELEASE":  GVisorRelease,
		"GVISOR_SHA256":   sha256Hex(gv),
		"INSTALL_DOCKER":  yesNo(plan.InstallDocker),
		"OS_ID":           facts["os_id"],
		"OS_CODENAME":     facts["os_codename"],
	}
	existing, err := conn.ReadRoot(ctx, daemonJSON)
	if err != nil {
		return out, err
	}
	if merged, changed, err := MergeRuntimes(existing); err != nil {
		return out, err
	} else if changed {
		files["daemon.json"] = merged
		env["WAS_DAEMON_JSON"] = hashOrNone(existing)
	}
	oldKeys, err := conn.ReadRoot(ctx, runhost.AuthorizedKeys)
	if err != nil {
		return out, err
	}
	files["authorized_keys"] = mergeAuthorizedKeys(oldKeys, runhost.AuthorizedKeyLine(runPub, o.DeviceID), o.DeviceID)
	env["WAS_AUTHORIZED_KEYS"] = hashOrNone(oldKeys)
	oldDevices, err := conn.ReadRoot(ctx, runhost.DevicesFile)
	if err != nil {
		return out, err
	}
	devices, err := mergeDevices(oldDevices, runhost.Device{ID: o.DeviceID, Label: o.DeviceLabel, RunKey: runPub, SealKey: keys.SealPublic(), PairedAt: now})
	if err != nil {
		return out, err
	}
	files["devices.json"] = devices
	env["WAS_DEVICES_JSON"] = hashOrNone(oldDevices)
	files["env"] = envFile(env)
	for _, name := range sortedKeys(files) {
		if err := conn.Upload(ctx, stage, name, files[name]); err != nil {
			return out, err
		}
	}

	if err := runhost.SaveKeys(o.Name, keys); err != nil {
		return out, err
	}
	installed := false
	defer func() {
		if !installed {
			_ = runhost.DeleteKeys(o.Name)
		}
	}()

	say("installing")
	after, err := runScript(ctx, conn, "sh "+shellQuote(stage+"/install.sh")+" "+shellQuote(stage+"/env"), say)
	if err != nil {
		return out, fmt.Errorf("the install stopped: %w", err)
	}
	// The server now holds this device's run key: the record is kept from
	// here, so a failed check below can still be looked at and removed.
	installed = true
	out.Record = runhost.Record{
		Name:               o.Name,
		Host:               o.Answer.Host,
		Port:               o.Answer.Port,
		HostKey:            strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hk.Key))),
		HostKeyFingerprint: targethost.Fingerprint(hk.Key),
		Owner:              cfg.Owner,
		DeviceID:           o.DeviceID,
		CLIVersion:         o.Version,
		Arch:               plan.Arch,
		OS:                 facts["os_pretty"],
		GVisor:             GVisorRelease,
		AddedAt:            now,
	}
	if added := NewListeners(facts.Listening(), strings.Fields(after["listening_after"])); len(added) > 0 {
		return out, fmt.Errorf("the install opened a network listener it should not have: %s", strings.Join(added, " "))
	}
	conn.Unstage(ctx, stage)
	_ = conn.Close()

	say("checking the runhost over this device's run key alone")
	rc, err := runhost.DialRunhost(ctx, out.Record, keys)
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
	var caps struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if json.Unmarshal(st.Result, &caps) == nil {
		out.Record.Capabilities = caps.Capabilities
	}
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

// mergeConfig is the runhost's configuration after this setup. A runhost set
// up before keeps its name, its setup date and whether setup installed
// Docker, and takes the gVisor release installed now. One owned by its
// devices alone is bound to the account this device is signed in to; one
// owned by another account is refused.
func mergeConfig(existing []byte, want runhost.ServerConfig) (runhost.ServerConfig, error) {
	if len(bytes.TrimSpace(existing)) == 0 {
		return want, nil
	}
	var cfg runhost.ServerConfig
	if err := json.Unmarshal(existing, &cfg); err != nil {
		return want, fmt.Errorf("the runhost's configuration on the server is not readable: %w", err)
	}
	if cfg.Owner.Kind == runhost.OwnerAccount && cfg.Owner != want.Owner {
		return want, fmt.Errorf("this runhost belongs to %s: pair it signed in to that account", cfg.Owner)
	}
	if want.Owner.Kind == runhost.OwnerAccount {
		cfg.Owner = want.Owner
	}
	cfg.InstalledDocker = cfg.InstalledDocker || want.InstalledDocker
	cfg.GVisor = want.GVisor
	if cfg.Name == "" {
		cfg.Name = want.Name
	}
	if cfg.SetUpAt == "" {
		cfg.SetUpAt = want.SetUpAt
	}
	return cfg, nil
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

// asRunhost names the server as the runhost it is being set up as in a
// refusal of the target hosts' address and host-key checks, which setup
// shares: the person named no target host.
func asRunhost(err error) error {
	var r *targethost.Refusal
	if errors.As(err, &r) {
		r.Of = "runhost"
	}
	return err
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
func Remove(ctx context.Context, o RemoveOptions) (err error) {
	red := newRedactor(o.Auth)
	say := red.say(o.Say)
	defer func() { err = red.err(err) }()
	pinned, _, _, _, err := ssh.ParseAuthorizedKey([]byte(o.Record.HostKey))
	if err != nil {
		return fmt.Errorf("the record of %s holds no readable host key: %w", o.Record.Name, err)
	}
	answer := targethost.Answer{User: o.Answer.User, Host: o.Record.Host, Port: o.Record.Port}
	addr, err := targethost.ResolveAddress(ctx, o.Record.Name, answer, policy.HostsPolicy{})
	if err != nil {
		return asRunhost(err)
	}
	conn, err := Dial(ctx, addr, o.Record.Port, o.Answer.User, pinned, o.Auth)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.EnsureRoot(ctx, o.Ask, answer.String(), red); err != nil {
		return err
	}
	stage, err := conn.Stage(ctx)
	if err != nil {
		return err
	}
	defer conn.Unstage(context.WithoutCancel(ctx), stage)
	files := map[string][]byte{"remove.sh": removeScript}
	env := map[string]string{"STAGE": stage, "REMOVE_IMAGES": yesNo(o.RemoveImages), "REMOVE_DOCKER": yesNo(o.RemoveDocker)}
	existing, err := conn.ReadRoot(ctx, daemonJSON)
	if err != nil {
		return err
	}
	if out, changed, err := RemoveRuntimes(existing); err != nil {
		return err
	} else if changed {
		files["daemon.json"] = out
		env["WAS_DAEMON_JSON"] = hashOrNone(existing)
	}
	files["env"] = envFile(env)
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

// daemonJSON is the Docker daemon's configuration file.
const daemonJSON = "/etc/docker/daemon.json"

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// hashOrNone is a file's SHA-256 as the install compares it, "none" for a
// file that was not there.
func hashOrNone(b []byte) string {
	if b == nil {
		return "none"
	}
	return sha256Hex(b)
}

// redactor masks the admin sign-in's secrets, and a sudo password typed later,
// in everything setup says or returns.
type redactor struct{ secrets []string }

func newRedactor(a AdminAuth) *redactor { return &redactor{secrets: a.Values()} }

func (r *redactor) add(v string) {
	if v != "" {
		r.secrets = append(r.secrets, v)
	}
}

func (r *redactor) mask(s string) string {
	for _, v := range r.secrets {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	return s
}

func (r *redactor) say(to func(string)) func(string, ...any) {
	return func(format string, args ...any) {
		if to != nil {
			to(r.mask(fmt.Sprintf(format, args...)))
		}
	}
}

func (r *redactor) err(err error) error {
	if err == nil {
		return nil
	}
	if m := r.mask(err.Error()); m != err.Error() {
		return errors.New(m)
	}
	return err
}
