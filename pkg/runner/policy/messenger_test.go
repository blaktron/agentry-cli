package policy

import (
	"strings"
	"testing"
)

func TestMessengerAllowed(t *testing.T) {
	off, on := false, true
	for _, c := range []struct {
		name  string
		p     *Policy
		kind  string
		allow bool
		why   string
	}{
		{"no policy", nil, "telegram", true, ""},
		{"unset", &Policy{}, "telegram", true, ""},
		{"on", &Policy{AllowMessengers: &on}, "telegram", true, ""},
		{"off", &Policy{AllowMessengers: &off}, "telegram", false, "allowMessengers: false"},
		{"listed", &Policy{MessengerKinds: []string{"Telegram"}}, "telegram", true, ""},
		{"not listed", &Policy{MessengerKinds: []string{"telegram"}}, "discord", false, "does not allow discord"},
	} {
		ok, why := c.p.MessengerAllowed(c.kind)
		if ok != c.allow || (c.why != "" && !strings.Contains(why, c.why)) {
			t.Errorf("%s: MessengerAllowed(%s) = %v, %q", c.name, c.kind, ok, why)
		}
	}
}

func TestValidateRefusesAnUnknownMessengerKind(t *testing.T) {
	p := Default()
	p.MessengerKinds = []string{"telegarm"}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "telegarm") {
		t.Fatalf("Validate = %v", err)
	}
	p.MessengerKinds = []string{"telegram"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}
