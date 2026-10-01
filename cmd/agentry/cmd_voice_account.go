package main

// The Writing Voice between this computer and the account (plan
// writing-voice.md V3, D6; agentry-cli#532): two places, at most one voice
// each, never synced on their own. `voice upload` submits this computer's
// voice to the account, where AI Review passes it before it replaces the
// account's live voice; `voice download` puts the account's live voice
// over this computer's; `voice status` shows both and the review.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/voice"
	"github.com/blaktron/agentry-cli/pkg/voice/remote"
)

// voiceAccount is the signed-in client of the registry the CLI speaks to,
// past its minimum-version gate.
func voiceAccount(cmd *cobra.Command) (*remote.Client, error) {
	u := registryURL()
	token, err := signedInToken(cmd, u)
	if err != nil {
		return nil, err
	}
	return remote.New(u, token, httpClient)
}

// voiceAccountErr gives an account error the exit class a script can act on.
func voiceAccountErr(err error) error {
	switch {
	case errors.Is(err, remote.ErrSignedOut):
		return engine.Classify(engine.ExitUsage, err)
	case errors.Is(err, remote.ErrNoAccountVoice), errors.Is(err, voice.ErrNoVoice):
		return engine.Classify(engine.ExitUsage, err)
	}
	return err
}

// voicePollEvery is how often `voice upload --wait` asks again; a variable
// so a test can shorten it.
var voicePollEvery = 3 * time.Second

func newVoiceUploadCmd() *cobra.Command {
	var (
		wait    bool
		timeout time.Duration
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "upload",
		Short: "Submit this computer's Writing Voice to your account, for AI Review",
		Long: "Send the Writing Voice kept on this computer to your account. AI Review reads it there; once it passes, " +
			"it replaces your account's voice for runs In Agentry. This computer's voice does not change. " +
			"`agentry voice status` shows the review, or --wait waits for it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := voiceStore(cmd)
			if err != nil {
				return err
			}
			text, kept, err := s.OpenVoice(cmd.Context())
			if err != nil {
				return voiceAccountErr(voiceErr(err))
			}
			c, err := voiceAccount(cmd)
			if err != nil {
				return voiceAccountErr(err)
			}
			acct, err := c.Upload(cmd.Context(), text)
			if err != nil {
				return voiceAccountErr(err)
			}
			digest := kept.Meta.SHA256
			out := cmd.OutOrStdout()
			if !asJSON {
				_, _ = fmt.Fprintf(out, "Submitted your Writing Voice to %s (%d bytes, %s): in review.\n", c.URL(), len(text), digest)
				_, _ = fmt.Fprintln(out, "It replaces your account's voice once AI Review passes it. This computer's voice is unchanged.")
			}
			if wait {
				acct, err = waitForReview(cmd.Context(), c, digest, timeout)
				if err != nil {
					return voiceAccountErr(err)
				}
			}
			if asJSON {
				printJSON(out, acct)
				return nil
			}
			if !wait {
				_, _ = fmt.Fprintln(out, "`agentry voice status` shows the outcome.")
				return nil
			}
			return sayReview(out, acct, digest)
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for AI Review's verdict and report it")
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Minute, "how long --wait waits")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print your account's voice, as the registry answers it, as JSON")
	return cmd
}

// waitForReview asks again until the version of digest is no longer in
// review, or timeout passes; it answers the account as it last read it.
func waitForReview(ctx context.Context, c *remote.Client, digest string, timeout time.Duration) (remote.Account, error) {
	deadline := time.Now().Add(timeout)
	for {
		acct, err := c.Get(ctx)
		if err != nil {
			return acct, err
		}
		if p := acct.Pending; p == nil || p.Digest != digest || p.Review != remote.Reviewing || time.Now().After(deadline) {
			return acct, nil
		}
		select {
		case <-ctx.Done():
			return acct, ctx.Err()
		case <-time.After(voicePollEvery):
		}
	}
}

// sayReview reports where the version of digest stands. A flag is an
// error, so a script sees it; the findings are said first.
func sayReview(w io.Writer, acct remote.Account, digest string) error {
	switch p := acct.Pending; {
	case acct.Live != nil && acct.Live.Digest == digest:
		_, _ = fmt.Fprintln(w, "AI Review passed it: it is your account's live voice.")
		if acct.AlwaysAttach {
			_, _ = fmt.Fprintln(w, "Your voice is on for every run In Agentry. You can turn it off with `agentry voice attach off --account`.")
		}
	case p != nil && p.Digest == digest && p.Review == remote.Flagged:
		_, _ = fmt.Fprintln(w, "AI Review flagged it; your account's live voice is unchanged:")
		sayFindings(w, p.Findings)
		return errors.New("AI Review flagged your Writing Voice; edit it and upload again")
	case p != nil && p.Digest == digest:
		_, _ = fmt.Fprintln(w, "Still in review. `agentry voice status` shows the outcome.")
	default:
		_, _ = fmt.Fprintln(w, "Another version has replaced it in your account since. `agentry voice status` shows where it stands.")
	}
	return nil
}

// sayFindings lists what keeps a version from going live: the findings
// that flag it, each by its AI Review question or its line.
func sayFindings(w io.Writer, fs []remote.Finding) {
	for _, f := range fs {
		if !f.Flags {
			continue
		}
		where := f.Title
		switch {
		case f.Question != "":
			where = fmt.Sprintf("AI Review's %s question, scored %d of %d", f.Question, f.Score, remote.MaxScore)
		case f.Line > 0:
			where = fmt.Sprintf("%s, line %d (%s)", f.Title, f.Line, f.Scanner)
		}
		_, _ = fmt.Fprintf(w, "  - %s\n", where)
	}
}

func newVoiceDownloadCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "download",
		Short: "Keep your account's live Writing Voice on this computer",
		Long: "Put your account's live Writing Voice over the one kept on this computer. It asks first when this " +
			"computer keeps a different voice (--yes to skip).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := voiceAccount(cmd)
			if err != nil {
				return voiceAccountErr(err)
			}
			acct, err := c.Get(cmd.Context())
			if err != nil {
				return voiceAccountErr(err)
			}
			if acct.Live == nil {
				msg := "your account has no live Writing Voice yet"
				if p := acct.Pending; p != nil {
					msg += fmt.Sprintf(" (one is %s)", p.Review.Phrase())
				}
				return engine.Classify(engine.ExitUsage, errors.New(msg))
			}
			live := acct.Live
			s, err := voiceStore(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			switch kept, err := s.Voice(); {
			case errors.Is(err, voice.ErrNoVoice):
			case err != nil:
				return voiceErr(err)
			case kept.Meta.SHA256 == live.Digest:
				_, _ = fmt.Fprintf(out, "This computer already keeps your account's live voice (%s).\n", live.Digest)
				return nil
			case !yes:
				if err := promptYes(cmd,
					fmt.Sprintf("Replace this computer's Writing Voice (%s) with your account's live one (%s)? Type \"yes\": ", kept.Meta.SHA256, live.Digest),
					"this computer keeps a different Writing Voice; re-run with --yes to replace it with your account's",
					"download stopped: this computer's Writing Voice is unchanged"); err != nil {
					return engine.Classify(engine.ExitUsage, err)
				}
			}
			v, created, err := s.KeepVoice(cmd.Context(), []byte(live.Text), voice.SourceDownload, nil, voiceNow())
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "Kept your account's live Writing Voice on this computer (%d bytes, %s).\n", v.Meta.Bytes, v.Meta.SHA256)
			if created {
				sayVoiceOn(out)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "replace a different voice kept on this computer without asking")
	return cmd
}

// voiceStatus is what `voice status` shows, and its --json.
type voiceStatus struct {
	Local    *voice.Meta     `json:"local"`
	Registry string          `json:"registry"`
	SignedIn bool            `json:"signedIn"`
	Account  *remote.Account `json:"account"`
	Error    string          `json:"error,omitempty"`
}

func newVoiceStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show this computer's Writing Voice and your account's, with its review",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st := voiceStatus{Registry: registryURL()}
			s, err := voiceStore(cmd)
			if err != nil {
				return err
			}
			switch kept, err := s.Voice(); {
			case errors.Is(err, voice.ErrNoVoice):
			case err != nil:
				return voiceErr(err)
			default:
				st.Local = &kept.Meta
			}
			if c, err := voiceAccount(cmd); err == nil {
				st.SignedIn = true
				switch acct, err := c.Get(cmd.Context()); {
				case errors.Is(err, remote.ErrNoAccountVoice):
				case err != nil:
					st.Error = err.Error()
				default:
					st.Account = &acct
				}
			} else if !errors.Is(err, remote.ErrSignedOut) {
				st.SignedIn = true
				st.Error = err.Error()
			}
			if asJSON {
				printJSON(cmd.OutOrStdout(), st)
				return nil
			}
			sayVoiceStatus(cmd.OutOrStdout(), st)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print both voices and the review as JSON")
	return cmd
}

func sayVoiceStatus(w io.Writer, st voiceStatus) {
	if st.Local == nil {
		_, _ = fmt.Fprintln(w, "This computer: no Writing Voice.")
	} else {
		attach := "on"
		if !st.Local.AlwaysAttach {
			attach = "off"
		}
		_, _ = fmt.Fprintf(w, "This computer: %s; attached to every run here: %s.\n", st.Local.Describe(), attach)
	}
	switch {
	case !st.SignedIn:
		_, _ = fmt.Fprintf(w, "Your account at %s: signed out (`agentry login --registry %s`).\n", st.Registry, st.Registry)
		return
	case st.Error != "":
		_, _ = fmt.Fprintf(w, "Your account at %s: could not be read: %s\n", st.Registry, st.Error)
		return
	case st.Account == nil:
		_, _ = fmt.Fprintf(w, "Your account at %s: no Writing Voice. `agentry voice upload` submits this computer's.\n", st.Registry)
		return
	}
	a := st.Account
	if a.Live == nil {
		_, _ = fmt.Fprintf(w, "Your account at %s: no live voice yet.\n", st.Registry)
	} else {
		attach := "on"
		if !a.AlwaysAttach {
			attach = "off"
		}
		same := ""
		if st.Local != nil && st.Local.SHA256 == a.Live.Digest {
			same = " (the same as this computer's)"
		}
		_, _ = fmt.Fprintf(w, "Your account at %s: live voice %s, %d bytes%s; attached to every run In Agentry: %s.\n",
			st.Registry, a.Live.Digest, a.Live.Bytes, same, attach)
	}
	if p := a.Pending; p != nil {
		switch p.Review {
		case remote.Flagged:
			_, _ = fmt.Fprintf(w, "Pending: %s, %d bytes, flagged by AI Review; it will not go live:\n", p.Digest, p.Bytes)
			sayFindings(w, p.Findings)
		default:
			state := p.Review.Phrase()
			if p.Verdict == remote.VerdictIncomplete {
				state = "in review (AI Review has not answered yet; it is asked again)"
			}
			_, _ = fmt.Fprintf(w, "Pending: %s, %d bytes, %s.\n", p.Digest, p.Bytes, state)
		}
	}
	if pr := a.Previous; pr != nil {
		_, _ = fmt.Fprintf(w, "Previous: %s, %d bytes.\n", pr.Digest, pr.Bytes)
	}
}
