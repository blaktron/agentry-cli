package runhost

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// The service's wire format, end to end from the service's socket, through
// serve and the SSH session, to the device: the device's request is one JSON
// line, and the service answers in JSON lines, frames, the last of which is
// always an exit frame.

// Request is what a device asks of the service.
type Request struct {
	Verb string   `json:"verb"`
	Args []string `json:"args,omitempty"`
}

// Frame types.
const (
	// FrameOut is a line of narration for the person.
	FrameOut = "out"
	// FrameResult carries the verb's answer, as JSON.
	FrameResult = "result"
	// FrameExit ends the answer, with an exit code and, when it is not 0,
	// the reason.
	FrameExit = "exit"
	// FrameRelease answers a job's document: Text is ReleaseNeeded when the
	// release's archive must follow it, ReleaseHeld when the store holds it.
	FrameRelease = "release"
	// FrameData carries bytes of a sealed output, in order.
	FrameData = "data"
	// FrameStarted says the job runs under the service: Text is its id. A
	// device that leaves after it finds the job again with attach.
	FrameStarted = "started"
	// FrameEvent carries one event of a session, as the session protocol
	// wrote it, in Result.
	FrameEvent = "event"
)

// The two answers of a release frame.
const (
	ReleaseNeeded = "needed"
	ReleaseHeld   = "held"
)

// DataChunk is the most bytes one data frame carries.
const DataChunk = 256 << 10

// Frame is one line of the service's answer.
type Frame struct {
	Type   string          `json:"type"`
	Text   string          `json:"text,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Code   int             `json:"code,omitempty"`
	Error  string          `json:"error,omitempty"`
	Data   []byte          `json:"data,omitempty"`
}

// MaxLine bounds one line of the wire format.
const MaxLine = 4 << 20

// FrameWriter writes frames; it is safe for concurrent use.
type FrameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewFrameWriter writes frames to w.
func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

func (f *FrameWriter) write(fr Frame) error {
	b, err := json.Marshal(fr)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err = f.w.Write(append(b, '\n'))
	return err
}

// Out writes a line of narration.
func (f *FrameWriter) Out(format string, args ...any) error {
	return f.write(Frame{Type: FrameOut, Text: fmt.Sprintf(format, args...)})
}

// Result writes the verb's answer.
func (f *FrameWriter) Result(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return f.write(Frame{Type: FrameResult, Result: b})
}

// Frame writes f as it is.
func (f *FrameWriter) Frame(fr Frame) error { return f.write(fr) }

// Started says job id runs under the service.
func (f *FrameWriter) Started(id string) error {
	return f.write(Frame{Type: FrameStarted, Text: id})
}

// Release says whether a job's release must follow its document.
func (f *FrameWriter) Release(answer string) error {
	return f.write(Frame{Type: FrameRelease, Text: answer})
}

// Data writes r to its end as data frames.
func (f *FrameWriter) Data(r io.Reader) error {
	buf := make([]byte, DataChunk)
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if werr := f.write(Frame{Type: FrameData, Data: buf[:n]}); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Exit ends the answer.
func (f *FrameWriter) Exit(code int, err error) error {
	fr := Frame{Type: FrameExit, Code: code}
	if err != nil {
		fr.Error = err.Error()
		if code == 0 {
			fr.Code = 1
		}
	}
	return f.write(fr)
}

// WriteRequest writes a request line.
func WriteRequest(w io.Writer, r Request) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ReadRequest reads a request line.
func ReadRequest(r *bufio.Reader) (Request, error) {
	line, err := readLine(r)
	if err != nil {
		return Request{}, err
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return Request{}, fmt.Errorf("the request is not readable: %w", err)
	}
	return req, nil
}

// ErrNoExit means the answer ended without its exit frame: the connection
// dropped, or the service stopped mid-answer.
var ErrNoExit = errors.New("the runhost's answer ended before it finished")

// Answer is a whole answer, read.
type Answer struct {
	Out    []string
	Result json.RawMessage
	Code   int
	Error  string
}

// ReadAnswer reads frames until the exit frame, handing each narration line
// to out (which may be nil).
func ReadAnswer(r io.Reader, out func(string)) (Answer, error) {
	return ReadFrames(r, func(f Frame) error {
		if f.Type == FrameOut && out != nil {
			out(f.Text)
		}
		return nil
	})
}

// ReadFrames reads frames until the exit frame, handing every frame to each
// (an error from each stops the read and is returned).
func ReadFrames(r io.Reader, each func(Frame) error) (Answer, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var a Answer
	for {
		line, err := readLine(br)
		if errors.Is(err, io.EOF) {
			return a, ErrNoExit
		}
		if err != nil {
			return a, err
		}
		var f Frame
		if err := json.Unmarshal(line, &f); err != nil {
			return a, fmt.Errorf("the runhost answered something that is not a frame: %w", err)
		}
		if err := each(f); err != nil {
			return a, err
		}
		switch f.Type {
		case FrameOut:
			a.Out = append(a.Out, f.Text)
		case FrameResult:
			a.Result = f.Result
		case FrameExit:
			a.Code, a.Error = f.Code, f.Error
			return a, nil
		}
	}
}

func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			if len(line) > 0 && errors.Is(err, io.EOF) {
				return line, nil
			}
			return nil, err
		}
		line = append(line, chunk...)
		if len(line) > MaxLine {
			return nil, fmt.Errorf("a line longer than %d bytes", MaxLine)
		}
		if !isPrefix {
			return line, nil
		}
	}
}
