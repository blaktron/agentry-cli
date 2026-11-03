package messenger

import (
	"context"
	"errors"
	"fmt"
	"regexp"
)

// What a client on the person's own computer does with an account before
// and after it is added: check that the service takes its credential, and
// send the paired person a test message. Both go straight to the service,
// as the person's own use of their account, never through an agent.

// telegramToken is the shape of a token BotFather gives: the bot's id, a
// colon and its secret.
var telegramToken = regexp.MustCompile(`^[0-9]{3,20}:[A-Za-z0-9_-]{30,64}$`)

// CheckCredentialShape refuses a credential that is not the kind's shape,
// before anything is sent with it.
func CheckCredentialShape(kind, token string) error {
	switch kind {
	case KindTelegram:
		if !telegramToken.MatchString(token) {
			return errors.New("that is not a Telegram bot token: BotFather gives one as digits, a colon and about 35 letters, digits, - and _")
		}
		return nil
	}
	return fmt.Errorf("%q is not a messenger this build knows", kind)
}

// Check asks the service who the account is, as the role does when it
// starts: the account's id and name there. api is the service's address,
// empty for its own.
func Check(ctx context.Context, kind, token, api string) (Ready, error) {
	if err := CheckCredentialShape(kind, token); err != nil {
		return Ready{}, err
	}
	if api == "" {
		api = TelegramAPI
	}
	tg, err := newTelegram(api, token, "")
	if err != nil {
		return Ready{}, err
	}
	me, err := tg.getMe(ctx)
	if err != nil {
		return Ready{}, err
	}
	name := me.FirstName
	if me.Username != "" {
		name = "@" + me.Username
	}
	return Ready{Account: id(me.ID), Name: name}, nil
}

// SendTest sends text to chat on the account, plain.
func SendTest(ctx context.Context, kind, token, api, chat, text string) error {
	if err := CheckCredentialShape(kind, token); err != nil {
		return err
	}
	if api == "" {
		api = TelegramAPI
	}
	tg, err := newTelegram(api, token, "")
	if err != nil {
		return err
	}
	return tg.sendMessage(ctx, chat, text, false)
}
