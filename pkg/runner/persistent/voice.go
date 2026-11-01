package persistent

import (
	"context"
	"errors"
	"fmt"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// KindVoice is the person's Writing Voice, given to a published agent.
const KindVoice EntryKind = "voice"

// EntryVoice is the Writing Voice's roster id.
const EntryVoice = "voice"

// voiceLine is the Writing Voice's consent sentence.
const voiceLine = "Writes in your Writing Voice: reads the voice you keep in Agentry through " + toolsrv.ToolVoiceRead + ", read-only, at each start"

// Voice opens the person's Writing Voice for a segment: the one they keep,
// as a run attaches it. It refuses when they keep none.
type Voice func(ctx context.Context) (*engine.AttachedVoice, error)

// voiceEntry is a published agent's Writing Voice entry, off until the
// person switches it on. A built-in has none: it writes for the
// application, never in a person's voice.
func voiceEntry(a Agent) (RosterEntry, bool) {
	if !a.Published() {
		return RosterEntry{}, false
	}
	return RosterEntry{ID: EntryVoice, Kind: KindVoice, Name: "Your Writing Voice", GrantedBy: GrantedByYou, Line: voiceLine}, true
}

// openVoice is the segment's Writing Voice: the person's, opened by open,
// when the roster has it on; nil when it is off.
func openVoice(ctx context.Context, a Agent, on func(id string) bool, open Voice) (*engine.AttachedVoice, error) {
	if !a.Published() || !on(EntryVoice) {
		return nil, nil
	}
	if open == nil {
		return nil, errors.New("the agent writes in your Writing Voice, and this client cannot open it: switch it off, or start the agent from the CLI")
	}
	v, err := open(ctx)
	if err != nil {
		return nil, fmt.Errorf("your Writing Voice: %w (switch it off with agentry agent grant %s %s off)", err, a.Name, EntryVoice)
	}
	return v, nil
}
