package connection_test

import (
	"context"
	"errors"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/mcpauth/mcpauthtest"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
)

// A connection handed to another machine refreshes there from its own
// store, and forgetting it there leaves the vendor's grant alone.
func TestAHandedConnectionRefreshesOnTheOtherMachine(t *testing.T) {
	v := mcpauthtest.New(t)
	v.DCR = true
	here := connection.Open(t.TempDir(), &fakeKeys{m: map[string]string{}})
	k := release
	k.Server = v.ServerURL()
	g := grant(t, v)
	if _, err := here.Put(k, g, ""); err != nil {
		t.Fatal(err)
	}
	h, err := here.Hand(k, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.RefreshToken != g.Token.RefreshToken || h.Record.Server != k.Server {
		t.Fatalf("handed %+v", h.Record)
	}
	thereKeys := &fakeKeys{m: map[string]string{}}
	there := connection.Open(t.TempDir(), thereKeys)
	if err := there.Take(h, nil); err != nil {
		t.Fatal(err)
	}
	if len(thereKeys.m) != 1 {
		t.Fatalf("the other machine keeps %d secrets", len(thereKeys.m))
	}
	a, _, err := there.Use(context.Background(), k, v.Guard().Client(), nil)
	if err != nil || a.Token == "" {
		t.Fatalf("the handed connection did not refresh: %q %v", a.Token, err)
	}
	if err := there.ForgetHanded(k); err != nil {
		t.Fatal(err)
	}
	if _, err := there.Get(k); !errors.Is(err, connection.ErrNone) {
		t.Fatalf("the forgotten connection is still kept: %v", err)
	}
	if len(thereKeys.m) != 0 {
		t.Fatal("its secret stayed in the keychain")
	}
	if err := there.Take(&connection.Handed{Record: h.Record}, nil); err == nil {
		t.Fatal("a handed connection without its refresh token was taken")
	}
}
