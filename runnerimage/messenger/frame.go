package messenger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// The link between the role and the agent's supervisor is JSON lines over
// the role's standard input and output. The supervisor writes a Start line
// first, which carries the account's credential, then replies, typing and
// pairing changes; the role writes its ready line, then the messages it
// accepted, the pairing codes unpaired senders sent, the messages it
// dropped, delivery reports and its connection's state. Each line holds
// exactly one of them, and a line that holds anything else, or a field
// neither side knows, is refused. Nothing on the link names a tool, a
// roster entry, a grant or the agent's lifecycle, so a message from the
// service can only ever become a turn's text.

// MaxLine bounds one line of the link either way.
const MaxLine = 256 << 10

// MaxInboundText bounds the text of one message the role passes on; a
// longer one is cut, and says so.
const MaxInboundText = 16 << 10

// Peer is the person's own account on the messenger, paired with the agent:
// the sender the role passes messages from, and the chat its replies go to.
type Peer struct {
	Sender string `json:"sender"`
	Chat   string `json:"chat"`
}

// Start is the supervisor's first line: the kind, the account's credential,
// the account's own server where the kind has one (a Matrix homeserver's
// https:// address), and the paired person, if any.
type Start struct {
	Kind   string `json:"kind"`
	Token  string `json:"token"`
	Server string `json:"server,omitempty"`
	Peer   *Peer  `json:"peer,omitempty"`
}

// Reply is an answer for the paired person's chat. ID names its Sent report.
type Reply struct {
	ID   uint64 `json:"id"`
	Chat string `json:"chat"`
	Text string `json:"text"`
}

// Typing shows the paired person's chat that the agent is answering.
type Typing struct {
	Chat string `json:"chat"`
}

// Paired names the person paired from now on, or none.
type Paired struct {
	Peer *Peer `json:"peer,omitempty"`
}

// ToRole is one line the supervisor writes.
type ToRole struct {
	Start  *Start  `json:"start,omitempty"`
	Reply  *Reply  `json:"reply,omitempty"`
	Typing *Typing `json:"typing,omitempty"`
	Paired *Paired `json:"paired,omitempty"`
}

// Ready is the role's first line, once the service took the credential:
// the account's own id and name there.
type Ready struct {
	Account string `json:"account"`
	Name    string `json:"name,omitempty"`
}

// Inbound is a message from the paired person.
type Inbound struct {
	// ID is the message's id in its chat, which grows message by message.
	ID     int64  `json:"id,omitempty"`
	Chat   string `json:"chat"`
	Sender string `json:"sender"`
	Text   string `json:"text"`
	// Cut says the text was longer than MaxInboundText.
	Cut bool `json:"cut,omitempty"`
}

// Pairing is a pairing code an unpaired sender sent in a direct message:
// the supervisor alone knows whether it is the code it showed.
type Pairing struct {
	Chat   string `json:"chat"`
	Sender string `json:"sender"`
	Code   string `json:"code"`
}

// The reasons a message is dropped.
const (
	DroppedUnpaired  = "unpaired"     // from a sender who is not paired
	DroppedNotDirect = "not-direct"   // in a group or a channel
	DroppedNoText    = "no-text"      // a sticker, a photo, a voice note
	DroppedCommand   = "command"      // the service's own command (/start)
	DroppedPairings  = "pairing-rate" // more pairing codes than the role passes on
)

// Dropped is a message the role did not pass on, and why: never who sent it
// or what it said.
type Dropped struct {
	Reason string `json:"reason"`
}

// Sent reports a Reply: how many messages it went as, whether it was cut at
// MaxParts, or why it was not sent.
type Sent struct {
	ID    uint64 `json:"id"`
	Parts int    `json:"parts,omitempty"`
	Cut   bool   `json:"cut,omitempty"`
	Error string `json:"error,omitempty"`
}

// State is the connection's state as it changes: connected, or why not.
type State struct {
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
}

// FromRole is one line the role writes.
type FromRole struct {
	Ready   *Ready   `json:"ready,omitempty"`
	Inbound *Inbound `json:"inbound,omitempty"`
	Pairing *Pairing `json:"pairing,omitempty"`
	Dropped *Dropped `json:"dropped,omitempty"`
	Sent    *Sent    `json:"sent,omitempty"`
	State   *State   `json:"state,omitempty"`
}

// errFrame refuses a line that is not exactly one frame.
var errFrame = errors.New("a line of the messenger link is not exactly one frame")

// ReadToRole decodes one line the supervisor wrote, strictly.
func ReadToRole(line []byte) (ToRole, error) {
	var f ToRole
	if err := strictDecode(line, &f); err != nil {
		return ToRole{}, err
	}
	if count(f.Start != nil, f.Reply != nil, f.Typing != nil, f.Paired != nil) != 1 {
		return ToRole{}, errFrame
	}
	return f, nil
}

// ReadFromRole decodes one line the role wrote, strictly.
func ReadFromRole(line []byte) (FromRole, error) {
	var f FromRole
	if err := strictDecode(line, &f); err != nil {
		return FromRole{}, err
	}
	if count(f.Ready != nil, f.Inbound != nil, f.Pairing != nil, f.Dropped != nil, f.Sent != nil, f.State != nil) != 1 {
		return FromRole{}, errFrame
	}
	return f, nil
}

func strictDecode(line []byte, v any) error {
	if len(line) > MaxLine {
		return fmt.Errorf("a line of the messenger link is longer than %d bytes", MaxLine)
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errFrame
	}
	return nil
}

func count(set ...bool) int {
	n := 0
	for _, s := range set {
		if s {
			n++
		}
	}
	return n
}

// Writer writes frames as lines, one at a time.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriter writes frames to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Write writes one frame as a line.
func (w *Writer) Write(frame any) error {
	b, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if len(b) >= MaxLine {
		return fmt.Errorf("a frame of %d bytes is longer than the messenger link carries", len(b))
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.w.Write(append(b, '\n'))
	return err
}
