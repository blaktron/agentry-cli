package persistent

// An image server's set-up (its client, its sign-in) runs one-shots on the
// server's volume and records what it did on the agent's config. A stopped
// agent is set up under its owner lock. A running one is held
// (hold.go): its segment ends between turns so no container holds the
// volume, the record goes to the supervisor as a change, and the next
// segment starts with the server set up, the conversation continuing.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
	"github.com/blaktron/agentry-cli/pkg/runner/session/protocol"
)

// SignInRecord is an image server's sign-in record as a set-up leaves it,
// by the server's key. It carries no credential.
type SignInRecord struct {
	Key    string      `json:"key"`
	SignIn ImageSignIn `json:"signIn"`
}

// recordSignIn merges what a sign-in records into c's record of the image
// server rec.Key: the account it answers as, when, the switches it asked
// for, the tenant, the client it was made with and whether Agentry's own
// app made it. The person's own client id and signoff are left as they
// are, and each field is checked first, since the record reaches the
// server's environment and arguments.
func recordSignIn(a Agent, c *Config, rec SignInRecord) error {
	s, ok := imageServer(a, rec.Key)
	if !ok {
		return fmt.Errorf("the %s agent has no image server %q", a.Name, rec.Key)
	}
	in := rec.SignIn
	if in.Account != "" && !signInAccountRE.MatchString(in.Account) {
		return fmt.Errorf("a sign-in names its account as an email address, not %q", in.Account)
	}
	for _, g := range in.Granted {
		if !slices.ContainsFunc(s.Switches, func(w offer.ImageSwitch) bool { return w.Key == g && w.SignedFor }) {
			return fmt.Errorf("%s has no switch %q a sign-in asks for", s.Name, g)
		}
	}
	if in.Tenant != "" {
		if err := checkTenant(in.Tenant); err != nil {
			return err
		}
	}
	if in.SignedInWith != "" {
		if err := CheckMicrosoftClient(in.SignedInWith); err != nil {
			return fmt.Errorf("the client a sign-in was made with: %w", err)
		}
	}
	now := time.Now().UTC()
	editSignIn(c, rec.Key, func(cur *ImageSignIn) {
		if !in.Client.IsZero() {
			cur.Client = now
		}
		if in.Account != "" {
			cur.Account, cur.At, cur.Granted = in.Account, now, slices.Clone(in.Granted)
		}
		if in.Tenant != "" {
			cur.Tenant = in.Tenant
		}
		if in.SignedInWith != "" {
			cur.SignedInWith = in.SignedInWith
		}
		cur.AgentryApp = cur.AgentryApp || in.AgentryApp
	})
	return nil
}

// signInAccountRE is an account a server answers as: an email address.
var signInAccountRE = regexp.MustCompile(`(?i)^[^@\s\x00-\x1f\x7f-][^@\s\x00-\x1f\x7f]{0,63}@[a-z0-9.-]{1,253}$`)

// signInEdit changes the sign-in record of the image server key and
// switches its mail provider on, as a set-up's record is.
type signInEdit func(key string, change func(*ImageSignIn)) error

// runServerSetUp runs set, a set-up of what (a server's name), with the
// agent's config as it is and edit, which records what the set-up did:
// under the owner lock when the agent is stopped, through a hold when it
// runs. The hold ends when set returns, and the next segment starts with
// what was recorded. onProvider is told of a mail provider the record
// switched on.
func runServerSetUp(ctx context.Context, home string, a Agent, client, what string, onProvider func(offer.MailProvider), set func(c Config, edit signInEdit) error) error {
	edited := func(c Config, key string, change func(*ImageSignIn)) Change {
		in := c.SignIns[key]
		change(&in)
		ch := Change{SignIn: &SignInRecord{Key: key, SignIn: in}}
		if p, ok := mailProviderOf(a, key); ok {
			ch.MailProvider = &MailProviderSwitch{Key: p.Key, On: true}
			if !slices.Contains(c.MailProviders, p.Key) && onProvider != nil {
				onProvider(p)
			}
		}
		return ch
	}
	for tries := 0; ; tries++ {
		owner, err := Acquire(home, a.Name, client)
		var held *HeldError
		if !errors.As(err, &held) {
			if err != nil {
				return err
			}
			defer owner.Release()
			return setUpStopped(home, a, set, edited)
		}
		h, herr := HoldAgent(ctx, home, a.Name, what)
		var refused *RefusedError
		switch {
		case (errors.Is(herr, ErrNotRunning) || errors.Is(herr, ErrNoAnswer)) && tries == 0:
			// The agent stopped meanwhile, or the lock's holder is no
			// supervisor: try the lock again once, then name the holder.
			continue
		case errors.Is(herr, ErrNotRunning) || errors.Is(herr, ErrNoAnswer):
			return held
		case errors.As(herr, &refused) && refused.Class == string(protocol.RefusedMalformed):
			return fmt.Errorf("%w: its supervisor is an older Agentry, which cannot be held for a set-up; stop the agent, set it up, then start it", held)
		case herr != nil:
			return fmt.Errorf("hold the %s agent to set up %s: %w", a.Name, what, herr)
		}
		return setUpHeld(ctx, home, a, what, h, set, edited)
	}
}

// setUpStopped runs set on a stopped agent, the caller holding its lock.
func setUpStopped(home string, a Agent, set func(Config, signInEdit) error, edited func(Config, string, func(*ImageSignIn)) Change) error {
	c, err := LoadConfig(home, a.Name)
	if err != nil {
		return err
	}
	return set(c, func(key string, change func(*ImageSignIn)) error {
		now, err := LoadConfig(home, a.Name)
		if err != nil {
			return err
		}
		if _, err := applyChange(home, a, &now, edited(now, key, change), HomeMalwareCheck(home)); err != nil {
			return err
		}
		return SaveConfig(home, a.Name, now)
	})
}

// setUpHeld runs set on a running agent through its hold h, which it ends.
func setUpHeld(ctx context.Context, home string, a Agent, what string, h *AgentHold, set func(Config, signInEdit) error, edited func(Config, string, func(*ImageSignIn)) Change) error {
	c, err := LoadConfig(home, a.Name)
	if err != nil {
		_ = h.Close()
		return err
	}
	err = set(c, func(key string, change func(*ImageSignIn)) error {
		now, err := LoadConfig(home, a.Name)
		if err != nil {
			return err
		}
		_, err = h.Change(ctx, edited(now, key, change))
		return err
	})
	if rerr := h.Resume(context.WithoutCancel(ctx), err == nil); rerr != nil {
		return errors.Join(err, fmt.Errorf("the %s agent after setting up %s: %w", a.Name, what, rerr))
	}
	return err
}
