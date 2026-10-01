package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	"github.com/blaktron/agentry-cli/pkg/voice"
	"github.com/blaktron/agentry-cli/pkg/voice/remote"
)

// accountVoice runs `agentry voice …` against reg.
func accountVoice(t *testing.T, reg *knowledgetest.Registry, args ...string) (string, string, error) {
	t.Helper()
	return runCLI(t, strings.NewReader(""), append(append([]string{"voice"}, args...), "--registry", reg.URL)...)
}

// keepLocal keeps text as this computer's voice, its key wrapped by reg.
func keepLocal(t *testing.T, reg *knowledgetest.Registry, text string) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "VOICE.md")
	if err := os.WriteFile(f, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "voice", "set", f, "--registry", reg.URL); err != nil {
		t.Fatalf("set: %v", err)
	}
}

func localMeta(t *testing.T, reg *knowledgetest.Registry) voice.Meta {
	t.Helper()
	out, _, err := runCLI(t, strings.NewReader(""), "voice", "show", "--json", "--registry", reg.URL)
	if err != nil {
		t.Fatalf("show --json: %v", err)
	}
	var m voice.Meta
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Upload submits this computer's voice for review and changes nothing
// here; --wait reports the verdict, a pass as the account's live voice and
// a flag, with its findings, as an error.
func TestUploadSubmitsThisComputersVoiceForReview(t *testing.T) {
	isolateEnv(t)
	newInstall(t)
	reg := knowledgetest.NewRegistry(t)
	signIn(t, reg, "alice")
	keepLocal(t, reg, firstVoice)
	before := localMeta(t, reg)

	out, _, err := accountVoice(t, reg, "upload")
	if err != nil || !strings.Contains(out, "in review") || !strings.Contains(out, "This computer's voice is unchanged") {
		t.Fatalf("upload: %q %v", out, err)
	}
	if after := localMeta(t, reg); after != before {
		t.Fatalf("upload changed this computer's voice: %+v → %+v", before, after)
	}
	out, _, err = accountVoice(t, reg, "status")
	if err != nil || !strings.Contains(out, "no live voice yet") || !strings.Contains(out, "Pending: "+before.SHA256) || !strings.Contains(out, "in review") {
		t.Fatalf("status while in review: %q %v", out, err)
	}

	reg.SetReviewOnRead(knowledgetest.VoicePass)
	out, _, err = accountVoice(t, reg, "upload", "--wait")
	if err != nil || !strings.Contains(out, "AI Review passed it") || !strings.Contains(out, "on for every run In Agentry") {
		t.Fatalf("upload --wait, passed: %q %v", out, err)
	}
	out, _, err = accountVoice(t, reg, "status")
	if err != nil || !strings.Contains(out, "live voice "+before.SHA256) || !strings.Contains(out, "the same as this computer's") {
		t.Fatalf("status after the pass: %q %v", out, err)
	}

	keepLocal(t, reg, secondVoice)
	reg.SetReviewOnRead(knowledgetest.VoiceFlag)
	out, _, err = accountVoice(t, reg, "upload", "--wait")
	if err == nil || !strings.Contains(out, "AI Review flagged it") || !strings.Contains(out, "override question, scored 3 of 3") ||
		strings.Contains(out, "Instruction override") {
		t.Fatalf("upload --wait, flagged: %q %v", out, err)
	}
	out, _, _ = accountVoice(t, reg, "status")
	if !strings.Contains(out, "live voice "+before.SHA256) || !strings.Contains(out, "flagged by AI Review; it will not go live") {
		t.Fatalf("status after the flag: %q", out)
	}
}

// Download keeps the account's live voice here, asks before replacing a
// different one, refuses with none live, and takes nothing that is not
// the digest it came with.
func TestDownloadKeepsTheAccountsLiveVoice(t *testing.T) {
	isolateEnv(t)
	newInstall(t)
	reg := knowledgetest.NewRegistry(t)
	signIn(t, reg, "bob")

	if _, _, err := accountVoice(t, reg, "download"); !errors.Is(err, remote.ErrNoAccountVoice) {
		t.Fatalf("download with no voice in the account: %v", err)
	}
	keepLocal(t, reg, firstVoice)
	if _, _, err := accountVoice(t, reg, "upload"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := accountVoice(t, reg, "download"); err == nil || !strings.Contains(err.Error(), "no live Writing Voice yet (one is in review)") {
		t.Fatalf("download with none live: %v", err)
	}
	reg.Review("bob", knowledgetest.VoicePass)
	out, _, err := accountVoice(t, reg, "download")
	if err != nil || !strings.Contains(out, "already keeps your account's live voice") {
		t.Fatalf("download of the same voice: %q %v", out, err)
	}

	keepLocal(t, reg, secondVoice)
	local := localMeta(t, reg)
	if _, _, err := accountVoice(t, reg, "download"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("download over a different voice without --yes: %v", err)
	}
	if localMeta(t, reg) != local {
		t.Fatal("a refused download changed this computer's voice")
	}
	out, _, err = accountVoice(t, reg, "download", "--yes")
	if err != nil || !strings.Contains(out, "Kept your account's live Writing Voice") {
		t.Fatalf("download --yes: %q %v", out, err)
	}
	m := localMeta(t, reg)
	if m.SHA256 != voice.Digest([]byte(firstVoice)) || m.Source != voice.SourceDownload || !m.AlwaysAttach {
		t.Fatalf("after the download: %+v", m)
	}
	if got := m.Origin(); !strings.Contains(got, "downloaded from your account (reviewed there, not checked again here)") {
		t.Fatalf("the receipt's origin: %q", got)
	}
	// Nothing says AI Review passed this computer's copy: the record beside
	// it is not authenticated (DR-56).
	for _, said := range []string{m.Origin(), m.Describe(), m.Source.Phrase()} {
		if strings.Contains(said, "passed") {
			t.Fatalf("a downloaded voice is described as passed: %q", said)
		}
	}

	keepLocal(t, reg, secondVoice)
	reg.TamperLive("bob")
	if _, _, err := accountVoice(t, reg, "download", "--yes"); err == nil || !strings.Contains(err.Error(), "not the digest it names") {
		t.Fatalf("a live voice that is not its digest: %v", err)
	}
	if localMeta(t, reg).SHA256 != voice.Digest([]byte(secondVoice)) {
		t.Fatal("a voice that is not its digest was kept")
	}
	reg.EscapeLive("bob")
	if _, _, err := accountVoice(t, reg, "download", "--yes"); err == nil || !strings.Contains(err.Error(), "control character") {
		t.Fatalf("a live voice with an escape sequence: %v", err)
	}
	if localMeta(t, reg).SHA256 != voice.Digest([]byte(secondVoice)) {
		t.Fatal("a voice with an escape sequence was kept")
	}
}

// attach --account sets the account's flag and leaves this computer's; an
// account with no voice has no flag to set.
func TestAttachAccountSetsTheAccountsFlag(t *testing.T) {
	isolateEnv(t)
	newInstall(t)
	reg := knowledgetest.NewRegistry(t)
	signIn(t, reg, "carol")
	if _, _, err := accountVoice(t, reg, "attach", "on", "--account"); !errors.Is(err, remote.ErrNoAccountVoice) {
		t.Fatalf("attach --account with no voice in the account: %v", err)
	}
	keepLocal(t, reg, firstVoice)
	if _, _, err := accountVoice(t, reg, "upload"); err != nil {
		t.Fatal(err)
	}
	reg.Review("carol", knowledgetest.VoicePass)
	out, _, err := accountVoice(t, reg, "attach", "off", "--account")
	if err != nil || !strings.Contains(out, "off for runs In Agentry") {
		t.Fatalf("attach off --account: %q %v", out, err)
	}
	if !localMeta(t, reg).AlwaysAttach {
		t.Fatal("attach --account changed this computer's flag")
	}
	out, _, _ = accountVoice(t, reg, "status", "--json")
	var st voiceStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil || st.Account == nil || st.Account.AlwaysAttach || !st.SignedIn || st.Local == nil {
		t.Fatalf("status --json: %s %v", out, err)
	}
}

// Signed out, status shows this computer's voice and says so; upload and
// download are refused as a usage error.
func TestTheAccountCommandsSignedOut(t *testing.T) {
	isolateEnv(t)
	newInstall(t)
	reg := knowledgetest.NewRegistry(t)
	signOut(t)
	keepLocal(t, reg, firstVoice)
	out, _, err := accountVoice(t, reg, "status")
	if err != nil || !strings.Contains(out, "signed out") || !strings.Contains(out, "This computer: sha256:") {
		t.Fatalf("status signed out: %q %v", out, err)
	}
	for _, c := range []string{"upload", "download"} {
		if _, _, err := accountVoice(t, reg, c); !errors.Is(err, remote.ErrSignedOut) {
			t.Fatalf("%s signed out: %v", c, err)
		}
	}
	if len(reg.Requests) != 0 {
		t.Fatalf("a signed-out command reached the registry: %v", reg.Requests)
	}
}

func init() { voicePollEvery = 10 * time.Millisecond }
