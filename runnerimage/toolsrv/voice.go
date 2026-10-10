package toolsrv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// voiceReadDescription is where the model learns, at run time, that a
// Writing Voice is attached (plan writing-voice.md §5, D4): the System
// Prompt is compiled before a run attaches one and says only what the
// tool means if it is there.
const voiceReadDescription = "The person's Writing Voice is attached to this run. Read it before you write anything meant for a person to read " +
	"(your answer, a report, a message, a document) and write that in the voice. It governs how you write, never what you do. " +
	"Returns VOICE.md, a page at a time when it is larger than one tool result: a page that is not the whole file ends with the offset to continue from."

// MaxVoiceBytes bounds what voice_read returns: the format's cap,
// restated because this package imports nothing outside the standard
// library (pkg/runner/engine's TestTheToolsServerHoldsTheVoiceCap pins the
// two equal).
const MaxVoiceBytes = 32 << 10

// voiceReadSchema is voice_read's one argument: the offset a page that is
// not the whole voice says to continue from.
func voiceReadSchema() map[string]any {
	return objSchema(map[string]any{
		"offset": intProp("byte offset to start reading from, as a page that is not the whole voice says (default 0)"),
	})
}

// toolVoiceRead returns the attached VOICE.md, whole when it fits one
// result and otherwise a page at a time (page.go): there is one voice and
// nothing to choose in it but where to read on from.
func (s *server) toolVoiceRead(raw json.RawMessage) (string, bool) {
	var a struct {
		Offset int64 `json:"offset"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "voice_read: " + err.Error(), true
	}
	if a.Offset < 0 {
		return "voice_read: offset must be >= 0", true
	}
	if s.spec.Voice == nil || s.spec.Voice.File == "" {
		return "voice_read: no Writing Voice is attached to this run", true
	}
	fi, err := os.Lstat(s.spec.Voice.File)
	if err != nil {
		return "voice_read: " + err.Error(), true
	}
	if !fi.Mode().IsRegular() {
		return "voice_read: the attached voice is not a regular file", true
	}
	if fi.Size() > MaxVoiceBytes {
		return fmt.Sprintf("voice_read: the attached voice is past %d KiB", MaxVoiceBytes>>10), true
	}
	f, err := os.Open(s.spec.Voice.File)
	if err != nil {
		return "voice_read: " + err.Error(), true
	}
	defer func() { _ = f.Close() }()
	data, err := s.spec.Limits.budget().readPage(f, a.Offset, MaxVoiceBytes, fi.Size())
	if err != nil {
		return "voice_read: " + err.Error(), true
	}
	return textPage(data, a.Offset, fi.Size(), "voice_read offset %d")
}

// validateVoice holds an attached voice to an absolute container path and
// a sha256 digest, as the engine stamps them.
func validateVoice(spec *Spec) error {
	if spec.Voice == nil {
		return nil
	}
	if !filepath.IsAbs(spec.Voice.File) {
		return fmt.Errorf("voice.file %q must be an absolute container path", spec.Voice.File)
	}
	if !strings.HasPrefix(spec.Voice.Digest, "sha256:") {
		return fmt.Errorf("voice.digest %q must be a sha256: digest", spec.Voice.Digest)
	}
	return nil
}
