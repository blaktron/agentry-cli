package messenger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Slack, through an app the person makes in their workspace: Socket Mode
// for the messages sent to it, which opens a WebSocket with the app-level
// token (xapp-), and the Web API with the bot's token (xoxb-) to learn the
// bot and answer. The two travel as one credential, the app-level token
// first and the bot's after a space. Slack gives an app no way to show it
// is typing outside its assistant threads, so the role shows none.

// SlackAPI is the Web API's address.
const SlackAPI = "https://slack.com/api"

// slackPingEvery is how often the role pings Socket Mode's WebSocket; a
// connection that answers nothing for slackQuiet is opened again.
const (
	slackPingEvery = 30 * time.Second
	slackQuiet     = 2 * time.Minute
)

// slack is one app's client.
type slack struct {
	api       string
	app, bot  string
	proxy     string
	client    *http.Client
	self      string
	selfBot   string
	anyWSHost bool
}

// SlackTokens are the two tokens of a Slack credential.
func SlackTokens(credential string) (app, bot string, err error) {
	for _, f := range strings.Fields(credential) {
		switch {
		case strings.HasPrefix(f, "xapp-") && app == "":
			app = f
		case strings.HasPrefix(f, "xoxb-") && bot == "":
			bot = f
		default:
			return "", "", errors.New("a Slack credential is the app-level token (xapp-…) and the bot's token (xoxb-…), and nothing else")
		}
	}
	if app == "" || bot == "" {
		return "", "", errors.New("a Slack credential is the app-level token (xapp-…) and the bot's token (xoxb-…): both are needed")
	}
	return app, bot, nil
}

// newSlack is a client for the app whose credential is credential. base is
// "" for Slack's own address, or a test server's, which serves the Web API
// under /api and may open Socket Mode anywhere.
func newSlack(base, credential, proxy string) (*slack, error) {
	app, bot, err := SlackTokens(credential)
	if err != nil {
		return nil, err
	}
	c, err := newHTTPClient(proxy, callTimeout)
	if err != nil {
		return nil, err
	}
	s := &slack{api: SlackAPI, app: app, bot: bot, proxy: proxy, client: c}
	if base != "" {
		s.api, s.anyWSHost = strings.TrimRight(base, "/")+"/api", true
	}
	return s, nil
}

func (s *slack) scrub(text string) string { return scrubber("the app's token", s.app, s.bot)(text) }

// The Web API's errors that say a token is not taken.
var slackRefusals = []string{"invalid_auth", "not_authed", "account_inactive", "token_revoked", "token_expired", "not_allowed_token_type", "missing_scope"}

// call sends a Web API method with token and decodes its answer into out.
func (s *slack) call(ctx context.Context, method, token string, body, out any) error {
	a, err := doJSON(ctx, s.client, http.MethodPost, s.api+"/"+method, "Bearer "+token, body, callTimeout, method, s.scrub)
	if err != nil {
		return err
	}
	if a.status == http.StatusTooManyRequests {
		return &ServiceError{Service: "Slack", Method: method, Code: a.status, Description: "ratelimited", After: max(retryAfterHeader(a.header), time.Second)}
	}
	var res struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Needed string `json:"needed"`
	}
	if err := json.Unmarshal(a.body, &res); err != nil {
		return fmt.Errorf("%s: Slack answered HTTP %d with something that is not the Web API's", method, a.status)
	}
	if !res.OK {
		if slices.Contains(slackRefusals, res.Error) {
			which := "the bot's token (xoxb-)"
			if token == s.app {
				which = "the app-level token (xapp-), which needs the connections:write scope"
			}
			why := "Slack does not take " + which + ": " + res.Error
			if res.Needed != "" {
				why += " (it needs " + s.scrub(res.Needed) + ")"
			}
			return refused(why)
		}
		return &ServiceError{Service: "Slack", Method: method, Code: a.status, Description: s.scrub(res.Error)}
	}
	if out != nil {
		if err := json.Unmarshal(a.body, out); err != nil {
			return fmt.Errorf("%s: Slack's answer could not be read", method)
		}
	}
	return nil
}

func (s *slack) connect(ctx context.Context) (Ready, error) {
	var me struct {
		TeamID string `json:"team_id"`
		Team   string `json:"team"`
		UserID string `json:"user_id"`
		User   string `json:"user"`
		BotID  string `json:"bot_id"`
	}
	if err := s.call(ctx, "auth.test", s.bot, nil, &me); err != nil {
		return Ready{}, err
	}
	if me.UserID == "" || me.TeamID == "" {
		return Ready{}, errors.New("auth.test: Slack named no account")
	}
	if me.BotID == "" {
		return Ready{}, refused("the xoxb- token is not a bot's")
	}
	s.self, s.selfBot = me.UserID, me.BotID
	name := "@" + me.User
	if me.Team != "" {
		name += " in " + me.Team
	}
	return Ready{Account: me.TeamID + "/" + me.UserID, Name: name}, nil
}

// slackEnvelope is one Socket Mode message.
type slackEnvelope struct {
	Type       string `json:"type"`
	EnvelopeID string `json:"envelope_id"`
	Reason     string `json:"reason"`
	Payload    struct {
		Event slackEvent `json:"event"`
	} `json:"payload"`
}

type slackEvent struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	ChannelType string `json:"channel_type"`
	Channel     string `json:"channel"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Text        string `json:"text"`
	TS          string `json:"ts"`
}

func (s *slack) listen(ctx context.Context, r *role) error {
	var open struct {
		URL string `json:"url"`
	}
	if err := s.call(ctx, "apps.connections.open", s.app, nil, &open); err != nil {
		return err
	}
	u, err := url.Parse(open.URL)
	if err != nil || (!s.anyWSHost && (u.Scheme != "wss" || !s.socketHost(u.Hostname()))) {
		return errors.New("apps.connections.open named a Socket Mode address the role does not reach")
	}
	ws, err := dialWebSocket(ctx, open.URL, s.proxy)
	if err != nil {
		return fmt.Errorf("the Slack Socket Mode connection: %s", s.scrub(err.Error()))
	}
	done := make(chan struct{})
	defer func() { close(done); ws.close() }()
	go func() {
		t := time.NewTicker(slackPingEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				ws.close()
				return
			case <-done:
				return
			case <-t.C:
				_ = ws.ping()
			}
		}
	}()
	for {
		b, err := ws.read(slackQuiet)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("the Slack Socket Mode connection: %s", s.scrub(err.Error()))
		}
		var env slackEnvelope
		if err := json.Unmarshal(b, &env); err != nil {
			return errors.New("the Slack Socket Mode connection sent something that is not its JSON")
		}
		if env.EnvelopeID != "" {
			// Every envelope is acknowledged at once, or Slack sends it again.
			ack, _ := json.Marshal(map[string]string{"envelope_id": env.EnvelopeID})
			if err := ws.writeText(ack); err != nil {
				return fmt.Errorf("the Slack Socket Mode connection: %s", s.scrub(err.Error()))
			}
		}
		switch env.Type {
		case "hello":
			r.up()
		case "disconnect":
			return fmt.Errorf("the Slack Socket Mode connection was asked to connect again (%s)", env.Reason)
		case "events_api":
			if env.Payload.Event.Type == "message" {
				r.pass(s.message(env.Payload.Event))
			}
		}
	}
}

// socketHost says host is one of Socket Mode's own, which the relay reaches.
func (s *slack) socketHost(host string) bool {
	k, _ := Lookup(KindSlack)
	for _, h := range k.Hosts {
		if name, _, _ := strings.Cut(h, ":"); name == host && strings.HasPrefix(name, "wss-") {
			return true
		}
	}
	return false
}

var slackUnescaper = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")

// message is an event as the role judges it: direct in the app's own
// conversation with a person (channel_type "im"), and read only when a
// person wrote it with no subtype (an edit, a deletion, a file and a join
// carry one).
func (s *slack) message(e slackEvent) message {
	// A message's ts is its seconds and microseconds, which grow message
	// by message: the role's id is the two as one number.
	sec, frac, _ := strings.Cut(e.TS, ".")
	frac = (frac + "000000")[:6]
	n, _ := strconv.ParseInt(sec+frac, 10, 64)
	out := message{id: n, chat: e.Channel, sender: e.User, direct: e.ChannelType == "im", own: e.User == s.self || (e.BotID != "" && e.BotID == s.selfBot), bot: e.BotID != "", text: slackUnescaper.Replace(e.Text)}
	if secs, err := strconv.ParseInt(sec, 10, 64); err == nil && secs > 0 {
		out.at = time.Unix(secs, 0)
	}
	if e.Subtype != "" {
		out.text = ""
	}
	return out
}

// send sends a part as Slack's mrkdwn, opening no link's preview.
func (s *slack) send(ctx context.Context, chat string, p Part) error {
	body := map[string]any{"channel": chat, "text": p.Rich, "mrkdwn": true, "unfurl_links": false, "unfurl_media": false}
	return s.call(ctx, "chat.postMessage", s.bot, body, nil)
}

func (s *slack) typing(context.Context, string) error { return nil }

// SlackManifest is the app manifest a person creates their Slack app from:
// a bot that takes direct messages in its Messages tab over Socket Mode,
// with the scopes that needs and no other.
func SlackManifest(name string) string {
	m := map[string]any{
		"display_information": map[string]any{"name": name, "description": "Your Agentry agent, reached from Slack."},
		"features": map[string]any{
			"app_home": map[string]any{"home_tab_enabled": false, "messages_tab_enabled": true, "messages_tab_read_only_enabled": false},
			"bot_user": map[string]any{"display_name": name, "always_online": false},
		},
		"oauth_config": map[string]any{"scopes": map[string]any{"bot": []string{"chat:write", "im:history"}}},
		"settings": map[string]any{
			"event_subscriptions":    map[string]any{"bot_events": []string{"message.im"}},
			"interactivity":          map[string]any{"is_enabled": false},
			"org_deploy_enabled":     false,
			"socket_mode_enabled":    true,
			"token_rotation_enabled": false,
		},
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return string(b)
}
