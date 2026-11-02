package setup

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/clirelease"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
)

//go:embed assets/pair.sh
var pairScript []byte

// PairOptions are what Pair needs.
type PairOptions struct {
	// Name is the runhost's name on this device.
	Name string
	// Answer is the server's SSH address and admin account.
	Answer targethost.Answer
	// Auth is the admin sign-in.
	Auth AdminAuth
	// KnownHosts are where the server's host key is looked up.
	KnownHosts targethost.KnownHostsFiles
	// Ask asks the person at a terminal; nil is no one to ask.
	Ask targethost.Asker
	// Say narrates, one line at a time.
	Say func(string)
	// Version is this device's CLI version.
	Version string
	// Owner is the account this device is signed in to, or the paired
	// devices when it is signed out.
	Owner runhost.Owner
	// DeviceID and DeviceLabel name this device to the server.
	DeviceID, DeviceLabel string
	// Now is the clock, for the record's dates.
	Now func() time.Time
}

// PairOutcome is a finished pairing.
type PairOutcome struct {
	Record runhost.Record
	// Bound is true when the pairing bound a runhost its devices owned to
	// this device's account.
	Bound bool
}

// Pair adds this device to a runhost set up from another: with the admin
// sign-in, it installs this device's run key and sealing key and nothing
// else. The runhost must belong to the account this device is signed in to;
// one its devices own is bound to that account. The server's CLI stays as
// it is: a device newer than it updates it after, over the run key, and an
// older one is told to update itself. The record it returns is not yet
// saved: the caller saves it.
func Pair(ctx context.Context, o PairOptions) (out *PairOutcome, err error) {
	if err := runhost.ValidName(o.Name); err != nil {
		return nil, err
	}
	if !clirelease.ValidVersion(o.Version) {
		return nil, ErrNotARelease
	}
	if !runhost.ValidDeviceID(o.DeviceID) {
		return nil, fmt.Errorf("device id %q cannot name a device", o.DeviceID)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	red := newRedactor(o.Auth)
	say := red.say(o.Say)
	out = &PairOutcome{}
	defer func() { err = red.err(err) }()

	addr, err := resolveAddress(ctx, o.Name, o.Answer, policy.HostsPolicy{})
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

	oldConfig, err := conn.ReadRoot(ctx, runhost.ConfigFile)
	if err != nil {
		return out, err
	}
	if len(strings.TrimSpace(string(oldConfig))) == 0 {
		return out, fmt.Errorf("%s is not a runhost yet: set it up with agentry runhost add", o.Answer.Host)
	}
	var cfg runhost.ServerConfig
	if err := json.Unmarshal(oldConfig, &cfg); err != nil {
		return out, fmt.Errorf("the runhost's configuration on the server is not readable: %w", err)
	}
	files := map[string][]byte{"pair.sh": pairScript}
	env := map[string]string{}
	switch {
	case cfg.Owner.Kind == runhost.OwnerAccount && cfg.Owner != o.Owner:
		return out, fmt.Errorf("this runhost belongs to %s: sign in to that account on this device, then pair again", cfg.Owner)
	case cfg.Owner.Kind != runhost.OwnerAccount && o.Owner.Kind == runhost.OwnerAccount:
		cfg.Owner = o.Owner
		b, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return out, err
		}
		files["config.json"], env["WAS_CONFIG_JSON"], out.Bound = b, hashOrNone(oldConfig), true
	}

	server, err := serverCLI(ctx, conn)
	if err != nil {
		return out, err
	}
	cmp, ok := clirelease.CompareVersions(server, o.Version)
	if !ok {
		return out, fmt.Errorf("the runhost runs CLI %q, which this device cannot order against its own %s", server, o.Version)
	}
	say("the runhost runs CLI %s, owned by %s", server, cfg.Owner)

	keys, err := runhost.NewDeviceKeys()
	if err != nil {
		return out, err
	}
	runPub, err := keys.RunPublic()
	if err != nil {
		return out, err
	}
	line := runhost.AuthorizedKeyLine(runPub, o.DeviceID)
	if cmp < 0 {
		// A CLI older than this device's may not take a forced command that
		// names its device: the key goes in the form every CLI serves, and
		// the service names its device once an update has restarted it.
		line = `restrict,command="` + runhost.ServeCommand + `" ` + runPub + " " + runhost.DeviceComment(o.DeviceID)
	}
	oldKeys, err := conn.ReadRoot(ctx, runhost.AuthorizedKeys)
	if err != nil {
		return out, err
	}
	files["authorized_keys"] = mergeAuthorizedKeys(oldKeys, line, o.DeviceID)
	env["WAS_AUTHORIZED_KEYS"] = hashOrNone(oldKeys)
	oldDevices, err := conn.ReadRoot(ctx, runhost.DevicesFile)
	if err != nil {
		return out, err
	}
	now := o.Now().UTC().Format(time.RFC3339)
	devices, err := mergeDevices(oldDevices, runhost.Device{ID: o.DeviceID, Label: o.DeviceLabel, RunKey: runPub, SealKey: keys.SealPublic(), PairedAt: now})
	if err != nil {
		return out, err
	}
	files["devices.json"] = devices
	env["WAS_DEVICES_JSON"] = hashOrNone(oldDevices)

	stage, err := conn.Stage(ctx)
	if err != nil {
		return out, err
	}
	defer conn.Unstage(context.WithoutCancel(ctx), stage)
	env["STAGE"] = stage
	files["env"] = envFile(env)
	for _, name := range sortedKeys(files) {
		if err := conn.Upload(ctx, stage, name, files[name]); err != nil {
			return out, err
		}
	}
	if err := runhost.SaveKeys(o.Name, keys); err != nil {
		return out, err
	}
	paired := false
	defer func() {
		if !paired {
			_ = runhost.DeleteKeys(o.Name)
		}
	}()
	say("pairing this device")
	if _, err := runScript(ctx, conn, "sh "+shellQuote(stage+"/pair.sh")+" "+shellQuote(stage+"/env"), say); err != nil {
		return out, fmt.Errorf("the pairing stopped: %w", err)
	}
	paired = true
	conn.Unstage(ctx, stage)
	_ = conn.Close()

	out.Record = runhost.Record{
		Name:               o.Name,
		Host:               o.Answer.Host,
		Port:               o.Answer.Port,
		HostKey:            strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hk.Key))),
		HostKeyFingerprint: targethost.Fingerprint(hk.Key),
		Owner:              cfg.Owner,
		DeviceID:           o.DeviceID,
		CLIVersion:         server,
		GVisor:             cfg.GVisor,
		AddedAt:            now,
	}
	say("checking the runhost over this device's run key alone")
	rc, err := runhost.DialRunhost(ctx, out.Record, keys)
	if err != nil {
		return out, err
	}
	defer func() { _ = rc.Close() }()
	st, err := rc.Ask(ctx, "status", nil, nil)
	if err != nil {
		return out, err
	}
	if st.Code != 0 {
		return out, errors.New(nonEmpty(st.Error, "the runhost did not say its status"))
	}
	var s struct {
		Version      string          `json:"version"`
		Arch         string          `json:"arch"`
		OS           string          `json:"os"`
		GVisor       string          `json:"gvisor"`
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := json.Unmarshal(st.Result, &s); err != nil {
		return out, fmt.Errorf("the runhost's status: %w", err)
	}
	out.Record.CLIVersion, out.Record.Arch, out.Record.OS, out.Record.GVisor, out.Record.Capabilities = s.Version, s.Arch, s.OS, s.GVisor, s.Capabilities
	return out, nil
}

// serverCLI is the version of the CLI the runhost's path points at.
func serverCLI(ctx context.Context, conn *Conn) (string, error) {
	r, err := conn.RunRoot(ctx, "readlink "+shellQuote(runhost.CLIPath), nil)
	if err != nil {
		return "", err
	}
	v := path.Base(path.Dir(strings.TrimSpace(string(r.Stdout))))
	if r.Code != 0 || !clirelease.ValidVersion(v) {
		return "", errors.New("the runhost's CLI could not be found: agentry runhost add repairs it")
	}
	return v, nil
}
