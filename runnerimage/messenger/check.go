package messenger

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// What a client on the person's own computer does with an account before
// and after it is added: check that the service takes its credential, and
// send the paired person a test message. Both go straight to the service,
// as the person's own use of their account, never through an agent.

// Credential is a messenger account's credential, as the keychain keeps it,
// and the account's own server where its kind has one.
type Credential struct {
	Kind   string
	Token  string
	Server string
}

// The shapes of the credentials each service gives.
var (
	// BotFather's token: the bot's id, a colon and its secret.
	telegramToken = regexp.MustCompile(`^[0-9]{3,20}:[A-Za-z0-9_-]{30,64}$`)
	// The developer portal's bot token: three base64url parts joined by dots.
	discordToken = regexp.MustCompile(`^[A-Za-z0-9_-]{18,40}\.[A-Za-z0-9_-]{4,10}\.[A-Za-z0-9_-]{20,64}$`)
	slackToken   = regexp.MustCompile(`^xox[ab]-[A-Za-z0-9-]{20,}$|^xapp-[A-Za-z0-9-]{20,}$`)
	// An access token is the homeserver's own, of no fixed shape: printable
	// and without a space.
	matrixToken = regexp.MustCompile(`^[\x21-\x7e]{16,1000}$`)
)

// CheckCredentialShape refuses a credential that is not the kind's shape,
// before anything is sent with it.
func CheckCredentialShape(kind, token string) error {
	switch kind {
	case KindTelegram:
		if !telegramToken.MatchString(token) {
			return errors.New("that is not a Telegram bot token: BotFather gives one as digits, a colon and about 35 letters, digits, - and _")
		}
		return nil
	case KindDiscord:
		if !discordToken.MatchString(token) {
			return errors.New("that is not a Discord bot token: the developer portal's Bot page gives one as three parts of letters, digits, - and _ joined by dots")
		}
		return nil
	case KindSlack:
		app, bot, err := SlackTokens(token)
		if err != nil {
			return err
		}
		if !slackToken.MatchString(app) || !slackToken.MatchString(bot) {
			return errors.New("those are not Slack tokens: an app-level token begins xapp- and a bot's begins xoxb-")
		}
		return nil
	case KindMatrix:
		if !matrixToken.MatchString(token) {
			return errors.New("that is not a Matrix access token: one is printable text of at least 16 characters, with no space")
		}
		return nil
	}
	return fmt.Errorf("%q is not a messenger this build knows", kind)
}

// CredentialParts are the values a credential is made of, each masked on its
// own wherever it would appear: a Slack credential's two tokens, any other
// credential whole.
func CredentialParts(kind, token string) []string {
	if kind == KindSlack {
		if app, bot, err := SlackTokens(token); err == nil {
			return []string{token, app, bot}
		}
	}
	return []string{token}
}

// clientFor is c's client, reaching the service directly from this
// computer: at api when it is not "" (a test's server), else at the
// service's own address or the account's server.
func clientFor(c Credential, api string) (client, error) {
	if err := CheckCredentialShape(c.Kind, c.Token); err != nil {
		return nil, err
	}
	k, ok := Lookup(c.Kind)
	if !ok {
		return nil, fmt.Errorf("%q is not a messenger this build knows", c.Kind)
	}
	if k.NamesServer {
		if _, err := ServerHost(c.Server); err != nil {
			return nil, err
		}
		if api == "" {
			api = c.Server
		}
	}
	return newClient(k.Name, api, c.Token, "")
}

// Check asks the service who the account is, as the role does when it
// starts: the account's id and name there. api is the service's address,
// empty for its own.
func Check(ctx context.Context, c Credential, api string) (Ready, error) {
	cl, err := clientFor(c, api)
	if err != nil {
		return Ready{}, err
	}
	return cl.connect(ctx)
}

// SendTest sends text to chat on the account, plain.
func SendTest(ctx context.Context, c Credential, api, chat, text string) error {
	cl, err := clientFor(c, api)
	if err != nil {
		return err
	}
	k, _ := Lookup(c.Kind)
	if _, err := cl.connect(ctx); err != nil {
		return err
	}
	return cl.send(ctx, chat, Part{Rich: strings.TrimSpace(escapeFor(k.Format, text)), Plain: text})
}
