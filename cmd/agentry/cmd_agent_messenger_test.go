package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

const cliBotToken = "987654321:AAcliTestTokenValueThatIsLongEnough_x"

// fakeTelegram answers getMe for cliBotToken and records sendMessage.
func fakeTelegram(t *testing.T) *[]map[string]any {
	t.Helper()
	var sent []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bot"+cliBotToken+"/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized"})
			return
		}
		switch strings.TrimPrefix(r.URL.Path, "/bot"+cliBotToken+"/") {
		case "getMe":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"id": 987654321, "is_bot": true, "first_name": "Helper", "username": "my_helper_bot"}})
		case "sendMessage":
			var p map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			sent = append(sent, p)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 1}})
		}
	}))
	t.Cleanup(srv.Close)
	old := messengerAPI
	messengerAPI = srv.URL
	t.Cleanup(func() { messengerAPI = old })
	return &sent
}

// TestAgentMessengerAddPairListRemove: add checks the token with Telegram
// and keeps it in the keychain alone, asking agreement first; pair records
// the code's digest and never the code; list and show print the account and
// never the token; remove takes both away.
func TestAgentMessengerAddPairListRemove(t *testing.T) {
	home := agentHome(t)
	keys := memSecrets{}
	old := agentSecrets
	agentSecrets = keys
	t.Cleanup(func() { agentSecrets = old })
	sent := fakeTelegram(t)
	account := persistent.MessengerKeychainAccount("assistant", "telegram")

	if _, _, err := runCLI(t, strings.NewReader("not a token\n"), "agent", "messenger", "add", "assistant", "telegram", "--yes"); err == nil || exitCode(err) != engine.ExitUsage || !strings.Contains(err.Error(), "BotFather") {
		t.Fatalf("a malformed token: %v", err)
	}
	if _, _, err := runCLI(t, strings.NewReader("123456789:AAanotherTokenTheServiceDoesNotKnow_y\n"), "agent", "messenger", "add", "assistant", "telegram", "--yes"); err == nil || !strings.Contains(err.Error(), "did not take the token") || strings.Contains(err.Error(), "AAanother") {
		t.Fatalf("a refused token: %v", err)
	}
	_, asked, err := runCLI(t, strings.NewReader(cliBotToken+"\n"), "agent", "messenger", "add", "assistant", "telegram")
	if err == nil || !strings.Contains(asked, "Takes your messages on Telegram, through your bot @my_helper_bot") {
		t.Fatalf("unagreed: %v\n%s", err, asked)
	}
	if _, has := keys[account]; has {
		t.Fatal("an unagreed messenger's token was kept")
	}
	out, errOut, err := runCLI(t, strings.NewReader(cliBotToken+"\n"), "agent", "messenger", "add", "assistant", "telegram", "--yes")
	if err != nil || !strings.Contains(out, "Telegram took the token of @my_helper_bot") || strings.Contains(out+errOut, cliBotToken) {
		t.Fatalf("add: %v\n%s\n%s", err, out, errOut)
	}
	if keys[account] != cliBotToken {
		t.Fatal("the token is not in the keychain")
	}
	cfg, err := persistent.LoadConfig(home, "assistant")
	if err != nil || len(cfg.Messengers) != 1 || cfg.Messengers[0].Account != "987654321" || cfg.Messengers[0].Name != "@my_helper_bot" {
		t.Fatalf("config: %v %+v", err, cfg.Messengers)
	}

	out, _, err = runCLI(t, nil, "agent", "messenger", "pair", "assistant", "telegram", "--no-wait", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(out), &p); err != nil || len(p.Code) != 8 {
		t.Fatalf("pair --json: %v %s", err, out)
	}
	cfg, _ = persistent.LoadConfig(home, "assistant")
	if pr := cfg.Messengers[0].Pairing; pr == nil || pr.CodeDigest == "" {
		t.Fatalf("no pairing recorded: %+v", cfg.Messengers[0])
	}
	raw, _ := json.Marshal(cfg)
	if strings.Contains(raw2(t, home), p.Code) || strings.Contains(string(raw), p.Code) {
		t.Fatal("config.yaml holds the pairing code itself")
	}

	out, _, err = runCLI(t, nil, "agent", "messenger", "list", "assistant")
	if err != nil || !strings.Contains(out, "@my_helper_bot") || !strings.Contains(out, "a code is taken until") || strings.Contains(out, cliBotToken) {
		t.Fatalf("list: %v\n%s", err, out)
	}
	out, _, err = runCLI(t, nil, "agent", "show", "assistant", "--json")
	if err != nil || !strings.Contains(out, `"messengers"`) || !strings.Contains(out, `"messenger:telegram"`) || strings.Contains(out, cliBotToken) {
		t.Fatalf("show --json: %v\n%s", err, out)
	}

	// Paired, test sends to the paired chat alone.
	cfg, _ = persistent.LoadConfig(home, "assistant")
	cfg.Messengers[0].Peer = &persistent.MessengerPeer{Sender: "111", Chat: "111"}
	if err := persistent.SaveConfig(home, "assistant", cfg); err != nil {
		t.Fatal(err)
	}
	out, _, err = runCLI(t, nil, "agent", "messenger", "test", "assistant", "telegram")
	if err != nil || !strings.Contains(out, "Sent a test message") || len(*sent) != 1 || (*sent)[0]["chat_id"] != "111" {
		t.Fatalf("test: %v\n%s\n%v", err, out, *sent)
	}

	out, _, err = runCLI(t, nil, "agent", "messenger", "remove", "assistant", "telegram")
	if err != nil || !strings.Contains(out, "Removed Telegram") {
		t.Fatalf("remove: %v\n%s", err, out)
	}
	if _, has := keys[account]; has {
		t.Fatal("remove left the token in the keychain")
	}
	cfg, _ = persistent.LoadConfig(home, "assistant")
	if len(cfg.Messengers) != 0 {
		t.Fatalf("remove left %+v", cfg.Messengers)
	}
}

// raw2 is the agent's config.yaml as it is on disk.
func raw2(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(persistent.Dir(home, "assistant"), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
