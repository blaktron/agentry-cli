// voice_read tests (plan writing-voice.md D4, agentry-cli#531): the tool
// exists exactly when the engine attached a voice, returns the file whole,
// and refuses anything that is not the regular file it was given.

package toolsrv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const voiceCall = `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"voice_read","arguments":{}}}`

func voiceSpec(t *testing.T, text string) *Spec {
	t.Helper()
	s := baseSpec(t)
	file := filepath.Join(t.TempDir(), ".voice", "VOICE.md")
	mustWrite(t, file, []byte(text))
	s.Voice = &VoiceFile{File: file, Digest: "sha256:bbbb"}
	return s
}

func TestVoiceReadExistsOnlyWhenAVoiceIsAttached(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec func(*testing.T) *Spec
		want string
	}{
		{"nothing attached", baseSpec, "write_report"},
		{"a voice attached", func(t *testing.T) *Spec { return voiceSpec(t, "# Voice\n") }, "voice_read,write_report"},
		{"beside a pack", func(t *testing.T) *Spec {
			s := knowledgeSpec(t)
			s.Voice = &VoiceFile{File: "/knowledge/.voice/VOICE.md", Digest: "sha256:bbbb"}
			return s
		}, "knowledge_list,knowledge_search,knowledge_read,voice_read,write_report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resps := runServe(t, tc.spec(t), initLine, listLine)
			if got := strings.Join(toolNames(t, resps[1]), ","); got != tc.want {
				t.Fatalf("tools/list = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestVoiceReadReturnsTheVoiceWhole(t *testing.T) {
	const text = "# How Sam writes\n\nShort sentences. The verb early.\n"
	resps := runServe(t, voiceSpec(t, text), initLine, listLine, voiceCall)
	for _, raw := range resultOf(t, resps[1])["tools"].([]any) {
		tool := raw.(map[string]any)
		if tool["name"] == ToolVoiceRead && !strings.Contains(tool["description"].(string), "Writing Voice is attached") {
			t.Errorf("voice_read's description does not say a voice is attached: %q", tool["description"])
		}
	}
	res := resultOf(t, resps[2])
	if res["isError"] != false {
		t.Fatalf("voice_read errored: %v", res)
	}
	if got := res["content"].([]any)[0].(map[string]any)["text"]; got != text {
		t.Fatalf("voice_read = %q, want %q", got, text)
	}
}

func TestVoiceReadRefusesWhatIsNotTheFile(t *testing.T) {
	t.Run("a link", func(t *testing.T) {
		s := baseSpec(t)
		target := filepath.Join(t.TempDir(), "secret")
		mustWrite(t, target, []byte("not the voice"))
		link := filepath.Join(t.TempDir(), "VOICE.md")
		if err := os.Symlink(target, link); err != nil {
			t.Skip(err)
		}
		s.Voice = &VoiceFile{File: link, Digest: "sha256:bbbb"}
		res := resultOf(t, runServe(t, s, initLine, listLine, voiceCall)[2])
		if res["isError"] != true {
			t.Fatalf("voice_read followed a link: %v", res)
		}
	})
	t.Run("past the cap", func(t *testing.T) {
		res := resultOf(t, runServe(t, voiceSpec(t, strings.Repeat("a", 32<<10+1)), initLine, listLine, voiceCall)[2])
		if res["isError"] != true {
			t.Fatalf("voice_read returned a voice past its cap: %v", res)
		}
	})
}

func TestValidateSpecVoice(t *testing.T) {
	abs := t.TempDir()
	for _, tc := range []struct {
		name    string
		voice   *VoiceFile
		wantErr string
	}{
		{"none", nil, ""},
		{"valid", &VoiceFile{File: "/knowledge/.voice/VOICE.md", Digest: "sha256:1"}, ""},
		{"relative", &VoiceFile{File: "VOICE.md", Digest: "sha256:1"}, "absolute"},
		{"no digest", &VoiceFile{File: "/knowledge/.voice/VOICE.md"}, "sha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSpec(&Spec{SpecVersion: SpecVersion, WorkspaceDir: abs, OutDir: abs, Voice: tc.voice})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateSpec: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateSpec = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
