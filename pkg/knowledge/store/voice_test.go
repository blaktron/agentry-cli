package store_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	"github.com/blaktron/agentry-cli/pkg/knowledge/store"
	"github.com/blaktron/agentry-cli/pkg/voice"
)

var (
	voiceV1 = []byte("# How Sam writes\n\nShort sentences. The verb early.\n")
	voiceV2 = []byte("# How Sam writes\n\nShort sentences. The verb early. Never a semicolon.\n")
	t0      = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
)

// TestAVoiceIsKeptSealedAndOpensIntoMemory: this computer keeps one voice,
// sealed (no text of it in the clear on disk), and opens it to the digest
// its record names; the first is made with always-attach on.
func TestAVoiceIsKeptSealedAndOpensIntoMemory(t *testing.T) {
	ctx := context.Background()
	for _, owner := range []string{"", "alice"} {
		t.Run("signed in as "+owner, func(t *testing.T) {
			reg := knowledgetest.NewRegistry(t)
			a := newMachine(t, reg, owner)
			if _, err := a.store().Voice(); !errors.Is(err, voice.ErrNoVoice) {
				t.Fatalf("an empty store's voice: %v", err)
			}
			v, created, err := a.store().KeepVoice(ctx, voiceV1, voice.SourceCreate, &voice.Creator{CLI: "test", Model: "m"}, t0)
			if err != nil || !created {
				t.Fatalf("keep: created %v, %v", created, err)
			}
			if !v.Meta.AlwaysAttach || v.Meta.SHA256 != voice.Digest(voiceV1) || v.Meta.Bytes != len(voiceV1) || !strings.HasPrefix(v.Meta.ID, "vo_") {
				t.Fatalf("record %+v", v.Meta)
			}
			wantWrapper := store.WrapperMachine
			if owner != "" {
				wantWrapper = store.WrapperRegistry
			}
			if v.Key.Wrapper != wantWrapper {
				t.Fatalf("wrapped by %s, want %s", v.Key.Wrapper, wantWrapper)
			}
			assertNoClearText(t, a.home, voiceV1)
			text, got, err := a.store().OpenVoice(ctx)
			if err != nil || !bytes.Equal(text, voiceV1) || got.Meta.ID != v.Meta.ID {
				t.Fatalf("open: %q %v", text, err)
			}
		})
	}
}

// TestANewVersionKeepsTheVoicesIdentityAndFlag: a new version replaces the
// text and keeps the id, the key, when it was made and a flag the person
// turned off.
func TestANewVersionKeepsTheVoicesIdentityAndFlag(t *testing.T) {
	ctx := context.Background()
	a := newMachine(t, knowledgetest.NewRegistry(t), "")
	first, _, err := a.store().KeepVoice(ctx, voiceV1, voice.SourceCreate, nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store().SetVoiceAttach(false); err != nil {
		t.Fatal(err)
	}
	second, created, err := a.store().KeepVoice(ctx, voiceV2, voice.SourceEdit, nil, t0.Add(time.Hour))
	if err != nil || created {
		t.Fatalf("second keep: created %v, %v", created, err)
	}
	if second.Meta.ID != first.Meta.ID || second.Meta.Created != first.Meta.Created || second.Meta.AlwaysAttach || second.Meta.Source != voice.SourceEdit {
		t.Fatalf("second record %+v, first %+v", second.Meta, first.Meta)
	}
	if second.Key.WrappedKey.Recipient != first.Key.WrappedKey.Recipient || !bytes.Equal(second.Key.WrappedKey.Key, first.Key.WrappedKey.Key) {
		t.Fatal("a new version rewrapped the key")
	}
	text, _, err := a.store().OpenVoice(ctx)
	if err != nil || !bytes.Equal(text, voiceV2) {
		t.Fatalf("open: %q %v", text, err)
	}
	if entries, _ := os.ReadDir(a.home); len(entries) != 1 || entries[0].Name() != store.VoiceDir {
		t.Fatalf("the runner home holds %v, want the voice alone (no stage left)", entries)
	}
}

func TestAVoiceOutsideTheFormatIsNotKept(t *testing.T) {
	a := newMachine(t, knowledgetest.NewRegistry(t), "")
	for _, text := range [][]byte{nil, []byte(" \n"), bytes.Repeat([]byte("a"), voice.MaxBytes+1)} {
		if _, _, err := a.store().KeepVoice(context.Background(), text, voice.SourceEdit, nil, t0); err == nil {
			t.Errorf("kept a voice of %d bytes", len(text))
		}
	}
	if _, _, err := a.store().KeepVoice(context.Background(), voiceV1, voice.Source("pasted"), nil, t0); err == nil {
		t.Error("kept a voice from an unknown source")
	}
	if _, err := a.store().Voice(); !errors.Is(err, voice.ErrNoVoice) {
		t.Fatalf("a refused voice was kept: %v", err)
	}
}

func TestATamperedVoiceDoesNotOpen(t *testing.T) {
	ctx := context.Background()
	a := newMachine(t, knowledgetest.NewRegistry(t), "")
	v, _, err := a.store().KeepVoice(ctx, voiceV1, voice.SourceCreate, nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	sealed := filepath.Join(v.Dir, store.VoiceSealedFile)
	raw, err := os.ReadFile(sealed)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(sealed, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.store().OpenVoice(ctx); err == nil {
		t.Fatal("a tampered voice opened")
	}
}

func TestDeleteVoiceRemovesItAndItsCachedKey(t *testing.T) {
	ctx := context.Background()
	a := newMachine(t, knowledgetest.NewRegistry(t), "alice")
	v, _, err := a.store().KeepVoice(ctx, voiceV1, voice.SourceCreate, nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	if !a.keys.has("knowledge:" + v.Meta.ID) {
		t.Fatalf("the registry-wrapped key was not cached: %v", a.keys.m)
	}
	a.owner = "" // offline: the cached key opens it
	if _, _, err := a.store().OpenVoice(ctx); err != nil {
		t.Fatalf("a cached key does not open the voice offline: %v", err)
	}
	if err := a.store().DeleteVoice(); err != nil {
		t.Fatal(err)
	}
	if a.keys.has("knowledge:" + v.Meta.ID) {
		t.Fatal("delete left the cached key")
	}
	if _, err := a.store().Voice(); !errors.Is(err, voice.ErrNoVoice) {
		t.Fatalf("after delete: %v", err)
	}
}

// assertNoClearText walks dir for any file holding text.
func assertNoClearText(t *testing.T, dir string, text []byte) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, rerr := os.ReadFile(p)
		if rerr == nil && bytes.Contains(raw, bytes.TrimSpace(text)[:20]) {
			t.Errorf("%s holds the voice's text in the clear", p)
		}
		return nil
	})
}
