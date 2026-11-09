package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/authflow"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/remote"
	"github.com/blaktron/agentry-cli/pkg/runhost/signin"
	"github.com/blaktron/agentry-cli/pkg/runhost/store"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// A remote runhost pays for its runs' models with Agentry credits through a
// token of its own: minted on the owner's account through the registry's
// authorize page, able to open a credits session and nothing else, and kept
// in the runhost's store. The device that mints it never sends its own
// sign-in to the server.

// runhostCreditsEntry is the store entry the credits token is kept in.
const runhostCreditsEntry = "credits"

// runhostCredits is the credits token as the runhost's store keeps it.
type runhostCredits = remote.Credits

// runhostCreditsInfo is what the runhost says of its credits token: never
// the token.
type runhostCreditsInfo = remote.CreditsInfo

// runhostStoreList is the store verb's list.
type runhostStoreList = remote.StoreList

// runhostCreditsRemoved is what removing the credits token did.
type runhostCreditsRemoved = remote.CreditsRemoved

var (
	tokenIDRE      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	deviceIDRE     = regexp.MustCompile(`^[0-9a-f]{16}$`)
	creditsTokenRE = regexp.MustCompile(`^agy_[A-Za-z0-9_-]{16,200}$`)
)

// validate holds a credits token sent to the runhost to what one can be.
func validateCredits(c runhostCredits) error {
	u, err := url.Parse(c.Registry)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return fmt.Errorf("the credits token's registry %q is not a registry's address", c.Registry)
	}
	if u.Scheme != "https" && (u.Scheme != "http" || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1")) {
		return fmt.Errorf("the credits token's registry %q is not https", c.Registry)
	}
	if !tokenIDRE.MatchString(c.TokenID) {
		return errors.New("the credits token's id is not a token id")
	}
	if !creditsTokenRE.MatchString(c.Token) {
		return errors.New("the credits token is not a personal access token")
	}
	if c.Name == "" || len(c.Name) > 120 || strings.ContainsAny(c.Name, "\r\n") {
		return errors.New("the credits token's name is not one line of at most 120 characters")
	}
	if !deviceIDRE.MatchString(c.Device) {
		return errors.New("the credits token names no paired device")
	}
	return nil
}

func openRunhostStore() (*store.Store, error) {
	key, _, err := runhostStoreKeyBytes()
	if err != nil {
		return nil, err
	}
	return store.OpenWithKey(runhostStoreDir, key)
}

// runhostStoredCredits is the credits token in the runhost's store; ok is
// false when there is none.
func runhostStoredCredits(st *store.Store) (runhostCredits, bool, error) {
	b, err := st.Entry(runhostCreditsEntry)
	if errors.Is(err, store.ErrNoEntry) {
		return runhostCredits{}, false, nil
	}
	if err != nil {
		return runhostCredits{}, false, err
	}
	var c runhostCredits
	if err := json.Unmarshal(b, &c); err != nil {
		return runhostCredits{}, false, store.ErrDamaged
	}
	return c, true, nil
}

// revokeCredits has the token revoke itself at its registry.
func revokeCredits(ctx context.Context, c runhostCredits) error {
	return remote.RevokeCredits(ctx, httpClient, c)
}

// runhostStoreVerb answers the run key's store verb:
//
//	store list            what the store holds, never a credential
//	store put-credits     the credits token, a JSON document as input
//	store remove-credits  revokes the credits token and removes it
//	store remove-signin <vendor>  ends a model sign-in and removes it
//	store put-model       the runhost's own model and its API key, a JSON document as input
//	store models, put-saved-model, remove-saved-model, default-model
//	                      its saved models (runhostModelsVerb)
func runhostStoreVerb(ctx context.Context, req runhost.Request, in io.Reader, fw *runhost.FrameWriter) (int, error) {
	if len(req.Args) == 1 {
		if code, err, ok := runhostModelsVerb(ctx, req.Args[0], in, fw); ok {
			return code, err
		}
	}
	if len(req.Args) == 2 && req.Args[0] == "remove-signin" {
		return runhostSignInRemove(ctx, req.Args[1], fw)
	}
	if len(req.Args) == 1 && req.Args[0] == "put-model" {
		return runhostPutModel(in, fw)
	}
	if len(req.Args) == 2 && req.Args[0] == "remove-schedule" {
		info, err := runhostScheduleRemove(ctx, req.Args[1])
		if err != nil {
			return engine.ExitUsage, err
		}
		return 0, fw.Result(info)
	}
	if len(req.Args) != 1 {
		return engine.ExitUsage, errors.New("store takes one of list, put-credits, remove-credits, remove-signin <vendor> and remove-schedule <id>")
	}
	st, err := openRunhostStore()
	if err != nil {
		return 1, err
	}
	switch req.Args[0] {
	case "list":
		list := runhostStoreList{Releases: countStoredReleases(), SignIns: runhostSignInList()}
		if scheds, err := runhostScheduleList(); err == nil {
			list.Schedules = scheds
		}
		_, list.Sealed, _ = runhostStoreKeyBytes()
		c, ok, err := runhostStoredCredits(st)
		if err != nil {
			return 1, err
		}
		if ok {
			list.Credits = c.Info()
		}
		return 0, fw.Result(list)
	case "put-credits":
		var c runhostCredits
		dec := json.NewDecoder(io.LimitReader(in, 16<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return engine.ExitUsage, fmt.Errorf("the credits token: %w", err)
		}
		if err := validateCredits(c); err != nil {
			return engine.ExitUsage, err
		}
		if !runhostDevicePaired(c.Device) {
			return engine.ExitUsage, fmt.Errorf("device %s is not paired with this runhost", c.Device)
		}
		if req.Device != "" && c.Device != req.Device {
			return engine.ExitUsage, fmt.Errorf("the token names device %s, but device %s's run key sent it", c.Device, req.Device)
		}
		old, had, err := runhostStoredCredits(st)
		if err != nil && !errors.Is(err, store.ErrDamaged) {
			return 1, err
		}
		c.StoredAt = time.Now().UTC().Format(time.RFC3339)
		b, err := json.Marshal(c)
		if err != nil {
			return 1, err
		}
		if err := st.PutEntry(runhostCreditsEntry, b); err != nil {
			return 1, err
		}
		_ = fw.Out("the credits token %q is kept in this runhost's store, encrypted", c.Name)
		if had && old.TokenID != c.TokenID {
			if err := revokeCredits(ctx, old); err != nil {
				_ = fw.Out("warning: the earlier credits token %q was not revoked (%v): revoke it among your personal access tokens on %s", old.Name, err, old.Registry)
			} else {
				_ = fw.Out("the earlier credits token %q is revoked", old.Name)
			}
		}
		return 0, fw.Result(c.Info())
	case "remove-credits":
		out, err := revokeStoredCredits(ctx, st, "")
		if err != nil {
			return 1, err
		}
		return 0, fw.Result(out)
	}
	return engine.ExitUsage, fmt.Errorf("store does not take %q: list, put-credits, remove-credits, remove-signin <vendor> or remove-schedule <id>", req.Args[0])
}

// revokeStoredCredits revokes the store's credits token at its registry
// and removes it from the store. With device set, only a token that device
// minted is touched. The token leaves the store whether or not the registry
// could be reached: the person revokes it there if not, as the answer says.
func revokeStoredCredits(ctx context.Context, st *store.Store, device string) (runhostCreditsRemoved, error) {
	out := runhostCreditsRemoved{}
	c, ok, err := runhostStoredCredits(st)
	if err != nil || !ok || (device != "" && c.Device != device) {
		return out, err
	}
	out.Credits = c.Info()
	if err := revokeCredits(ctx, c); err != nil {
		out.RevokeError = err.Error()
	} else {
		out.Revoked = true
	}
	if err := st.RemoveEntry(runhostCreditsEntry); err != nil {
		return out, err
	}
	out.Removed = true
	return out, nil
}

// countStoredReleases is how many releases the store holds.
func countStoredReleases() int {
	ents, err := os.ReadDir(runhostStoreDir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".release") {
			n++
		}
	}
	return n
}

// runhostDevicePaired says whether id is one of the runhost's paired
// devices.
func runhostDevicePaired(id string) bool {
	var devs runhost.Devices
	if err := readJSONFile(runhostDevicesFile, &devs); err != nil {
		return false
	}
	return slices.ContainsFunc(devs.Devices, func(d runhost.Device) bool { return d.ID == id })
}

// runhostJobCredits is the token a job paid with Agentry credits opens its
// session with: the runhost's own, for the registry the job's model names.
func runhostJobCredits(model policy.Model) (string, error) {
	reg, err := policy.AgentryRegistry(model)
	if err != nil {
		return "", err
	}
	st, err := openRunhostStore()
	if err != nil {
		return "", err
	}
	c, ok, err := runhostStoredCredits(st)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("this runhost holds no Agentry credits token: agentry runhost model %s --provider agentry gives it one", runhostOwnName())
	}
	if strings.TrimRight(c.Registry, "/") != strings.TrimRight(reg, "/") {
		return "", fmt.Errorf("this runhost's credits token is for %s, not %s, which this run's model names", c.Registry, reg)
	}
	return c.Token, nil
}

// runhostOwnName is the runhost's name as its configuration has it.
func runhostOwnName() string {
	var cfg runhost.ServerConfig
	if err := readJSONFile(runhost.ConfigFile, &cfg); err != nil || cfg.Name == "" {
		return "<name>"
	}
	return cfg.Name
}

// newRunhostModelCmd builds `agentry runhost model`.
func newRunhostModelCmd() *cobra.Command {
	var provider, signIn, model, keyEnv, add, remove, def, moveTo string
	var yes, oauthApp, keepCredential bool
	cmd := &cobra.Command{
		Use:   "model <name> [--add <saved model> | --remove <saved model> [--move-to <saved model>] | --default <saved model>]",
		Short: "List the saved models a runhost holds, and make one of this computer's available there or take one away",
		Long: "A runhost holds some of this computer's saved models (agentry models list), up to\n" +
			"10. Its persistent agents and its schedules each run on one of them, or on its\n" +
			"default, the first one made available. With no flag, this lists them.\n\n" +
			"--add <saved model> makes one available there. A key is read here and kept in\n" +
			"the runhost's store, encrypted, after you agree. A plan's sign-in (Claude,\n" +
			"ChatGPT, Copilot) is made on the runhost itself from your browser here, unless it\n" +
			"keeps one already, and nothing of it stays on this computer. Agentry credits are\n" +
			"paid with a token of the runhost's own, made on your account. An endpoint of your\n" +
			"own that takes no key is given its address alone. The runhost then checks the\n" +
			"model from where it is, as a save is checked here, and refuses it, keeping\n" +
			"nothing, when it cannot reach it. Adding it again replaces the runhost's copy.\n\n" +
			"--remove <saved model> takes one away, with its credential when no other saved\n" +
			"model there uses it (--keep-credential keeps it). It is refused while one of the\n" +
			"runhost's agents or schedules names it, unless --move-to names the saved model\n" +
			"they move to; removing the default needs --move-to too. --default <saved model>\n" +
			"makes one the default.\n\n" +
			"A one-off run with --on still carries the key of its own saved model with it,\n" +
			"and nothing of any other.\n\n" +
			"--provider agentry gives the runhost a credits token alone, --sign-in\n" +
			"claude|chatgpt|copilot signs it in alone, and --provider <api provider> sets the\n" +
			"one model a runhost that holds no saved models runs on.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			given := 0
			for _, v := range []string{provider, signIn, add, remove, def} {
				if v != "" {
					given++
				}
			}
			switch {
			case given > 1:
				return engine.Classify(engine.ExitUsage, errors.New("give one of --add, --remove, --default, --provider and --sign-in"))
			case moveTo != "" && remove == "":
				return engine.Classify(engine.ExitUsage, errors.New("--move-to goes with --remove"))
			case keepCredential && remove == "":
				return engine.Classify(engine.ExitUsage, errors.New("--keep-credential goes with --remove"))
			case add != "":
				return runRunhostModelAdd(cmd, args[0], add, oauthApp, yes)
			case remove != "":
				return runRunhostModelRemove(cmd, args[0], remove, moveTo, keepCredential)
			case def != "":
				return runRunhostModelDefault(cmd, args[0], def)
			case signIn != "":
				return runRunhostModelSignIn(cmd, args[0], signIn, oauthApp, yes)
			case provider == policy.ProviderAgentry:
				return runRunhostModelCredits(cmd, args[0], yes)
			case provider != "":
				return runRunhostModelKey(cmd, args[0], provider, model, keyEnv, yes)
			}
			return runRunhostModelsList(cmd, args[0])
		},
	}
	cmd.Flags().StringVar(&add, "add", "", "make this computer's saved model available on the runhost, by name or id (agentry models list)")
	cmd.Flags().StringVar(&remove, "remove", "", "take one of the runhost's saved models away, by name or id")
	cmd.Flags().StringVar(&moveTo, "move-to", "", "with --remove: the saved model the runhost's agents and schedules naming it move to, and the new default when it is the default")
	cmd.Flags().BoolVar(&keepCredential, "keep-credential", false, "with --remove: leave its key, sign-in or credits token on the runhost")
	cmd.Flags().StringVar(&def, "default", "", "make one of the runhost's saved models its default")
	cmd.Flags().StringVar(&provider, "provider", "", "agentry (a credits token alone), or an API provider whose key the runhost keeps for a runhost that holds no saved models")
	cmd.Flags().StringVar(&model, "model", "", "with --provider <api provider>: the model the runhost's persistent agents run on (default: the provider's)")
	cmd.Flags().StringVar(&keyEnv, "api-key-env", "", "with --provider <api provider>: the variable holding its key here (default: the provider's, then this computer's keychain)")
	cmd.Flags().StringVar(&signIn, "sign-in", "", "sign the runhost in to a plan alone: claude, chatgpt or copilot")
	cmd.Flags().BoolVar(&oauthApp, "oauth-app", false, "for a Copilot sign-in: sign in through Agentry's OAuth App instead of its GitHub App")
	cmd.Flags().BoolVar(&yes, "yes", false, "agree without asking: keep the key, make the token, sign in, and send a Claude subscription's check its one message")
	return cmd
}

func runRunhostModelCredits(cmd *cobra.Command, name string, yes bool) error {
	cr, err := giveRunhostCredits(cmd, name, yes)
	if err != nil {
		return err
	}
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), map[string]any{"runhost": name, "credits": cr})
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "runhost %s pays for its runs' models with your Agentry credits on %s (token %q)\n", name, cr.Registry, cr.TokenName)
	if pol, err := devicePolicy(); err == nil && pol.Model.Provider != policy.ProviderAgentry {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "  A run uses this computer's model: choose Agentry credits with agentry setup for runs on it to pay with them.")
	}
	return nil
}

// giveRunhostCredits mints the runhost name a credits token on the signed-in
// account, after the person agrees (or yes), sends it to the runhost's
// store and records it.
func giveRunhostCredits(cmd *cobra.Command, name string, yes bool) (*runhost.CreditsRecord, error) {
	home, err := runnerHome()
	if err != nil {
		return nil, err
	}
	recs, err := runhost.LoadRecords(home)
	if err != nil {
		return nil, err
	}
	rec, ok := recs.Find(name)
	if !ok {
		return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("no runhost named %s on this computer (`agentry runhost list`)", name))
	}
	reg := strings.TrimRight(registryURL(), "/")
	owner := runhostOwner(cmd)
	if owner.Kind != runhost.OwnerAccount {
		return nil, engine.Classify(engine.ExitPolicy, fmt.Errorf("a runhost's credits token is made on your account: sign in to %s first (agentry login)", reg))
	}
	if rec.Owner.Kind == runhost.OwnerAccount && (rec.Owner.Handle != owner.Handle || strings.TrimRight(rec.Owner.Registry, "/") != reg) {
		return nil, engine.Classify(engine.ExitPolicy, fmt.Errorf("runhost %s belongs to %s, and this computer is signed in as %s on %s", name, rec.Owner, owner.Handle, reg))
	}
	keys, err := runhost.LoadKeys(name)
	if err != nil {
		return nil, err
	}
	tokenName := "Runhost " + name + " (credits only)"
	if !yes {
		ask := terminalAsker(cmd)
		if ask == nil {
			return nil, engine.Classify(engine.ExitUsage, errors.New("no one is at a terminal to confirm: pass --yes"))
		}
		msg := fmt.Sprintf("Make a token on your account %s that can only pay for model use with your Agentry credits, named %q, and keep it on %s (%s), encrypted?", owner.Handle, tokenName, name, rec.Host)
		ok, err := ask.Confirm(msg, false)
		if err != nil || !ok {
			return nil, errors.Join(err, errors.New("no token made"))
		}
	}
	say := func(s string) { _, _ = fmt.Fprintln(narration(cmd), s) }
	pat, err := mintRunhostCreditsToken(cmd, reg, name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
	defer cancel()
	c, err := runhost.DialRunhost(ctx, *rec, keys)
	if err != nil {
		return nil, errors.Join(engine.Classify(engine.ExitGateway, err), unsentCreditsToken(cmd.Context(), reg, pat))
	}
	defer func() { _ = c.Close() }()
	doc := runhostCredits{Registry: reg, TokenID: pat.ID, Name: pat.Name, Token: pat.Token, Device: rec.DeviceID}
	if err := remote.PutCredits(ctx, c, doc, say); err != nil {
		return nil, errors.Join(engine.Classify(engine.ExitGateway, fmt.Errorf("%s: %w", name, err)), unsentCreditsToken(cmd.Context(), reg, pat))
	}
	rec.Credits = &runhost.CreditsRecord{Registry: reg, TokenID: pat.ID, TokenName: pat.Name, AddedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := recs.Save(home); err != nil {
		return nil, err
	}
	return rec.Credits, nil
}

// mintRunhostCreditsToken has the person approve a runhost's credits token
// in the browser (remote.MintCredits), saying where.
func mintRunhostCreditsToken(cmd *cobra.Command, reg, name string) (authflow.PAT, error) {
	errOut := cmd.ErrOrStderr()
	return remote.MintCredits(cmd.Context(), reg, name, func(u string) {
		_, _ = fmt.Fprintf(errOut, "Approve runhost %s's credits token in your browser:\n  %s\n", name, u)
		if os.Getenv("AGENTRY_NO_BROWSER") == "" {
			if err := openBrowser(u); err != nil {
				_, _ = fmt.Fprintf(errOut, "(could not open a browser: %v — open the address above yourself)\n", err)
			}
		}
	}, func() { _, _ = fmt.Fprintln(errOut, "Waiting for the browser (up to 5 minutes; Ctrl-C to stop)…") })
}

// unsentCreditsToken revokes a token that never reached the runhost.
func unsentCreditsToken(ctx context.Context, reg string, pat authflow.PAT) error {
	return remote.RevokeUnsent(ctx, httpClient, reg, pat)
}

// newRunhostStoreCmd builds `agentry runhost store`.
func newRunhostStoreCmd() *cobra.Command {
	var remove string
	cmd := &cobra.Command{
		Use:   "store <name> [--remove credits|claude|chatgpt|copilot|schedule:<id>]",
		Short: "List what a runhost keeps in its store, and remove its credits token, a sign-in or a schedule",
		Long: "Lists the runhost's store over this computer's run key: the releases it has\n" +
			"run, kept so a later run need not send them again, its Agentry credits token,\n" +
			"if it has one, and its model sign-ins. --remove credits revokes the token at its\n" +
			"registry and removes it from the store; --remove claude runs Claude Code's own\n" +
			"logout on the runhost and deletes its sign-in volume there; --remove chatgpt and\n" +
			"--remove copilot revoke the sign-in at the vendor where it can and remove it;\n" +
			"--remove schedule:<id> takes a schedule off the runhost with everything it\n" +
			"kept there (agentry schedule add --on makes one).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			switch remove {
			case "":
				a, _, err := askRunhostArgs(cmd, name, "store", []string{"list"})
				if err != nil {
					return err
				}
				if a.Code != 0 {
					return engine.Classify(engine.ExitCLI, fmt.Errorf("%s: %s", name, a.Error))
				}
				if flags.jsonOut {
					_, err := fmt.Fprintln(cmd.OutOrStdout(), string(a.Result))
					return err
				}
				var list runhostStoreList
				if err := json.Unmarshal(a.Result, &list); err != nil {
					return err
				}
				w := cmd.OutOrStdout()
				_, _ = fmt.Fprintf(w, "releases:  %d kept\n", list.Releases)
				if list.Credits != nil {
					_, _ = fmt.Fprintf(w, "credits:   %q on %s, kept since %s\n", list.Credits.Name, list.Credits.Registry, list.Credits.StoredAt)
				} else {
					_, _ = fmt.Fprintln(w, "credits:   none")
				}
				if len(list.SignIns) == 0 {
					_, _ = fmt.Fprintln(w, "sign-ins:  none")
				}
				defer func() {
					if len(list.Schedules) == 0 {
						_, _ = fmt.Fprintln(w, "schedules: none")
					}
					for _, sc := range list.Schedules {
						_, _ = fmt.Fprintf(w, "schedule:  %s %q (%s), from device %s, keeps %s\n", sc.ID, sc.Name, sc.Ref, sc.Device, scheduleEntriesSaid(sc.Entries))
					}
					if list.Sealed {
						_, _ = fmt.Fprintln(w, "the store's key is sealed by systemd to this runhost and its account")
					} else {
						_, _ = fmt.Fprintln(w, "the store's key is a file of the runhost's account (its systemd seals none): it takes no schedule")
					}
				}()
				for _, k := range list.SignIns {
					line := fmt.Sprintf("sign-in:   %s as %s, kept since %s", k.Vendor, nonEmptyOr(k.Label, "an account the vendor did not name"), k.KeptAt)
					if k.Broken != "" {
						line += " (no longer works: " + k.Broken + ")"
					}
					_, _ = fmt.Fprintln(w, line)
				}
				return nil
			case runhostCreditsEntry:
				removed, err := removeRunhostCredits(cmd, name)
				if err != nil {
					return err
				}
				if flags.jsonOut {
					return emitJSON(cmd.OutOrStdout(), removed)
				}
				printCreditsRemoved(cmd.OutOrStdout(), name, removed)
				return nil
			}
			if id, ok := strings.CutPrefix(remove, "schedule:"); ok {
				a, _, err := askRunhostArgs(cmd, name, "store", []string{"remove-schedule", id})
				if err != nil {
					return err
				}
				if a.Code != 0 {
					return engine.Classify(engine.ExitUsage, fmt.Errorf("%s: %s", name, a.Error))
				}
				if flags.jsonOut {
					_, err := fmt.Fprintln(cmd.OutOrStdout(), string(a.Result))
					return err
				}
				var info runhost.ScheduleInfo
				_ = json.Unmarshal(a.Result, &info)
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "schedule %s %q is off runhost %s, with what it kept: %s\n", id, info.Name, name, scheduleEntriesSaid(info.Entries))
				return nil
			}
			if signin.IsVendor(remove) {
				removed, err := removeRunhostSignIn(cmd, name, remove)
				if err != nil {
					return err
				}
				if flags.jsonOut {
					return emitJSON(cmd.OutOrStdout(), removed)
				}
				printSignInRemoved(cmd.OutOrStdout(), name, removed)
				return nil
			}
			return engine.Classify(engine.ExitUsage, fmt.Errorf("--remove %q: the store's entry to remove is credits, claude, chatgpt or copilot", remove))
		},
	}
	cmd.Flags().StringVar(&remove, "remove", "", "the entry to remove: credits, claude, chatgpt, copilot or schedule:<id>")
	return cmd
}

// removeRunhostCredits has the runhost revoke and remove its credits token,
// and forgets it in this computer's record.
func removeRunhostCredits(cmd *cobra.Command, name string) (runhostCreditsRemoved, error) {
	_, _, c, err := dialRunhostNamed(cmd.Context(), name)
	if err != nil {
		return runhostCreditsRemoved{}, err
	}
	out, err := remote.RemoveCredits(cmd.Context(), c)
	_ = c.Close()
	if err != nil {
		return runhostCreditsRemoved{}, engine.Classify(engine.ExitCLI, fmt.Errorf("%s: %w", name, err))
	}
	if home, err := runnerHome(); err == nil {
		if recs, err := runhost.LoadRecords(home); err == nil {
			if rec, ok := recs.Find(name); ok && rec.Credits != nil {
				rec.Credits = nil
				_ = recs.Save(home)
			}
		}
	}
	return out, nil
}

func printCreditsRemoved(w io.Writer, name string, r runhostCreditsRemoved) {
	switch {
	case r.Credits == nil:
		_, _ = fmt.Fprintf(w, "runhost %s holds no credits token\n", name)
	case r.Revoked:
		_, _ = fmt.Fprintf(w, "runhost %s's credits token %q is revoked and removed\n", name, r.Credits.Name)
	default:
		_, _ = fmt.Fprintf(w, "runhost %s's credits token %q is removed from its store, but %s did not revoke it (%s): revoke it among your personal access tokens there\n", name, r.Credits.Name, r.Credits.Registry, r.RevokeError)
	}
}

// devicePolicy is this computer's policy, as a run without --policy reads it.
func devicePolicy() (*policy.Policy, error) {
	f, err := policy.Resolve("")
	if err != nil {
		return nil, err
	}
	pol, _, err := policy.Load(f.Path)
	return pol, err
}
