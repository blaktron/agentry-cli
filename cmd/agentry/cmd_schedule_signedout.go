// `agentry schedule --signed-out`: a schedule that also runs while its
// person is signed out (plan quickagent-scheduling.md §5a, D17–D19;
// agentry-cli#749). Linux with Docker Engine only. Linger, turned on once
// behind a confirm, keeps the person's systemd user manager running after
// sign-out; the keys a fire reads are sealed into an encrypted systemd
// credential the unit loads, because the keychain is locked then.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// scheduleGOOS is the system a schedule is made on; replaced in tests.
var scheduleGOOS = runtime.GOOS

// scheduleDockerOS is what the docker daemon says it runs on; replaced in
// tests.
var scheduleDockerOS = func(ctx context.Context) (string, error) { return dockerx.New().OperatingSystem(ctx) }

// scheduleLinger is the linger of the person running this process;
// replaced in tests.
var scheduleLinger = func() (*schedule.Linger, error) {
	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("who is running agentry: %w", err)
	}
	return schedule.NewLinger(u.Username), nil
}

// scheduleCollectKeys reads the keychain values to seal; replaced in tests,
// which have no keychain.
var scheduleCollectKeys = modelkey.Collect

// scheduleSignedIn reports whether the person running this process has a
// session (logind's user state is not "lingering"); replaced in tests. Off
// Linux, or when logind cannot be asked, it says yes: only linger rings an
// alarm with nobody signed in.
var scheduleSignedIn = func() bool {
	if runtime.GOOS != "linux" {
		return true
	}
	out, err := exec.Command("loginctl", "show-user", strconv.Itoa(os.Getuid()), "-p", "State", "--value").Output()
	return err != nil || strings.TrimSpace(string(out)) != "lingering"
}

// lingerSentence is what the person is told turning linger on does (D17).
const lingerSentence = "Agentry's schedules will start when this computer starts, even if you are not signed in"

// signedOutUnsupported is the refusal on a system where a run cannot go
// on after sign-out (D17).
func signedOutUnsupported() error {
	return engine.Classify(engine.ExitUsage, fmt.Errorf("--signed-out is not available on %s: Docker Desktop stops when you sign out", scheduleGOOS))
}

// signedOutGaps is every reason sc cannot run while its person is signed
// out (D17, D18). It is asked once fireGaps found nothing, so the release
// is installed and the model key is in the keychain.
func signedOutGaps(ctx context.Context, sc schedule.Schedule) []string {
	if scheduleGOOS != "linux" {
		return []string{signedOutUnsupported().Error()}
	}
	var gaps []string
	switch v, err := schedule.SystemdVersion(); {
	case err != nil:
		gaps = append(gaps, fmt.Sprintf("running while signed out needs systemd %d or later, and its version could not be read: %v", schedule.MinSystemd, err))
	case v < schedule.MinSystemd:
		gaps = append(gaps, fmt.Sprintf("running while signed out needs systemd %d or later, which keeps the key in an encrypted credential (this computer has systemd %d); a key is never kept in a plain file", schedule.MinSystemd, v))
	}
	switch osName, err := scheduleDockerOS(ctx); {
	case err != nil:
		gaps = append(gaps, fmt.Sprintf("running while signed out needs Docker Engine, and Docker could not be asked which engine it is: %v", err))
	case strings.Contains(osName, "Docker Desktop"):
		gaps = append(gaps, "running while signed out needs Docker Engine: Docker Desktop stops when you sign out")
	}
	home, err := runnerHome()
	if err != nil {
		return append(gaps, err.Error())
	}
	m, _, _, err := scheduledManifest(home, sc)
	if err != nil {
		return append(gaps, err.Error())
	}
	for _, s := range oauthServers(m) {
		gaps = append(gaps, fmt.Sprintf("the declared server %s signs in with an OAuth connection, which is kept in the keychain, and the keychain is locked while you are signed out", connectionServer(s)))
	}
	for _, k := range sc.Run.Knowledge {
		if !filepath.IsAbs(k) {
			gaps = append(gaps, fmt.Sprintf("the kept knowledge pack %s opens with a key kept in the keychain, which is locked while you are signed out", k))
		}
	}
	return gaps
}

// confirmLinger asks the person, once, to let their schedules start with
// the computer (D17). Linger already on asks nothing; --yes is the
// confirm given elsewhere (a client that asked it).
func confirmLinger(cmd *cobra.Command, yes bool) error {
	l, err := scheduleLinger()
	if err != nil {
		return err
	}
	on, err := l.On()
	if err != nil || on {
		return err
	}
	if yes {
		return nil
	}
	return promptYes(cmd,
		lingerSentence+": linger is turned on for "+l.User+" (your distribution may ask for an administrator's password).\nType \"yes\" to turn it on: ",
		"--signed-out turns on linger for "+l.User+": "+lingerSentence+". Confirm on a terminal, or pass --yes",
		"not scheduled: linger was not turned on")
}

// enableLinger turns linger on if it is off, and says so.
func enableLinger(cmd *cobra.Command, store schedule.Store) error {
	l, err := scheduleLinger()
	if err != nil {
		return err
	}
	now, err := store.EnableLinger(l, cmd.InOrStdin(), cmd.ErrOrStderr(), scheduleNow())
	if err != nil {
		return fmt.Errorf("turn on linger for %s: %w", l.User, err)
	}
	if now {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "linger is on for %s: %s; it is turned off again with the last schedule that runs signed out\n", l.User, lingerSentence)
	}
	return nil
}

// releaseLinger turns linger off once no schedule runs signed out and
// this CLI turned it on, and says so; a failure is said, not fatal.
func releaseLinger(in io.Reader, errOut io.Writer, store schedule.Store) {
	l, err := scheduleLinger()
	if err == nil {
		var off bool
		if off, err = store.ReleaseLinger(l, in, errOut); off && err == nil {
			_, _ = fmt.Fprintf(errOut, "linger is off for %s again: no schedule runs signed out\n", l.User)
		}
	}
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "warning: linger was not turned off: %v (`loginctl disable-linger` does it)\n", err)
	}
}

// sealScheduleKeys seals, into path, the keychain values a fire of sc
// reads: the model key, the person's own provider keys the policy takes
// from the keychain, and the registry token when the keychain holds it
// (a private release resolves through the registry at every fire).
func sealScheduleKeys(sc schedule.Schedule, path string) error {
	pf, err := policy.Resolve(sc.Run.Policy)
	if err != nil {
		return err
	}
	pol, _, err := policy.EnsureDefault(pf.Path)
	if err != nil {
		return err
	}
	required, optional := schedule.SealAccounts(pol)
	if sc.Local == "" {
		if a := keychainTokenAccount(sc.Registry); a != "" {
			optional = append(optional, a)
		}
	}
	values, err := scheduleCollectKeys(required, optional)
	if err != nil {
		return err
	}
	return schedule.SealCredential(path, values)
}

// keychainTokenAccount is the keychain account credentialFor reads reg's
// token from, or "" when it would not read the keychain (a token in the
// config file, a local sign-out).
func keychainTokenAccount(reg string) string {
	if cfg, err := loadCLIConfig(); err == nil {
		if e := cfg.findRegistry(reg); e != nil && (e.TokenRef == tokenRefNone || (e.TokenRef == tokenRefFile && e.Token != "")) {
			return ""
		}
	}
	host, err := registryHost(reg)
	if err != nil {
		return ""
	}
	return host
}

// signedOutOn makes sc, already checked by fireGaps, run while signed out:
// the refusals, the linger confirm, its keys sealed into credential (a
// pending path for a schedule not kept yet), and the probe in the user
// manager loading them. Linger itself is turned on by the caller, once the
// schedule is kept.
func signedOutOn(cmd *cobra.Command, sc schedule.Schedule, credential string, yes bool) error {
	if gaps := signedOutGaps(cmd.Context(), sc); len(gaps) > 0 {
		return engine.Classify(engine.ExitPolicy, fmt.Errorf("%s cannot run while you are signed out:\n  - %s", sc.Ref, strings.Join(gaps, "\n  - ")))
	}
	if err := confirmLinger(cmd, yes); err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	if err := sealScheduleKeys(sc, credential); err != nil {
		return engine.Classify(engine.ExitPolicy, fmt.Errorf("seal the keys of a schedule that runs while you are signed out: %w", err))
	}
	if gaps := probeGaps(sc, credential); len(gaps) > 0 {
		_ = os.Remove(credential)
		return needsErr(sc, gaps, " while you are signed out")
	}
	return nil
}

// credentialKept reports whether id's sealed keys are in the store.
func credentialKept(store schedule.Store, id string) bool {
	path, err := store.CredentialPath(id)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// dropCredential removes a schedule's sealed keys once nothing fires it
// signed out: the key outlives no schedule that needs it.
func dropCredential(store schedule.Store, id string) error {
	path, err := store.CredentialPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// resealSignedOut seals every signed-out schedule's keys again from the
// keychain, after a key or a sign-in changed (setup, login, logout). What
// fails is said, not fatal: the change it follows is already made.
func resealSignedOut(errOut io.Writer) {
	store, err := scheduleStore()
	if err != nil {
		return
	}
	list, _, err := store.List()
	if err != nil {
		return
	}
	n := 0
	for _, sc := range list {
		if !sc.SignedOut {
			continue
		}
		if st, _ := store.State(sc.ID); st.Ended {
			continue
		}
		path, err := store.CredentialPath(sc.ID)
		if err == nil {
			err = sealScheduleKeys(sc, path)
		}
		if err != nil {
			// The keys sealed before may be the ones just changed or
			// signed out of: they go, and its runs fail, closed, until
			// they are sealed again.
			_ = dropCredential(store, sc.ID)
			_, _ = fmt.Fprintf(errOut, "warning: the keys of %s (%s), which runs while you are signed out, were not sealed again: %v; its old sealed keys were removed, so it does not run until `agentry schedule edit %s --signed-out` seals them again\n", sc.Name, sc.ID, err, sc.ID)
			continue
		}
		n++
	}
	if n > 0 {
		_, _ = fmt.Fprintf(errOut, "sealed the keys of %d schedule(s) that run while you are signed out again\n", n)
	}
}
