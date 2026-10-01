// A Writing Voice attached to a run (plan writing-voice.md §5, D4;
// agentry-cli#531). It rides on knowledge's path (DR-38): the compiled plan
// already mounts one read-only /knowledge volume on the tools container,
// so the engine writes the voice's VOICE.md under it, at .voice/ — a name
// no pack can take, since a pack's name starts with a letter or a digit —
// and grants voice_read in the run's own copies of the tools spec and the
// gateway's allowlist. Attaching a voice moves neither the plan nor its
// hash. The run's copy dies with the knowledge directory at teardown.
package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/voice"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// VoiceDir is the voice's directory under the knowledge mount.
const VoiceDir = ".voice"

// VoiceFile is the voice's path in the tools container.
var VoiceFile = path.Join(spec.KnowledgeMount, VoiceDir, voice.File)

// AttachedVoice is a Writing Voice attached to a run: its text, held in
// memory by the caller (it is at most voice.MaxBytes), the digest the
// caller verified, and how the receipt names where it came from.
type AttachedVoice struct {
	Text   []byte
	Digest string
	// Origin is the receipt's account of the voice: "kept on this
	// computer, made by the Voice Writer, not reviewed", or the account's
	// live voice as a hosted run's driver names it.
	Origin string
}

// AttachVoice is the attachment of a voice's text, checked against the
// format and digested here, so a caller cannot name a digest the text does
// not have.
func AttachVoice(text []byte, origin string) (*AttachedVoice, error) {
	if err := voice.Check(text); err != nil {
		return nil, err
	}
	return &AttachedVoice{Text: bytes.Clone(text), Digest: voice.Digest(text), Origin: origin}, nil
}

// checkVoice refuses a voice on a built-in's run (plan writing-voice.md
// D5): the Drafter, the Knowledge Creator and the Voice Writer write for
// the application, never in a person's voice.
func checkVoice(p Params) error {
	if p.Voice != nil && p.Plan.Source == "builtin" {
		return Classify(ExitCLI, errors.New("a built-in package's run never takes a Writing Voice"))
	}
	return nil
}

// stageVoice writes the voice into the run's knowledge directory and
// checks what it wrote, as stageKnowledge checks a pack's copy.
func stageVoice(v *AttachedVoice, knowledgeDir string) error {
	if v == nil {
		return nil
	}
	if err := voice.Check(v.Text); err != nil {
		return Classify(ExitCLI, fmt.Errorf("the Writing Voice: %w", err))
	}
	if got := voice.Digest(v.Text); got != v.Digest {
		return Classify(ExitVerify, fmt.Errorf("the Writing Voice changed after it was verified (verified %s, now %s)", v.Digest, got))
	}
	dir := filepath.Join(knowledgeDir, VoiceDir)
	if err := os.Mkdir(dir, 0o755); err != nil { //nolint:gosec // read by the tools container's user
		return Classify(ExitCLI, fmt.Errorf("stage the Writing Voice: %w", err))
	}
	file := filepath.Join(dir, voice.File)
	if err := os.WriteFile(file, v.Text, 0o644); err != nil { //nolint:gosec // read by the tools container's user
		return Classify(ExitCLI, fmt.Errorf("stage the Writing Voice: %w", err))
	}
	staged, err := os.ReadFile(file) //nolint:gosec // the run's own copy
	if err != nil || voice.Digest(staged) != v.Digest {
		return Classify(ExitVerify, fmt.Errorf("the Writing Voice changed as it was staged: %v", err))
	}
	return nil
}

// grantVoice stamps the voice into the run's rendered tools spec and
// rebuilds the tools server's allowlist line from the spec's own
// GrantedTools, after grantKnowledge, and holds the rendered catalog to the
// mount voice_read reads, as grantKnowledge does.
func grantVoice(gatewayDir, knowledgeDir string, v *AttachedVoice) error {
	if v == nil {
		return nil
	}
	catalog, err := os.ReadFile(filepath.Join(gatewayDir, "catalog.yaml")) //nolint:gosec // our own rendered config
	if err != nil {
		return err
	}
	if !bytes.Contains(catalog, []byte(knowledgeDir+":"+spec.KnowledgeMount+":ro")) {
		return fmt.Errorf("this run's plan has no read-only %s mount for the tools container; remove the plan directory to recompile it with this CLI", spec.KnowledgeMount)
	}
	specPath := filepath.Join(gatewayDir, "toolspec.json")
	b, err := os.ReadFile(specPath) //nolint:gosec // our own rendered config
	if err != nil {
		return err
	}
	var ts toolsrv.Spec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ts); err != nil {
		return fmt.Errorf("the rendered tools spec does not decode as the tools server reads it: %w", err)
	}
	ts.Voice = &toolsrv.VoiceFile{File: VoiceFile, Digest: v.Digest}
	out, err := json.MarshalIndent(&ts, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(specPath, append(out, '\n'), 0o644); err != nil { //nolint:gosec // read by the tools container
		return err
	}
	return setAllowlist(filepath.Join(gatewayDir, "tools.yaml"), ts.GrantedTools())
}

// withVoice adds the receipt's voice line when a voice is attached: where
// it is mounted, its digest and size, where it came from, and what reaches
// it. A run with none carries no line.
func withVoice(enforced map[string]string, v *AttachedVoice, execCommands []string) map[string]string {
	if v == nil {
		return enforced
	}
	out := make(map[string]string, len(enforced)+1)
	for k, val := range enforced {
		out[k] = val
	}
	line := fmt.Sprintf("Writing Voice mounted read-only at %s (%s, %d bytes; %s); tool %s, on the tools container only",
		VoiceFile, v.Digest, len(v.Text), v.Origin, toolsrv.ToolVoiceRead)
	if len(execCommands) > 0 {
		line += "; the declared exec commands run in that container and can read it too, as they can " + spec.InputsMount
	}
	out["voice"] = line
	return out
}
