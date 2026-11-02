package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/authflow"
	"github.com/blaktron/agentry-cli/pkg/runhost"
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

// creditsSessionScope is the one scope a runhost's credits token carries.
const creditsSessionScope = "credits:session"

// runhostCredits is the credits token as the runhost's store keeps it.
type runhostCredits struct {
	Registry string `json:"registry"`
	TokenID  string `json:"tokenId"`
	Name     string `json:"name"`
	Token    string `json:"token"`
	Device   string `json:"device"`
	StoredAt string `json:"storedAt,omitempty"`
}

// runhostCreditsInfo is what the runhost says of its credits token: never
// the token.
type runhostCreditsInfo struct {
	Registry string `json:"registry"`
	TokenID  string `json:"tokenId"`
	Name     string `json:"name"`
	Device   string `json:"device"`
	StoredAt string `json:"storedAt"`
}

func (c runhostCredits) info() *runhostCreditsInfo {
	return &runhostCreditsInfo{Registry: c.Registry, TokenID: c.TokenID, Name: c.Name, Device: c.Device, StoredAt: c.StoredAt}
}

// runhostStoreList is the store verb's list.
type runhostStoreList struct {
	Releases int                 `json:"releases"`
	Credits  *runhostCreditsInfo `json:"credits,omitempty"`
}

// runhostCreditsRemoved is what removing the credits token did.
type runhostCreditsRemoved struct {
	Removed bool                `json:"removed"`
	Revoked bool                `json:"revoked"`
	Credits *runhostCreditsInfo `json:"credits,omitempty"`
	// RevokeError says why the registry did not revoke the token, which
	// the person then revokes there.
	RevokeError string `json:"revokeError,omitempty"`
}

var (
	tokenIDRE      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	deviceIDRE     = regexp.MustCompile(`^[0-9a-f]{16}$`)
	creditsTokenRE = regexp.MustCompile(`^agy_[A-Za-z0-9_-]{16,200}$`)
)

// validate holds a credits token sent to the runhost to what one can be.
func (c runhostCredits) validate() error {
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
	return store.Open(runhostStoreDir, runhostStoreKey)
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

// revokeCredits has the token revoke itself at its registry. A token the
// registry no longer takes (revoked or expired) is already gone.
func revokeCredits(ctx context.Context, c runhostCredits) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimRight(c.Registry, "/")+"/api/v1/user/tokens/"+c.TokenID, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("revoke the credits token at %s: %w", c.Registry, err)
	}
	_ = res.Body.Close()
	switch res.StatusCode {
	case http.StatusNoContent, http.StatusUnauthorized:
		return nil
	}
	return fmt.Errorf("revoke the credits token at %s: the registry answered %d", c.Registry, res.StatusCode)
}

// runhostStoreVerb answers the run key's store verb:
//
//	store list            what the store holds, never a credential
//	store put-credits     the credits token, a JSON document as input
//	store remove-credits  revokes the credits token and removes it
func runhostStoreVerb(ctx context.Context, req runhost.Request, in io.Reader, fw *runhost.FrameWriter) (int, error) {
	if len(req.Args) != 1 {
		return engine.ExitUsage, errors.New("store takes one of list, put-credits and remove-credits")
	}
	st, err := openRunhostStore()
	if err != nil {
		return 1, err
	}
	switch req.Args[0] {
	case "list":
		list := runhostStoreList{Releases: countStoredReleases()}
		c, ok, err := runhostStoredCredits(st)
		if err != nil {
			return 1, err
		}
		if ok {
			list.Credits = c.info()
		}
		return 0, fw.Result(list)
	case "put-credits":
		var c runhostCredits
		dec := json.NewDecoder(io.LimitReader(in, 16<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return engine.ExitUsage, fmt.Errorf("the credits token: %w", err)
		}
		if err := c.validate(); err != nil {
			return engine.ExitUsage, err
		}
		if !runhostDevicePaired(c.Device) {
			return engine.ExitUsage, fmt.Errorf("device %s is not paired with this runhost", c.Device)
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
		return 0, fw.Result(c.info())
	case "remove-credits":
		c, ok, err := runhostStoredCredits(st)
		if err != nil {
			return 1, err
		}
		out := runhostCreditsRemoved{}
		if ok {
			out.Credits = c.info()
			if err := revokeCredits(ctx, c); err != nil {
				out.RevokeError = err.Error()
			} else {
				out.Revoked = true
			}
			// The token leaves the store whether or not the registry
			// could be reached: the person revokes it there if not.
			if err := st.RemoveEntry(runhostCreditsEntry); err != nil {
				return 1, err
			}
			out.Removed = true
		}
		return 0, fw.Result(out)
	}
	return engine.ExitUsage, fmt.Errorf("store does not take %q: list, put-credits or remove-credits", req.Args[0])
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
	var provider string
	var yes bool
	cmd := &cobra.Command{
		Use:   "model <name> --provider agentry",
		Short: "Let a runhost pay for its runs' models with your Agentry credits",
		Long: "Makes a token on your account that can only pay for model use with your\n" +
			"Agentry credits, named \"Runhost <name> (credits only)\", and keeps it in the\n" +
			"runhost's store, encrypted. The registry's authorize page opens in your browser\n" +
			"to approve it; this computer's own sign-in never reaches the server. A run on\n" +
			"the runhost whose model is Agentry credits (agentry setup) then pays with it.\n" +
			"Running it again replaces the token and revokes the earlier one; `agentry\n" +
			"runhost store <name> --remove credits` and `agentry runhost remove` revoke it.\n\n" +
			"A run's own API key needs nothing here: it travels with each run.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if provider != policy.ProviderAgentry {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("--provider %q: a runhost keeps a model of its own only for Agentry credits (--provider agentry); an API key travels with each run", provider))
			}
			return runRunhostModelCredits(cmd, args[0], yes)
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "", "the model provider: agentry (Agentry credits)")
	cmd.Flags().BoolVar(&yes, "yes", false, "make and keep the token without asking")
	_ = cmd.MarkFlagRequired("provider")
	return cmd
}

func runRunhostModelCredits(cmd *cobra.Command, name string, yes bool) error {
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
		return engine.Classify(engine.ExitUsage, fmt.Errorf("no runhost named %s on this computer (`agentry runhost list`)", name))
	}
	reg := strings.TrimRight(registryURL(), "/")
	owner := runhostOwner(cmd)
	if owner.Kind != runhost.OwnerAccount {
		return engine.Classify(engine.ExitPolicy, fmt.Errorf("a runhost's credits token is made on your account: sign in to %s first (agentry login)", reg))
	}
	if rec.Owner.Kind == runhost.OwnerAccount && (rec.Owner.Handle != owner.Handle || strings.TrimRight(rec.Owner.Registry, "/") != reg) {
		return engine.Classify(engine.ExitPolicy, fmt.Errorf("runhost %s belongs to %s, and this computer is signed in as %s on %s", name, rec.Owner, owner.Handle, reg))
	}
	keys, err := runhost.LoadKeys(name)
	if err != nil {
		return err
	}
	tokenName := "Runhost " + name + " (credits only)"
	if !yes {
		ask := terminalAsker(cmd)
		if ask == nil {
			return engine.Classify(engine.ExitUsage, errors.New("no one is at a terminal to confirm: pass --yes"))
		}
		msg := fmt.Sprintf("Make a token on your account %s that can only pay for model use with your Agentry credits, named %q, and keep it on %s (%s), encrypted?", owner.Handle, tokenName, name, rec.Host)
		ok, err := ask.Confirm(msg, false)
		if err != nil || !ok {
			return errors.Join(err, errors.New("no token made"))
		}
	}
	say := func(s string) { _, _ = fmt.Fprintln(narration(cmd), s) }
	pat, err := mintRunhostCreditsToken(cmd, reg, name)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
	defer cancel()
	c, err := runhost.DialRunhost(ctx, *rec, keys)
	if err != nil {
		return errors.Join(engine.Classify(engine.ExitGateway, err), unsentCreditsToken(cmd.Context(), reg, pat))
	}
	defer func() { _ = c.Close() }()
	doc := runhostCredits{Registry: reg, TokenID: pat.ID, Name: pat.Name, Token: pat.Token, Device: rec.DeviceID}
	a, err := c.Call(ctx, "store", []string{"put-credits"}, func(w io.Writer) error {
		return json.NewEncoder(w).Encode(doc)
	}, func(f runhost.Frame) error {
		if f.Type == runhost.FrameOut {
			say(f.Text)
		}
		return nil
	})
	if err == nil && a.Code != 0 {
		err = fmt.Errorf("%s: %s", name, a.Error)
	}
	if err != nil {
		return errors.Join(engine.Classify(engine.ExitGateway, err), unsentCreditsToken(cmd.Context(), reg, pat))
	}
	rec.Credits = &runhost.CreditsRecord{Registry: reg, TokenID: pat.ID, TokenName: pat.Name, AddedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := recs.Save(home); err != nil {
		return err
	}
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), map[string]any{"runhost": name, "credits": rec.Credits})
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "runhost %s pays for its runs' models with your Agentry credits on %s (token %q)\n", name, reg, pat.Name)
	if pol, err := devicePolicy(); err == nil && pol.Model.Provider != policy.ProviderAgentry {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "  A run uses this computer's model: choose Agentry credits with agentry setup for runs on it to pay with them.")
	}
	return nil
}

// mintRunhostCreditsToken has the person approve a runhost's credits token
// in the browser, and holds what comes back to a token that carries the
// credits scope alone, with an id the runhost can revoke it by.
func mintRunhostCreditsToken(cmd *cobra.Command, reg, name string) (authflow.PAT, error) {
	flow, err := authflow.Start()
	if err != nil {
		return authflow.PAT{}, err
	}
	defer flow.Close()
	u := flow.AuthorizeRunhostURL(reg, name)
	errOut := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(errOut, "Approve runhost %s's credits token in your browser:\n  %s\n", name, u)
	if os.Getenv("AGENTRY_NO_BROWSER") == "" {
		if err := openBrowser(u); err != nil {
			_, _ = fmt.Fprintf(errOut, "(could not open a browser: %v — open the address above yourself)\n", err)
		}
	}
	_, _ = fmt.Fprintln(errOut, "Waiting for the browser (up to 5 minutes; Ctrl-C to stop)…")
	pat, err := flow.WaitPAT(cmd.Context())
	if err != nil {
		return authflow.PAT{}, err
	}
	if pat.ID == "" || len(pat.Scopes) != 1 || pat.Scopes[0] != creditsSessionScope {
		// A registry that does not know a runhost's grant mints a sign-in
		// token instead, which no runhost may hold.
		what := "a token"
		if pat.Name != "" {
			what = fmt.Sprintf("the token %q", pat.Name)
		}
		return authflow.PAT{}, engine.Classify(engine.ExitPolicy, fmt.Errorf("%s does not mint a runhost's credits-only token, and handed back %s with other scopes: revoke it among your personal access tokens there; nothing was sent to %s", reg, what, name))
	}
	return pat, nil
}

// unsentCreditsToken revokes a token that never reached the runhost, and
// says so if it cannot.
func unsentCreditsToken(ctx context.Context, reg string, pat authflow.PAT) error {
	if err := revokeCredits(ctx, runhostCredits{Registry: reg, TokenID: pat.ID, Token: pat.Token}); err != nil {
		return fmt.Errorf("the token %q did not reach the runhost and could not be revoked (%v): revoke it among your personal access tokens on %s", pat.Name, err, reg)
	}
	return nil
}

// newRunhostStoreCmd builds `agentry runhost store`.
func newRunhostStoreCmd() *cobra.Command {
	var remove string
	cmd := &cobra.Command{
		Use:   "store <name> [--remove credits]",
		Short: "List what a runhost keeps in its store, and remove its credits token",
		Long: "Lists the runhost's store over this computer's run key: the releases it has\n" +
			"run, kept so a later run need not send them again, and its Agentry credits\n" +
			"token, if it has one. --remove credits revokes the token at its registry and\n" +
			"removes it from the store.",
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
			return engine.Classify(engine.ExitUsage, fmt.Errorf("--remove %q: the store's entry to remove is credits", remove))
		},
	}
	cmd.Flags().StringVar(&remove, "remove", "", "the entry to remove: credits")
	return cmd
}

// removeRunhostCredits has the runhost revoke and remove its credits token,
// and forgets it in this computer's record.
func removeRunhostCredits(cmd *cobra.Command, name string) (runhostCreditsRemoved, error) {
	a, _, err := askRunhostArgs(cmd, name, "store", []string{"remove-credits"})
	if err != nil {
		return runhostCreditsRemoved{}, err
	}
	if a.Code != 0 {
		return runhostCreditsRemoved{}, engine.Classify(engine.ExitCLI, fmt.Errorf("%s: %s", name, a.Error))
	}
	var out runhostCreditsRemoved
	if err := json.Unmarshal(a.Result, &out); err != nil {
		return runhostCreditsRemoved{}, err
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
