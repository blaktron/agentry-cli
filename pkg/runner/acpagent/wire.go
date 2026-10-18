package acpagent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

// The bounds the agent side holds the connection to.
const (
	// MaxFrame bounds one inbound JSON-RPC line. A longer line is answered
	// with a parse error and read past, and the connection goes on.
	MaxFrame = 1 << 20
	// MaxBehind is how much progress the connection queues for a client that
	// reads slowly. Past it the progress notifications are dropped, so the run
	// never waits for a reader; the answers the client is owed are kept, up
	// to a further mustKeepHeadroom, past which the connection is given up.
	MaxBehind = 16 << 20

	mustKeepHeadroom = 8 << 20
	// maxIDLen bounds a request id the agent echoes.
	maxIDLen = 256
)

// flushGrace is how long the agent waits at its end for the client to take
// what is queued. A variable so a test can shorten it.
var flushGrace = 10 * time.Second

// JSON-RPC error codes the agent answers with.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// frame is any inbound JSON-RPC message.
type frame struct {
	JSONRPC string `json:"jsonrpc"`
	// ID is the id as sent: empty when the frame has none, "null" when it
	// says null.
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

// id is the frame's id, nil when it has none.
func (f frame) id() *json.RawMessage {
	if len(f.ID) == 0 {
		return nil
	}
	r := f.ID
	return &r
}

// validID says whether raw is an id the agent echoes: a number, or a string
// of valid UTF-8, of at most maxIDLen bytes. Null, an object, an array and a
// boolean are not ids a client of this agent sends.
func validID(raw json.RawMessage) bool {
	if len(raw) == 0 || len(raw) > maxIDLen {
		return false
	}
	switch c := raw[0]; {
	case c == '"':
		return utf8.Valid(raw)
	case c == '-' || (c >= '0' && c <= '9'):
		return true
	}
	return false
}

var errFrameTooLong = errors.New("frame too long")

// frameReader reads newline-delimited frames of at most max bytes. A longer
// frame is discarded to its end and reported as errFrameTooLong, so one huge
// line costs the reader no more than max bytes of memory.
type frameReader struct {
	r   *bufio.Reader
	max int
}

func newFrameReader(r io.Reader, max int) *frameReader {
	return &frameReader{r: bufio.NewReaderSize(r, 64<<10), max: max}
}

// next returns the next non-blank line without its newline.
func (f *frameReader) next() ([]byte, error) {
	for {
		var line []byte
		tooLong := false
		for {
			chunk, err := f.r.ReadSlice('\n')
			if !tooLong {
				if len(line)+len(chunk) > f.max+1 {
					tooLong = true
					line = nil
				} else {
					line = append(line, chunk...)
				}
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				if errors.Is(err, io.EOF) && (len(line) > 0 || tooLong) {
					// A last line the client did not end: it is a frame.
					break
				}
				return nil, err
			}
			break
		}
		if tooLong {
			return nil, errFrameTooLong
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		return line, nil
	}
}

// outbox writes frames to the client from one goroutine, in order, and is
// never waited on by the run: a client that stops reading fills the queue, the
// progress it holds is dropped, and the connection is given up when even the
// answers it is owed cannot be queued.
type outbox struct {
	w io.Writer
	// maxBehind and keepHeadroom are MaxBehind and mustKeepHeadroom; a test
	// sets smaller ones.
	maxBehind, keepHeadroom int

	mu      sync.Mutex
	wake    chan struct{}
	queue   [][]byte
	queued  int
	dropped int
	dead    bool
	closed  bool
	done    chan struct{}
}

func newOutbox(w io.Writer) *outbox {
	o := &outbox{w: w, maxBehind: MaxBehind, keepHeadroom: mustKeepHeadroom, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go o.loop()
	return o
}

// send queues one frame. A progress frame (droppable) is dropped when the
// client is MaxBehind; the rest are kept until the headroom is spent.
func (o *outbox) send(b []byte, droppable bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.dead || o.closed {
		return
	}
	limit := o.maxBehind
	if !droppable {
		limit += o.keepHeadroom
	}
	if o.queued+len(b) > limit {
		if droppable {
			o.dropped++
			return
		}
		o.dead = true
		o.queue, o.queued = nil, 0
		return
	}
	o.queue = append(o.queue, b)
	o.queued += len(b)
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (o *outbox) loop() {
	defer close(o.done)
	for {
		o.mu.Lock()
		for len(o.queue) == 0 && !o.closed && !o.dead {
			o.mu.Unlock()
			<-o.wake
			o.mu.Lock()
		}
		if len(o.queue) == 0 || o.dead {
			o.mu.Unlock()
			return
		}
		b := o.queue[0]
		o.queue = o.queue[1:]
		o.queued -= len(b)
		o.mu.Unlock()
		if _, err := o.w.Write(append(b, '\n')); err != nil {
			o.mu.Lock()
			o.dead = true
			o.queue, o.queued = nil, 0
			o.mu.Unlock()
			return
		}
	}
}

// close stops taking frames and waits up to grace for the queue to be
// written; a client that never reads costs the grace and nothing more.
func (o *outbox) close(grace time.Duration) {
	o.mu.Lock()
	o.closed = true
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
	select {
	case <-o.done:
	case <-time.After(grace):
	}
}
