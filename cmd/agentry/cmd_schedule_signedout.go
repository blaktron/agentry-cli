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
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/intake"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/sources"
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
	gaps := signedOutMachineGaps(ctx)
	if scheduleGOOS != "linux" {
		return gaps
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
	pf, err := policy.Resolve(sc.Run.Policy)
	if err != nil {
		return append(gaps, err.Error())
	}
	pol, _, err := policy.EnsureDefault(pf.Path)
	if err != nil {
		return append(gaps, err.Error())
	}
	return append(gaps, signedOutKeyGaps(home, pol)...)
}

// signedOutKeyGaps is why a model key pol reads from the keychain cannot
// be spent while its person is signed out, where the fire reads the copy
// sealed for it and renews nothing (modelsignin.Store.CheckSealed,
// agentry-cli#811): a connected Hugging Face token, which only a signed-in
// run renews, or a connected key already past its expiry.
func signedOutKeyGaps(home string, pol *policy.Policy) []string {
	required, optional := schedule.SealAccounts(pol)
	s := connectStore(home)
	var gaps []string
	for _, account := range append(required, optional...) {
		if p := modelkey.ProviderOf(account); p != "" {
			if err := s.CheckSealed(p); err != nil && !slices.Contains(gaps, err.Error()) {
				gaps = append(gaps, err.Error())
			}
		}
	}
	return gaps
}

// signedOutMachineGaps is every reason this computer cannot run any
// schedule while its person is signed out: the system, systemd's version
// and the Docker engine (D17, D18), whatever the QuickAgent.
func signedOutMachineGaps(ctx context.Context) []string {
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
// from the keychain, the registry token when the keychain holds it (a
// private release resolves through the registry at every fire), and the
// key saved for each input source's host when there is one (a private
// source is fetched at every fire, agentry-cli#854).
func sealScheduleKeys(sc schedule.Schedule, path string) error {
	pf, err := policy.Resolve(sc.Run.Policy)
	if err != nil {
		return err
	}
	pol, _, err := policy.EnsureDefault(pf.Path)
	if err != nil {
		return err
	}
	home, err := runnerHome()
	if err != nil {
		return err
	}
	// A key no signed-out fire could spend is not sealed: the schedule
	// then fails closed and the person is told now, not at its fire.
	if gaps := signedOutKeyGaps(home, pol); len(gaps) > 0 {
		return errors.New(strings.Join(gaps, "; "))
	}
	required, optional := schedule.SealAccounts(pol)
	if sc.Local == "" {
		if a := keychainTokenAccount(sc.Registry); a != "" {
			optional = append(optional, a)
		}
	}
	hosts, err := scheduleSourceHosts(sc)
	if err != nil {
		return err
	}
	for _, h := range hosts {
		optional = append(optional, sources.KeyAccount(h))
	}
	// A target host's sign-in: its runs have no other (agentry-cli#863).
	for _, h := range sc.Run.Hosts {
		required = append(required, h.Account)
	}
	values, err := scheduleCollectKeys(required, optional)
	if err != nil {
		return err
	}
	return schedule.SealCredential(path, values)
}

// scheduleSourceHosts is the host of each input source sc's answers
// name, read with the release's own questions.
func scheduleSourceHosts(sc schedule.Schedule) ([]string, error) {
	if len(sc.Run.Intake) == 0 {
		return nil, nil
	}
	home, err := runnerHome()
	if err != nil {
		return nil, err
	}
	m, _, _, err := scheduledManifest(home, sc)
	if err != nil {
		return nil, err
	}
	answers, err := intake.ParsePreset(sc.Run.Intake)
	if err != nil {
		return nil, err
	}
	return intake.SourceHosts(m.Intake, answers)
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

// resealFailure is one signed-out schedule whose keys were not sealed
// again, and so were removed.
type resealFailure struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Error string `json:"error"`
}

// resealReport is what `agentry schedule reseal --json` prints.
type resealReport struct {
	Sealed []string        `json:"sealed"`
	Failed []resealFailure `json:"failed"`
}

// resealSignedOut seals every signed-out schedule's keys again from the
// keychain, after a key or a sign-in changed (setup, login, logout, and a
// connect or disconnect since agentry-cli#811). What fails is said, not
// fatal: the change it follows is already made.
func resealSignedOut(errOut io.Writer) {
	rep, err := resealAll()
	if err != nil {
		return
	}
	for _, f := range rep.Failed {
		_, _ = fmt.Fprintf(errOut, "warning: the keys of %s (%s), which runs while you are signed out, were not sealed again: %s; its old sealed keys were removed, so it does not run until `agentry schedule edit %s --signed-out` seals them again\n", f.Name, f.ID, f.Error, f.ID)
	}
	if n := len(rep.Sealed); n > 0 {
		_, _ = fmt.Fprintf(errOut, "sealed the keys of %d schedule(s) that run while you are signed out again\n", n)
	}
}

// resealAll seals the keys of every signed-out schedule that has not
// ended again. One that fails loses the keys sealed before: they may be
// the ones just changed or signed out of, so its runs fail, closed, until
// they are sealed again.
func resealAll() (resealReport, error) {
	rep := resealReport{Sealed: []string{}, Failed: []resealFailure{}}
	store, err := scheduleStore()
	if err != nil {
		return rep, err
	}
	list, _, err := store.List()
	if err != nil {
		return rep, err
	}
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
			_ = dropCredential(store, sc.ID)
			rep.Failed = append(rep.Failed, resealFailure{ID: sc.ID, Name: sc.Name, Error: err.Error()})
			continue
		}
		rep.Sealed = append(rep.Sealed, sc.ID)
	}
	return rep, nil
}

func newScheduleResealCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reseal",
		Short: "Seal the keys of every schedule that runs while you are signed out again, from the keychain",
		Long: "Seal again, from the keychain, the keys of every schedule that runs while you\n" +
			"are signed out: after the model key, one of your own provider keys or a\n" +
			"registry sign-in changed outside `agentry setup`, `login` and `logout`\n" +
			"(which do it themselves), for example in Agentry Desktop's Settings.\n" +
			"A schedule whose keys cannot be sealed again loses the keys sealed\n" +
			"before, and does not run until `agentry schedule edit <id> --signed-out`.\n" +
			"Exits 1 when any failed; --json prints {sealed, failed}.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep, err := resealAll()
			if err != nil {
				return err
			}
			if flags.jsonOut {
				if err := emitJSON(cmd.OutOrStdout(), rep); err != nil {
					return err
				}
			} else {
				for _, f := range rep.Failed {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "not sealed again: %s (%s): %s; its old sealed keys were removed, so it does not run until `agentry schedule edit %s --signed-out`\n", f.Name, f.ID, f.Error, f.ID)
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d schedule(s) sealed again, %d not\n", len(rep.Sealed), len(rep.Failed))
			}
			if len(rep.Failed) > 0 {
				return engine.Classify(engine.ExitPolicy, fmt.Errorf("the keys of %d schedule(s) were not sealed again", len(rep.Failed)))
			}
			return nil
		},
	}
}

// signedOutAvailability is whether this computer can run a schedule while
// its person is signed out, and why not.
type signedOutAvailability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	// ModelKey is why a run on the default policy's model key is refused
	// while signed out, on a computer that can otherwise run one: a
	// connected Hugging Face token, which only a signed-in run renews, or
	// a connected key past its expiry (agentry-cli#811).
	ModelKey string `json:"modelKey,omitempty"`
}

func signedOutHere(ctx context.Context) signedOutAvailability {
	gaps := signedOutMachineGaps(ctx)
	if len(gaps) > 0 {
		return signedOutAvailability{Reason: strings.Join(gaps, "; ")}
	}
	a := signedOutAvailability{Available: true}
	home, err := runnerHome()
	if err != nil {
		return a
	}
	pf, err := policy.Resolve("")
	if err != nil {
		return a
	}
	// Read, never written: this check changes nothing, and with no policy
	// yet there is no key to refuse.
	if pol, _, err := policy.Load(pf.Path); err == nil {
		a.ModelKey = strings.Join(signedOutKeyGaps(home, pol), "; ")
	}
	return a
}

func newScheduleCheckSignedOutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check-signed-out",
		Short: "Say whether this computer can run schedules while you are signed out",
		Long: "Say whether this computer can run a schedule while you are signed out\n" +
			"(--signed-out): Linux with Docker Engine and systemd 256 or later, and\n" +
			"whether a run on your model setup's key is refused then (a connected\n" +
			"Hugging Face token, which only a signed-in run renews, or a connected\n" +
			"key past its expiry). It asks nothing and changes nothing; --json prints\n" +
			"{available, reason, modelKey}.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a := signedOutHere(cmd.Context())
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), a)
			}
			if a.Available && a.ModelKey != "" {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "available on this computer, but a run on your model setup is refused while you are signed out: "+a.ModelKey)
				return nil
			}
			if a.Available {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "available: schedules can run while you are signed out on this computer")
				return nil
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "not available: "+a.Reason)
			return nil
		},
	}
}
