// Package voice is the Writing Voice (plan writing-voice.md §4, D1–D3;
// agentry-cli#531): the instructions an LLM follows to write as a person
// writes, one Markdown file, VOICE.md, at most MaxBytes. It is not a
// knowledge pack: it has no wiki, no sources and no name, and a person
// keeps one. A run reads it through the one tool voice_read, granted only
// when a voice is attached, and the System Prompt's head says what it
// governs and what it never does (D4).
//
// This package is the format: what a VOICE.md must be, its digest, and
// the record kept beside it. The local store keeps it sealed
// (pkg/knowledge/store), the Voice Writer writes it (pkg/voice/writer,
// pkg/voice/build), and the engine stages it into a run.
package voice

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

const (
	// File is the voice's one file.
	File = "VOICE.md"
	// MaxBytes caps a VOICE.md (D3): the drafted instruction's cap, so a
	// full LLM skill fits and AI Review reads it in one chunk.
	MaxBytes = 32 << 10
	// MetaSchema is the format of the record kept beside a voice.
	MetaSchema = "agentry-voice/v1"
)

// ErrNoVoice is what a store that keeps no voice answers.
var ErrNoVoice = errors.New("no Writing Voice is kept on this computer; make one with `agentry voice create --file SAMPLE`")

// Check holds text to the format: UTF-8 Markdown with something in it,
// no NUL, at most MaxBytes.
func Check(text []byte) error {
	switch {
	case len(bytes.TrimSpace(text)) == 0:
		return errors.New("a Writing Voice is empty")
	case len(text) > MaxBytes:
		return fmt.Errorf("a Writing Voice is %d bytes, past its %d KiB cap", len(text), MaxBytes>>10)
	case !utf8.Valid(text):
		return errors.New("a Writing Voice is not UTF-8 text")
	case bytes.IndexByte(text, 0) >= 0:
		return errors.New("a Writing Voice holds a NUL byte, which text does not")
	}
	return nil
}

// Digest is text's digest, "sha256:<hex>".
func Digest(text []byte) string {
	sum := sha256.Sum256(text)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Source is how a version of the voice came to be.
type Source string

const (
	// SourceCreate is the Voice Writer over writing samples and a brief.
	SourceCreate Source = "create"
	// SourceImprove is the Voice Writer over the current voice, more
	// samples and a brief.
	SourceImprove Source = "improve"
	// SourceAIEdit is the Voice Writer over the current voice and a note.
	SourceAIEdit Source = "ai-edit"
	// SourceEdit is the person's own text.
	SourceEdit Source = "edit"
	// SourceDownload is the account's live voice, downloaded (V3,
	// agentry-cli#532): AI Review passed it in the account before it was
	// live there.
	SourceDownload Source = "download"
)

// Valid says whether s is a source this client knows.
func (s Source) Valid() bool {
	switch s {
	case SourceCreate, SourceImprove, SourceAIEdit, SourceEdit, SourceDownload:
		return true
	}
	return false
}

// Meta is the record kept in the clear beside a sealed voice: what it is,
// never what it says.
type Meta struct {
	Schema string `json:"schema"`
	// ID names the voice's sealed copy and wraps its key ("vo_" and 32
	// hex digits). A new version keeps it.
	ID      string `json:"id"`
	SHA256  string `json:"sha256"`
	Bytes   int    `json:"bytes"`
	Source  Source `json:"source"`
	Created string `json:"created"`
	Updated string `json:"updated"`
	// AlwaysAttach is this computer's voice's own flag (D11): every run
	// here attaches it unless the run says not to. On when a voice is
	// first made.
	AlwaysAttach bool `json:"alwaysAttach"`
	// Creator is the client and model that wrote the last version; empty
	// for the person's own text.
	Creator *Creator `json:"creator,omitempty"`
}

// Creator is what wrote a version.
type Creator struct {
	CLI   string `json:"cli,omitempty"`
	Model string `json:"model,omitempty"`
}

// Stamp formats a time as the record does.
func Stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// Describe is how the receipt and the CLI name a version: its digest, its
// size and where it came from.
func (m Meta) Describe() string {
	if m.Source == SourceDownload {
		return fmt.Sprintf("%s, %d bytes, %s (reviewed there, not checked again here)", m.SHA256, m.Bytes, m.Source.Phrase())
	}
	return fmt.Sprintf("%s, %d bytes, %s on this computer, not reviewed", m.SHA256, m.Bytes, m.Source.Phrase())
}

// Origin is where this computer's voice came from, as a run's receipt
// records it. A downloaded voice was reviewed in the account; nothing here
// checks it again, and the record kept beside it is not authenticated
// (DR-56), so the receipt says no more than that.
func (m Meta) Origin() string {
	if m.Source == SourceDownload {
		return "kept on this computer, " + m.Source.Phrase() + " (reviewed there, not checked again here)"
	}
	return "kept on this computer, " + m.Source.Phrase() + ", not reviewed"
}

// Phrase is the source in a sentence.
func (s Source) Phrase() string {
	switch s {
	case SourceCreate:
		return "made by the Voice Writer"
	case SourceImprove:
		return "improved by the Voice Writer"
	case SourceAIEdit:
		return "edited by the Voice Writer"
	case SourceEdit:
		return "written by hand"
	case SourceDownload:
		// The record beside the voice is not authenticated (DR-56), so the
		// phrase names where it came from and claims no review itself.
		return "downloaded from your account"
	}
	return "of unknown origin"
}
