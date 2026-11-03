package messenger

import (
	"slices"
	"strings"
	"testing"
)

func TestCheckCredentialShapeKnowsEveryKind(t *testing.T) {
	good := map[string]string{
		KindTelegram: "123456:" + strings.Repeat("A", 35),
		KindDiscord:  discordTestToken,
		KindSlack:    slackTestCred,
		KindMatrix:   matrixTestToken,
	}
	for _, k := range Names() {
		v, ok := good[k]
		if !ok {
			t.Fatalf("no good credential for %s: the test does not know every kind", k)
		}
		if err := CheckCredentialShape(k, v); err != nil {
			t.Errorf("%s: %v", k, err)
		}
		if err := CheckCredentialShape(k, "not one"); err == nil {
			t.Errorf("%s took a malformed credential", k)
		}
	}
	for _, bad := range []string{slackBotToken, slackAppToken, slackBotToken + " " + slackBotToken, slackTestCred + " extra"} {
		if err := CheckCredentialShape(KindSlack, bad); err == nil {
			t.Errorf("Slack took %q", bad)
		}
	}
}

func TestASlackCredentialIsMaskedTokenByToken(t *testing.T) {
	parts := CredentialParts(KindSlack, slackTestCred)
	if !slices.Contains(parts, slackAppToken) || !slices.Contains(parts, slackBotToken) {
		t.Fatalf("parts = %v, want each token on its own", parts)
	}
	if got := CredentialParts(KindTelegram, testToken); len(got) != 1 || got[0] != testToken {
		t.Fatalf("parts = %v, want the token whole", got)
	}
}

func TestAServersHostIsItsAddressOverTLS(t *testing.T) {
	for in, want := range map[string]string{
		"https://matrix.example.org":       "matrix.example.org:443",
		"https://Matrix.Example.org:8448/": "matrix.example.org:8448",
		"http://127.0.0.1:8008":            "127.0.0.1:8008",
	} {
		if got, err := ServerHost(in); err != nil || got != want {
			t.Errorf("ServerHost(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "matrix.example.org", "http://matrix.example.org", "https://u:p@matrix.example.org", "https://matrix.example.org/path", "https://matrix.example.org?x=1", "ftp://matrix.example.org", "https://192.168.1.10", "https://10.0.0.2:8448", "https://[::1]", "https://a.example.org,evil.example.org", "https://a.example.org;x", "https://a.example.org#", "https://a.example.org?", "https://a.example.org:0", "https://a_b.example.org"} {
		if _, err := ServerHost(in); err == nil {
			t.Errorf("ServerHost(%q) took it", in)
		}
	}
	k, _ := Lookup(KindMatrix)
	if hosts, err := HostsFor(k, "https://matrix.example.org"); err != nil || len(hosts) != 1 || hosts[0] != "matrix.example.org:443" {
		t.Fatalf("HostsFor(matrix) = %v, %v, want the homeserver alone", hosts, err)
	}
	d, _ := Lookup(KindDiscord)
	if hosts, _ := HostsFor(d, "https://ignored.example.org"); !slices.Equal(hosts, d.Hosts) {
		t.Fatalf("HostsFor(discord) = %v, want the kind's own", hosts)
	}
}
