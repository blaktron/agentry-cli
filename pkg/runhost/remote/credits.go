package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/authflow"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/signin"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// A remote runhost pays for its runs' models with Agentry credits through a
// token of its own: minted on the owner's account through the registry's
// authorize page, able to open a credits session and nothing else, and kept
// in the runhost's store. The device that mints it never sends its own
// sign-in to the server.

// CreditsScope is the one scope a runhost's credits token carries.
const CreditsScope = "credits:session"

// Credits is the credits token as the runhost's store keeps it.
type Credits struct {
	Registry string `json:"registry"`
	TokenID  string `json:"tokenId"`
	Name     string `json:"name"`
	Token    string `json:"token"`
	Device   string `json:"device"`
	StoredAt string `json:"storedAt,omitempty"`
}

// CreditsInfo is what the runhost says of its credits token: never the
// token.
type CreditsInfo struct {
	Registry string `json:"registry"`
	TokenID  string `json:"tokenId"`
	Name     string `json:"name"`
	Device   string `json:"device"`
	StoredAt string `json:"storedAt"`
}

// Info is c without its token.
func (c Credits) Info() *CreditsInfo {
	return &CreditsInfo{Registry: c.Registry, TokenID: c.TokenID, Name: c.Name, Device: c.Device, StoredAt: c.StoredAt}
}

// StoreList is the store verb's list: never a credential.
type StoreList struct {
	Releases int          `json:"releases"`
	Credits  *CreditsInfo `json:"credits,omitempty"`
	// SignIns are the model sign-ins the runhost keeps.
	SignIns []signin.Kept `json:"signIns,omitempty"`
}

// CreditsRemoved is what removing the credits token did.
type CreditsRemoved struct {
	Removed bool         `json:"removed"`
	Revoked bool         `json:"revoked"`
	Credits *CreditsInfo `json:"credits,omitempty"`
	// RevokeError says why the registry did not revoke the token, which
	// the person then revokes there.
	RevokeError string `json:"revokeError,omitempty"`
}

// Unpaired is the unpair verb's answer: the device that left, and the
// credits token it had minted, revoked and removed with it.
type Unpaired struct {
	Device  string         `json:"device"`
	Credits CreditsRemoved `json:"credits"`
}

// MintCredits has the person approve a runhost's credits token in their
// browser, through the registry's authorize page for runhost name, and holds
// what comes back to a token that carries the credits scope alone, with an
// id the runhost can revoke it by. open is handed the page to open; wait
// says the device waits for the browser.
func MintCredits(ctx context.Context, reg, name string, open func(url string), wait func()) (authflow.PAT, error) {
	flow, err := authflow.Start()
	if err != nil {
		return authflow.PAT{}, err
	}
	defer flow.Close()
	if open != nil {
		open(flow.AuthorizeRunhostURL(reg, name))
	}
	if wait != nil {
		wait()
	}
	pat, err := flow.WaitPAT(ctx)
	if err != nil {
		return authflow.PAT{}, err
	}
	if pat.ID == "" || len(pat.Scopes) != 1 || pat.Scopes[0] != CreditsScope {
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

// RevokeCredits has the token revoke itself at its registry. A token the
// registry no longer takes (revoked or expired) is already gone.
func RevokeCredits(ctx context.Context, hc *http.Client, c Credits) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimRight(c.Registry, "/")+"/api/v1/user/tokens/"+c.TokenID, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := hc.Do(req)
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

// RevokeUnsent revokes a token that never reached the runhost, and says so
// if it cannot.
func RevokeUnsent(ctx context.Context, hc *http.Client, reg string, pat authflow.PAT) error {
	if err := RevokeCredits(ctx, hc, Credits{Registry: reg, TokenID: pat.ID, Token: pat.Token}); err != nil {
		return fmt.Errorf("the token %q did not reach the runhost and could not be revoked (%v): revoke it among your personal access tokens on %s", pat.Name, err, reg)
	}
	return nil
}

// PutCredits sends the credits token to the runhost's store over the run
// key; say takes the runhost's narration.
func PutCredits(ctx context.Context, c *runhost.Client, doc Credits, say func(string)) error {
	a, err := c.Call(ctx, "store", []string{"put-credits"}, func(w io.Writer) error {
		return json.NewEncoder(w).Encode(doc)
	}, func(f runhost.Frame) error {
		if f.Type == runhost.FrameOut && say != nil {
			say(f.Text)
		}
		return nil
	})
	if err == nil && a.Code != 0 {
		err = errors.New(nonEmptyOr(a.Error, "the runhost did not keep the credits token"))
	}
	return err
}

// ListStore asks the runhost what its store keeps.
func ListStore(ctx context.Context, c *runhost.Client) (StoreList, error) {
	var list StoreList
	err := askStore(ctx, c, []string{"list"}, nil, &list)
	return list, err
}

// RemoveCredits has the runhost revoke its credits token and remove it from
// its store.
func RemoveCredits(ctx context.Context, c *runhost.Client) (CreditsRemoved, error) {
	var out CreditsRemoved
	err := askStore(ctx, c, []string{"remove-credits"}, nil, &out)
	return out, err
}

// RemoveSignIn has the runhost end its sign-in to vendor and remove it;
// say takes the runhost's narration.
func RemoveSignIn(ctx context.Context, c *runhost.Client, vendor string, say func(string)) (signin.Removed, error) {
	var out signin.Removed
	err := askStore(ctx, c, []string{"remove-signin", vendor}, say, &out)
	return out, err
}

func askStore(ctx context.Context, c *runhost.Client, args []string, say func(string), into any) error {
	a, err := c.Ask(ctx, "store", args, say)
	if err != nil {
		return err
	}
	if a.Code != 0 {
		return errors.New(nonEmptyOr(a.Error, "the runhost's store did not answer"))
	}
	return json.Unmarshal(a.Result, into)
}

// Ended is what EndAll ended on a runhost before its removal.
type Ended struct {
	Credits CreditsRemoved
	SignIns []signin.Removed
	// Failed names what could not be ended, by "credits" or the vendor.
	Failed map[string]error
}

// EndAll ends what the runhost holds of the person's before it is removed,
// over the run key: its credits token revokes itself, and each model sign-in
// ends at its vendor (Claude Code's own logout and its volume deleted, which
// the Docker daemon would otherwise keep after the account goes).
func EndAll(ctx context.Context, c *runhost.Client, say func(string)) Ended {
	out := Ended{Failed: map[string]error{}}
	if r, err := RemoveCredits(ctx, c); err != nil {
		out.Failed["credits"] = err
	} else {
		out.Credits = r
	}
	for _, vendor := range signin.Vendors {
		r, err := RemoveSignIn(ctx, c, vendor, say)
		if err != nil {
			out.Failed[vendor] = err
			continue
		}
		if r.Removed {
			out.SignIns = append(out.SignIns, r)
		}
	}
	return out
}
