package store

// The Writing Voice this computer keeps (plan writing-voice.md §4, D6;
// agentry-cli#531): at most one, in <runner home>/voice/, sealed the way a
// kept pack is and beside the knowledge store rather than in it, since a
// voice is not a pack:
//
//	voice.json    its record (pkg/voice.Meta), in the clear: the digest,
//	              the size, where it came from, its always-attach flag
//	voice.sealed  VOICE.md sealed with the core's pkg/packseal under a
//	              data key of its own
//	key.json      that data key as it is wrapped: by the registry the
//	              person is signed in to, else by this machine's key
//
// No text of the voice rests on disk in the clear: it is opened into
// memory (it is at most 32 KiB), and a run writes its own copy into the
// run's knowledge channel, which dies at teardown. It is never reviewed:
// the account's voice, which AI Review passes before it is live, is the
// registry's (V2), and this computer's is moved to it only by the person.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/packseal"

	"github.com/blaktron/agentry-cli/pkg/voice"
)

// The voice's fixed names.
const (
	// VoiceDir is the voice's directory under the runner home.
	VoiceDir = "voice"
	// VoiceSealedFile is the sealed VOICE.md.
	VoiceSealedFile = "voice.sealed"
	// VoiceMetaFile is the voice's record.
	VoiceMetaFile = "voice.json"
)

// Voice is the voice this computer keeps, without its text.
type Voice struct {
	Dir  string
	Meta voice.Meta
	Key  KeyRecord
}

// voiceDir is <runner home>/voice.
func (s *Store) voiceDir() string { return filepath.Join(filepath.Dir(s.dir), VoiceDir) }

// Voice is the record of the voice this computer keeps, or voice.ErrNoVoice.
func (s *Store) Voice() (Voice, error) {
	dir := s.voiceDir()
	var v Voice
	raw, err := os.ReadFile(filepath.Join(dir, VoiceMetaFile)) //nolint:gosec // the store's own record
	if errors.Is(err, os.ErrNotExist) {
		return Voice{}, voice.ErrNoVoice
	}
	if err != nil {
		return Voice{}, err
	}
	if err := json.Unmarshal(raw, &v.Meta); err != nil {
		return Voice{}, fmt.Errorf("the Writing Voice's record: %w", err)
	}
	if v.Meta.Schema != voice.MetaSchema || !validVoiceID(v.Meta.ID) {
		return Voice{}, fmt.Errorf("the Writing Voice's record is not %s with a vo_ id", voice.MetaSchema)
	}
	raw, err = os.ReadFile(filepath.Join(dir, KeyFile)) //nolint:gosec // the store's own record
	if err != nil {
		return Voice{}, fmt.Errorf("the Writing Voice's key: %w", err)
	}
	if err := json.Unmarshal(raw, &v.Key); err != nil {
		return Voice{}, fmt.Errorf("the Writing Voice's key: %w", err)
	}
	v.Dir = dir
	return v, nil
}

// KeepVoice keeps text as this computer's voice, in place of the one it
// keeps, if any. A new version keeps the voice's id, its data key, its
// always-attach flag and when it was first made; the first voice is made
// with always-attach on (D11), and created says it was the first.
func (s *Store) KeepVoice(ctx context.Context, text []byte, src voice.Source, by *voice.Creator, now time.Time) (v Voice, created bool, err error) {
	if err := voice.Check(text); err != nil {
		return Voice{}, false, err
	}
	if !src.Valid() {
		return Voice{}, false, fmt.Errorf("the Writing Voice's source %q is not one this client knows", src)
	}
	stamp := voice.Stamp(now)
	m := voice.Meta{Schema: voice.MetaSchema, SHA256: voice.Digest(text), Bytes: len(text), Source: src,
		Created: stamp, Updated: stamp, AlwaysAttach: true, Creator: by}
	var dataKey []byte
	var kr KeyRecord
	old, err := s.Voice()
	switch {
	case err == nil:
		m.ID, m.Created, m.AlwaysAttach = old.Meta.ID, old.Meta.Created, old.Meta.AlwaysAttach
		if dataKey, _, err = s.ownKey(ctx, old.Meta.ID, "the Writing Voice", old.Key, false); err != nil {
			return Voice{}, false, err
		}
		kr = old.Key
	case errors.Is(err, voice.ErrNoVoice):
		created = true
		if m.ID, err = newVoiceID(); err != nil {
			return Voice{}, false, err
		}
		if dataKey, err = packseal.NewKey(); err != nil {
			return Voice{}, false, err
		}
		if kr, err = s.wrapNew(ctx, m.ID, dataKey); err != nil {
			return Voice{}, false, fmt.Errorf("the Writing Voice: %w", err)
		}
	default:
		return Voice{}, false, err
	}
	seal := func(w io.Writer) error {
		return packseal.Seal(w, dataKey, packseal.Header{Pack: m.ID, Digest: m.SHA256}, bytes.NewReader(text))
	}
	if v, err = s.writeVoice(m, kr, seal); err != nil {
		return Voice{}, false, fmt.Errorf("the Writing Voice: keep it: %w", err)
	}
	if kr.Wrapper == WrapperRegistry {
		s.cache(m.ID, dataKey)
	}
	return v, created, nil
}

// OpenVoice is this computer's voice and its text, opened into memory and
// held to the digest its record names.
func (s *Store) OpenVoice(ctx context.Context) ([]byte, Voice, error) {
	v, err := s.Voice()
	if err != nil {
		return nil, Voice{}, err
	}
	sealed, err := os.ReadFile(filepath.Join(v.Dir, VoiceSealedFile)) //nolint:gosec // the store's own file
	if err != nil {
		return nil, Voice{}, fmt.Errorf("the Writing Voice: %w", err)
	}
	key, fromCache, err := s.ownKey(ctx, v.Meta.ID, "the Writing Voice", v.Key, false)
	if err != nil {
		return nil, Voice{}, err
	}
	text, err := openVoice(v, sealed, key)
	if err != nil && fromCache && errors.Is(err, packseal.ErrOpen) {
		// A cached key that no longer opens it is dropped, and the key
		// asked for through its wrapper, as for a pack.
		_ = s.uncache(v.Meta.ID)
		if key, _, err = s.ownKey(ctx, v.Meta.ID, "the Writing Voice", v.Key, true); err != nil {
			return nil, Voice{}, err
		}
		text, err = openVoice(v, sealed, key)
	}
	if err != nil {
		return nil, Voice{}, err
	}
	return text, v, nil
}

func openVoice(v Voice, sealed, key []byte) ([]byte, error) {
	h, plain, err := packseal.Open(bytes.NewReader(sealed), key)
	if err != nil {
		return nil, fmt.Errorf("the Writing Voice: %w", err)
	}
	if h.Pack != v.Meta.ID || h.Digest != v.Meta.SHA256 {
		return nil, fmt.Errorf("the Writing Voice: its sealed text is %s with digest %s, not the %s and %s its record names", h.Pack, h.Digest, v.Meta.ID, v.Meta.SHA256)
	}
	text, err := io.ReadAll(io.LimitReader(plain, voice.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("the Writing Voice: %w", err)
	}
	if voice.Digest(text) != v.Meta.SHA256 {
		return nil, errors.New("the Writing Voice: its text is not the one its record names")
	}
	return text, nil
}

// SetVoiceAttach sets this computer's voice's always-attach flag.
func (s *Store) SetVoiceAttach(on bool) (Voice, error) {
	v, err := s.Voice()
	if err != nil {
		return Voice{}, err
	}
	v.Meta.AlwaysAttach = on
	if err := writeJSON(filepath.Join(v.Dir, VoiceMetaFile), v.Meta); err != nil {
		return Voice{}, fmt.Errorf("the Writing Voice: record its attach setting: %w", err)
	}
	return v, nil
}

// DeleteVoice removes this computer's voice and its cached key.
func (s *Store) DeleteVoice() error {
	v, err := s.Voice()
	if err != nil {
		return err
	}
	if err := os.RemoveAll(v.Dir); err != nil {
		return fmt.Errorf("the Writing Voice: delete it: %w", err)
	}
	if err := s.uncache(v.Meta.ID); err != nil {
		return fmt.Errorf("the Writing Voice: remove its cached key: %w", err)
	}
	return nil
}

// writeVoice writes the voice through a staging directory beside its own
// and moves it into place, so a reader sees the old voice or the new one.
func (s *Store) writeVoice(m voice.Meta, kr KeyRecord, seal func(io.Writer) error) (Voice, error) {
	final := s.voiceDir()
	parent := filepath.Dir(final)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return Voice{}, err
	}
	stage, err := os.MkdirTemp(parent, "."+VoiceDir+"-stage-")
	if err != nil {
		return Voice{}, err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	f, err := os.OpenFile(filepath.Join(stage, VoiceSealedFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // our own staging directory
	if err != nil {
		return Voice{}, err
	}
	err = seal(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Voice{}, err
	}
	if err := writeJSON(filepath.Join(stage, VoiceMetaFile), m); err != nil {
		return Voice{}, err
	}
	if err := writeJSON(filepath.Join(stage, KeyFile), kr); err != nil {
		return Voice{}, err
	}
	if _, err := os.Lstat(final); err == nil {
		old := stage + ".old"
		if err := os.Rename(final, old); err != nil {
			return Voice{}, err
		}
		if err := os.Rename(stage, final); err != nil {
			_ = os.Rename(old, final)
			return Voice{}, err
		}
		_ = os.RemoveAll(old)
	} else if err := os.Rename(stage, final); err != nil {
		return Voice{}, err
	}
	return Voice{Dir: final, Meta: m, Key: kr}, nil
}

// validVoiceID holds a voice id to vo_ and 32 lowercase hex digits.
func validVoiceID(id string) bool {
	rest, ok := strings.CutPrefix(id, "vo_")
	if !ok || len(rest) != 32 {
		return false
	}
	_, err := hex.DecodeString(rest)
	return err == nil && strings.ToLower(rest) == rest
}

func newVoiceID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "vo_" + hex.EncodeToString(b), nil
}
