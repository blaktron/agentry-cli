package messenger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Telegram's Bot API, the four methods the role uses: getMe to learn the
// bot, getUpdates as a long poll for the messages sent to it, sendMessage
// and sendChatAction to answer. The bot's token is part of every request's
// path, so no error this file returns repeats a request's address.

// TelegramAPI is the Bot API's address.
const TelegramAPI = "https://api.telegram.org"

// telegramPoll is how long one getUpdates waits for a message; the
// request's own deadline is longer by telegramSlack, so a connection that
// went quiet is given up on and opened again. A variable so a test need not
// wait.
var (
	telegramPoll  = 50 * time.Second
	telegramSlack = 15 * time.Second
)

// telegramCallTimeout bounds every request but the long poll.
const telegramCallTimeout = 30 * time.Second

// maxTelegramAnswer bounds what the role reads of one answer.
const maxTelegramAnswer = 8 << 20

// telegram is one bot's client.
type telegram struct {
	base   string
	token  string
	poll   time.Duration
	client *http.Client
}

// newTelegram is a client for the bot whose token is token, reaching base
// through proxy (an http:// CONNECT proxy, or "" for none).
func newTelegram(base, token, proxy string) (*telegram, error) {
	poll := telegramPoll
	tr := &http.Transport{
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: poll + telegramSlack,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          4,
	}
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil || u.Scheme != "http" || u.Host == "" {
			return nil, fmt.Errorf("the relay's address %q is not an http:// proxy", proxy)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &telegram{base: strings.TrimRight(base, "/"), token: token, poll: poll, client: &http.Client{Transport: tr}}, nil
}

// TelegramError is the Bot API's refusal of a request.
type TelegramError struct {
	Method      string
	Code        int
	Description string
	RetryAfter  int
}

func (e *TelegramError) Error() string {
	return fmt.Sprintf("Telegram refused %s (%d): %s", e.Method, e.Code, e.Description)
}

// TokenRefused begins the error of a credential the service does not know,
// in the role's state and in Serve's error alike.
const TokenRefused = "the service refused the account's credential"

// errTokenRefused says Telegram does not know the bot's token.
var errTokenRefused = errors.New(TokenRefused + ": Telegram does not know the bot's token, which was revoked or is not a bot token from BotFather")

type telegramAnswer struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// call posts params to method and decodes its result into out.
func (t *telegram) call(ctx context.Context, method string, params, out any, timeout time.Duration) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+"/bot"+t.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: the request could not be made", method)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %s", method, t.scrub(transportError(err)))
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxTelegramAnswer))
	if err != nil {
		return fmt.Errorf("%s: reading the answer: %s", method, t.scrub(err.Error()))
	}
	var a telegramAnswer
	if err := json.Unmarshal(b, &a); err != nil {
		return fmt.Errorf("%s: Telegram answered HTTP %d with something that is not the Bot API's", method, resp.StatusCode)
	}
	if !a.OK {
		if a.ErrorCode == http.StatusUnauthorized || a.ErrorCode == http.StatusNotFound {
			return errTokenRefused
		}
		return &TelegramError{Method: method, Code: a.ErrorCode, Description: t.scrub(a.Description), RetryAfter: a.Parameters.RetryAfter}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(a.Result, out); err != nil {
		return fmt.Errorf("%s: Telegram's result could not be read", method)
	}
	return nil
}

// transportError is a request's error without the request's address.
func transportError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err.Error()
}

// scrub takes the token out of text.
func (t *telegram) scrub(text string) string {
	if t.token == "" {
		return text
	}
	return strings.ReplaceAll(text, t.token, "[the bot's token]")
}

type telegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type telegramMessage struct {
	MessageID int64         `json:"message_id"`
	From      *telegramUser `json:"from"`
	Chat      telegramChat  `json:"chat"`
	Date      int64         `json:"date"`
	Text      string        `json:"text"`
}

type telegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *telegramMessage `json:"message"`
}

// getMe is the bot itself.
func (t *telegram) getMe(ctx context.Context) (telegramUser, error) {
	var u telegramUser
	err := t.call(ctx, "getMe", struct{}{}, &u, telegramCallTimeout)
	return u, err
}

// getUpdates waits up to telegramPoll for the messages after offset.
func (t *telegram) getUpdates(ctx context.Context, offset int64) ([]telegramUpdate, error) {
	var us []telegramUpdate
	params := map[string]any{"timeout": int(t.poll / time.Second), "allowed_updates": []string{"message"}}
	if offset != 0 {
		params["offset"] = offset
	}
	err := t.call(ctx, "getUpdates", params, &us, t.poll+telegramSlack)
	return us, err
}

// sendMessage sends text to chat, as Telegram's HTML when isHTML.
func (t *telegram) sendMessage(ctx context.Context, chat, text string, isHTML bool) error {
	params := map[string]any{"chat_id": chat, "text": text, "link_preview_options": map[string]bool{"is_disabled": true}}
	if isHTML {
		params["parse_mode"] = "HTML"
	}
	return t.call(ctx, "sendMessage", params, nil, telegramCallTimeout)
}

// sendTyping shows chat that the bot is typing, for five seconds or until
// it sends.
func (t *telegram) sendTyping(ctx context.Context, chat string) error {
	return t.call(ctx, "sendChatAction", map[string]string{"chat_id": chat, "action": "typing"}, nil, telegramCallTimeout)
}

// unparsable says Telegram could not read a message's HTML.
func unparsable(err error) bool {
	var te *TelegramError
	return errors.As(err, &te) && te.Code == http.StatusBadRequest && strings.Contains(strings.ToLower(te.Description), "parse")
}

// retryAfter is how long Telegram asked the role to wait, or 0.
func retryAfter(err error) time.Duration {
	var te *TelegramError
	if errors.As(err, &te) && te.RetryAfter > 0 {
		return time.Duration(te.RetryAfter) * time.Second
	}
	return 0
}

func id(n int64) string { return strconv.FormatInt(n, 10) }
