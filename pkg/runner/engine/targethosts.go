package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
)

// Target hosts (agentry-notes plans/runtime-hosts.md §4–§8;
// agentry-cli#861). The caller readies each target host before the run
// (targethost.Prepare: its address, its key, its sign-in) and hands it in
// with the intake answers. The engine refuses a hosted run that would reach
// one (D11), writes each host's folder for the host link alone, admits its
// relay to the one address and port, masks its sign-in wherever the run
// writes (D19), says why when the host link would not start, and keeps each
// host's call log as a receipt.

// HostedTargetHostRefusal is a hosted run's refusal of a release that
// declares target hosts (plan D11): exit 11, host-refused, hosted. The CLI
// refuses with it before anything is installed, and the engine again.
func HostedTargetHostRefusal(ids []string) error {
	return Classify(ExitPolicy, receipt.WithReason(
		errors.New("a hosted run never reaches a target host: this QuickAgent works a machine over SSH, which only a run on your own computer does"),
		receipt.ReasonHostRefused, receipt.HostHosted, ids...))
}

// checkTargetHosts refuses, before anything is made, a run whose plan
// declares target hosts the run cannot reach as declared: a hosted run
// (plan D11), and a target host that was not readied (every declared one
// must be: the host link holds one relay a host).
func checkTargetHosts(p Params) error {
	ids := p.Plan.Capabilities.TargetHosts
	if len(ids) == 0 {
		if p.Intake != nil && len(p.Intake.Hosts) > 0 {
			return Classify(ExitCLI, errors.New("the run was given a target host, and this QuickAgent declares none"))
		}
		return nil
	}
	if p.InputReview != nil {
		return HostedTargetHostRefusal(ids)
	}
	if p.Plan.HostLink == nil {
		return Classify(ExitCompile, errors.New("the plan declares target hosts but no host link; recompile it"))
	}
	for _, id := range ids {
		var h *targethost.Host
		if p.Intake != nil {
			h = p.Intake.Hosts[id]
		}
		switch {
		case h == nil:
			return Classify(ExitPolicy, receipt.WithReason(
				fmt.Errorf("target host %s was not given: answer it (--intake %s=[user@]host[:port]); a declared target host is needed for the run to start", id, id),
				receipt.ReasonHostRefused, receipt.HostCredential, id))
		case !h.Address.IsValid() || h.HostKey.Key == nil || h.Credential.Auth == "":
			return Classify(ExitPolicy, receipt.WithReason(
				fmt.Errorf("target host %s was not readied: its address, host key and sign-in are checked before the run starts", id),
				receipt.ReasonHostRefused, receipt.HostCredential, id))
		}
		if err := h.Target().Check(); err != nil {
			return Classify(ExitPolicy, receipt.WithReason(err, receipt.ReasonHostRefused, receipt.HostAddress, id))
		}
		// The caller resolved and checked the address; the engine holds
		// it to the same rule, whoever the caller is (plan D8).
		if err := targethost.CheckAddress(h.Address); err != nil {
			return Classify(ExitPolicy, receipt.WithReason(fmt.Errorf("target host %s: %s is %w", id, h.Address, err), receipt.ReasonHostRefused, receipt.HostAddress, id))
		}
		if p.Policy != nil {
			if ok, why := p.Policy.Hosts.Admits(h.Address); !ok {
				return Classify(ExitPolicy, receipt.WithReason(fmt.Errorf("target host %s: %s", id, why), receipt.ReasonHostRefused, receipt.HostAddress, id))
			}
		}
	}
	return nil
}

// targetHostsOf are the plan's target hosts, readied, in the plan's order.
func targetHostsOf(p Params) []*targethost.Host {
	var out []*targethost.Host
	for _, id := range p.Plan.Capabilities.TargetHosts {
		if p.Intake != nil && p.Intake.Hosts[id] != nil {
			out = append(out, p.Intake.Hosts[id])
		}
	}
	return out
}

// hostLinkUser is the uid:gid the host link runs as: the invoking user's,
// as the files image runs, or "" where there is none (Windows), which
// leaves the image's own (10001).
var hostLinkUser = func() (string, int, int) {
	uid, gid := os.Getuid(), os.Getgid()
	if uid < 0 || gid < 0 {
		return "", -1, -1
	}
	return fmt.Sprintf("%d:%d", uid, gid), uid, gid
}

// hostLinkPasswd is the passwd file bound over the host link's /etc/passwd:
// the image's own lines, and one for the uid it runs as (ssh refuses a uid
// it has no entry for, DR-91).
func hostLinkPasswd(uid, gid int) []byte {
	lines := []string{
		"root:x:0:0:root:/root:/sbin/nologin",
		"hostlink:x:10001:10001::/home/hostlink:/sbin/nologin",
	}
	if uid > 0 && uid != 10001 {
		lines = append(lines, fmt.Sprintf("agentry:x:%d:%d::/tmp:/sbin/nologin", uid, gid))
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func (rd *runDirs) hostsDir() string     { return filepath.Join(rd.cfg, spec.HostsDir) }
func (rd *runDirs) hostLogDir() string   { return filepath.Join(rd.cfg, spec.HostLogDir) }
func (rd *runDirs) hostFilesDir() string { return filepath.Join(rd.cfg, spec.HostFilesDir) }

// hostRelays are the relay targets the run's docker filter admits for its
// target hosts: each one's address and port, and nothing else.
func (rd *runDirs) hostRelays() []string {
	var out []string
	for _, h := range rd.targets {
		out = append(out, h.RelayHost())
	}
	return out
}

// ensureHostLinkImage builds the host link's image when it is missing; a
// variable so a test can stand in for the build.
var ensureHostLinkImage = targethost.EnsureImage

// prepareTargetHosts readies the host link: its image, and each host's
// folder (its target, its key or password, a passphrase and a sudo
// password when there are, its one host key), 0600 in a 0700 folder bound
// read-only into the host link alone, beside the log and copy folders. The
// receipt names each host as the run reaches it.
func prepareTargetHosts(ctx context.Context, d dockerx.Client, p Params, rd *runDirs, td *teardown, env *receipt.Envelope) error {
	if len(rd.targets) == 0 {
		return nil
	}
	want := targethost.HostLinkImage(p.Plan.Proxy.Image, p.Plan.HostLink.Base)
	if want.Tag != p.Plan.HostLink.Image {
		return Classify(ExitCompile, fmt.Errorf("the plan's host link image %s is not the one its base and runner make (%s); recompile it", p.Plan.HostLink.Image, want.Tag))
	}
	img, err := ensureHostLinkImage(ctx, d, want, filepath.Join(rd.root, "image"), p.Stderr)
	if err != nil {
		return Classify(ExitGateway, err)
	}
	env.CapabilitiesEnforced["hostLink.image"] = fmt.Sprintf("%s, built on this machine from %s with this CLI's runner and minimized to ssh and sftp (%s)",
		img.Tag, img.Base, strings.Join(img.Packages, ", "))
	dirs := []string{rd.hostsDir(), rd.hostLogDir(), rd.hostFilesDir()}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Classify(ExitCLI, fmt.Errorf("the host link's folders: %w", err))
		}
	}
	// Whatever happens next, the sign-ins die with the run (KeepRunDir
	// keeps none), and the call logs are kept as receipts.
	td.add(func(context.Context) { copyHostLogs(rd, env) })
	_, uid, gid := hostLinkUser()
	if err := os.WriteFile(filepath.Join(rd.hostsDir(), hostlink.PasswdFile), hostLinkPasswd(uid, gid), 0o644); err != nil { //nolint:gosec // no secret: user names and ids
		return Classify(ExitCLI, fmt.Errorf("the host link's passwd file: %w", err))
	}
	for _, h := range rd.targets {
		if err := writeHostFolder(filepath.Join(rd.hostsDir(), h.ID), h); err != nil {
			return Classify(ExitCLI, fmt.Errorf("target host %s: %w", h.ID, err))
		}
		env.CapabilitiesEnforced["host."+h.ID] = h.ReceiptLine(0)
	}
	for _, dir := range dirs {
		relabelShared(ctx, p.Stderr, dir)
	}
	return nil
}

// writeHostFolder writes one target host's folder, each file 0600.
func writeHostFolder(dir string, h *targethost.Host) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	t, err := json.Marshal(h.Target())
	if err != nil {
		return err
	}
	files := map[string][]byte{
		hostlink.TargetFile:     t,
		hostlink.KnownHostsFile: targethost.HostLinkKnownHosts(h.HostKey.Key),
	}
	c := h.Credential
	switch c.Auth {
	case hostlink.AuthKey:
		files[hostlink.KeyFile] = c.Key
		if c.Passphrase != "" {
			files[hostlink.PassphraseFile] = []byte(c.Passphrase)
		}
	case hostlink.AuthPassword:
		files[hostlink.PasswordFile] = []byte(c.Password)
	}
	if c.Sudo != "" {
		files[hostlink.SudoFile] = []byte(c.Sudo)
	}
	for name, b := range files {
		if err := writeFileAtomic(filepath.Join(dir, name), b, 0o600, nil); err != nil {
			return err
		}
	}
	return nil
}

// hostCredentialValues are the run's target-host sign-ins in every spelling
// the host link's scrub masks (hostlink.Spellings), for the run's redactor
// (D19).
func hostCredentialValues(p Params) []string {
	var out []string
	for _, h := range targetHostsOf(p) {
		for _, v := range h.Credential.Values() {
			out = append(out, hostlink.Spellings(v)...)
		}
	}
	return out
}

// copyHostLogs keeps each target host's call log as receipts/host-<id>.log,
// masked by the run's redactor, and counts its calls into the host's line.
func copyHostLogs(rd *runDirs, env *receipt.Envelope) {
	for _, h := range rd.targets {
		b, err := os.ReadFile(filepath.Join(rd.hostLogDir(), hostlink.LogFile(h.ID))) //nolint:gosec // the run's own folder
		if err != nil {
			continue
		}
		masked := rd.redact.String(string(b))
		_ = os.WriteFile(filepath.Join(rd.receiptsDir, hostlink.LogFile(h.ID)), []byte(masked), 0o644) //nolint:gosec // a receipt
		// The host link masks its own log; a kept run folder keeps it
		// masked by the run's redactor too.
		rd.redact.File(filepath.Join(rd.hostLogDir(), hostlink.LogFile(h.ID)))
		calls := 0
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			if len(bytes.TrimSpace(sc.Bytes())) > 0 {
				calls++
			}
		}
		env.CapabilitiesEnforced["host."+h.ID] = h.ReceiptLine(calls)
	}
}

// hostRefusalRE is the host link's start refusal as the gateway relays its
// stderr (hostlink.RefusedLine; "- <server>: <line>").
var hostRefusalRE = regexp.MustCompile(`^- ` + regexp.QuoteMeta(hostlink.ServerName) + `: target host ([a-z0-9_]{1,64}) refused \(([a-z-]+)\): (.*)$`)

// checkHostLinkStarted reads the gateway's startup log for the host link's
// own refusal, and for the gateway's line that it did not start: a target
// host that refused the sign-in or offered another key stops the run
// before the agent starts (plan §8).
func checkHostLinkStarted(logs string) error {
	lines := strings.Split(logs, "\n")
	for _, line := range lines {
		m := hostRefusalRE.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		id, detail, why := m[1], m[2], m[3]
		switch detail {
		case hostlink.RefusedAuth:
			return Classify(ExitPolicy, receipt.WithReason(fmt.Errorf("target host %s: %s", id, why), receipt.ReasonHostRefused, receipt.HostAuth, id))
		case hostlink.RefusedHostKey:
			return Classify(ExitPolicy, receipt.WithReason(fmt.Errorf("target host %s: %s", id, why), receipt.ReasonHostRefused, receipt.HostKeyChanged, id))
		default:
			return Classify(ExitGateway, receipt.WithReason(fmt.Errorf("target host %s: %s", id, why), receipt.ReasonHostRefused, receipt.HostUnreachable, id))
		}
	}
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "> Can't start "+hostlink.ServerName+":") || strings.HasPrefix(t, "> Can't list tools "+hostlink.ServerName+":") {
			return Classify(ExitGateway, fmt.Errorf("the host link did not start, so the agent would have none of the host tools; the gateway said: %s%s", t, serverOutputTail(lines, hostlink.ServerName)))
		}
	}
	return nil
}
