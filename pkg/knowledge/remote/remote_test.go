package remote_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	"github.com/blaktron/agentry-cli/pkg/knowledge/remote"
	"github.com/blaktron/agentry-cli/pkg/knowledge/store"
)

// A refusal reaches the person as the registry's own sentence, with the
// store's sentinel where the store acts on it.
func TestRefusalsCarryTheRegistrysSentence(t *testing.T) {
	ctx := context.Background()
	reg := knowledgetest.NewRegistry(t)
	alice, err := remote.New(reg.URL, reg.Token("alice"), nil)
	if err != nil {
		t.Fatal(err)
	}
	dk := bytes.Repeat([]byte{9}, 32)
	wk, err := alice.Wrap(ctx, dk)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := alice.Unwrap(ctx, wk); err != nil || !bytes.Equal(got, dk) {
		t.Fatalf("unwrap for its owner: %v", err)
	}

	bob, _ := remote.New(reg.URL, reg.Token("bob"), nil)
	_, err = bob.Unwrap(ctx, wk)
	if !errors.Is(err, store.ErrKeyNotYours) || !strings.Contains(err.Error(), "does not open for this account") {
		t.Fatalf("another account's unwrap: %v", err)
	}
	if _, err := alice.Create(ctx, "notes", wk); err != nil {
		t.Fatal(err)
	}
	_, err = alice.Create(ctx, "notes", wk)
	if !errors.Is(err, store.ErrRemoteNameTaken) || !strings.Contains(err.Error(), "already keeps a knowledge pack of this name") {
		t.Fatalf("a name taken: %v", err)
	}
	stranger, _ := remote.New(reg.URL, "agyp_not_a_token", nil)
	if _, err := stranger.List(ctx); !errors.Is(err, store.ErrSignedOut) || !strings.Contains(err.Error(), "agentry login") {
		t.Fatalf("an unknown token: %v", err)
	}
	reg.Forbid = true
	if _, err := alice.List(ctx); err == nil || !strings.Contains(err.Error(), "knowledge:read, knowledge:write") {
		t.Fatalf("a token without the scopes: %v", err)
	}
}

func TestNoTokenIsSignedOut(t *testing.T) {
	if _, err := remote.New("https://registry.example", "", nil); !errors.Is(err, store.ErrSignedOut) {
		t.Fatalf("err = %v", err)
	}
}
