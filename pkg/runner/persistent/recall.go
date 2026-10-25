package persistent

// Recall: the Assistant's search of its own earlier conversations. It reads
// the agent's transcript on the host, which the engine wrote already
// redacted, and answers with the turns that match: the person's, the
// agent's and its tasks', never a frame Agentry sent, an upkeep turn, a
// notice or a tool's result. A stored secret's value is masked again, in
// case it was stored after a turn that said it. It changes nothing.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
)

// The bounds of a recall's answer.
const (
	recallDefaultLimit = 10
	recallExcerpt      = 600
	recallMaxAnswer    = 8 << 10
	// recallMaxLine bounds a transcript line recall reads; a longer one is
	// skipped rather than read whole.
	recallMaxLine = 4 << 20
)

// recallArgs are recall's arguments.
type recallArgs struct {
	Query string `json:"query"`
	From  string `json:"from"`
	To    string `json:"to"`
	Limit int    `json:"limit"`
}

// recallHit is one matching turn.
type recallHit struct {
	at      time.Time
	who     string
	segment int
	turn    int
	text    string
}

// answerRecall answers a recall call.
func (s *Supervisor) answerRecall(raw json.RawMessage) applink.Answer {
	var args recallArgs
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&args) != nil || strings.TrimSpace(args.Query) == "" {
		return applink.Answer{Text: "recall takes a query, and a from date, a to date and a limit if you want them", IsError: true}
	}
	if args.Limit == 0 {
		args.Limit = recallDefaultLimit
	}
	if args.Limit < 0 || args.Limit > applink.RecallMaxLimit {
		return applink.Answer{Text: fmt.Sprintf("recall's limit is 1 to %d", applink.RecallMaxLimit), IsError: true}
	}
	from, to, err := recallSpan(args.From, args.To, time.Local)
	if err != nil {
		return applink.Answer{Text: err.Error(), IsError: true}
	}
	hits, err := recallTranscript(filepath.Join(Dir(s.opts.Home, s.opts.Agent.Name), transcriptFile), args.Query, from, to)
	if err != nil {
		return applink.Answer{Text: "the earlier conversations could not be read: " + err.Error(), IsError: true}
	}
	return applink.Answer{Text: recallText(hits, args.Query, args.Limit, s.storedSecretValues())}
}

// recallSpan is the times a from and a to date, YYYY-MM-DD in loc, bound: from
// the start of the first day to the end of the last. Either may be empty.
func recallSpan(fromDate, toDate string, loc *time.Location) (time.Time, time.Time, error) {
	var from, to time.Time
	if fromDate != "" {
		d, err := time.ParseInLocation(time.DateOnly, fromDate, loc)
		if err != nil {
			return from, to, fmt.Errorf("recall's from is a date, YYYY-MM-DD, not %q", fromDate)
		}
		from = d
	}
	if toDate != "" {
		d, err := time.ParseInLocation(time.DateOnly, toDate, loc)
		if err != nil {
			return from, to, fmt.Errorf("recall's to is a date, YYYY-MM-DD, not %q", toDate)
		}
		to = d.AddDate(0, 0, 1)
	}
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		return from, to, errors.New("recall's from date is after its to date")
	}
	return from, to, nil
}

// recallTranscript reads the transcript at path and returns, oldest first,
// the person's, the agent's and its tasks' turns in [from, to) that hold
// every word of query, in any case. An upkeep turn and the answer to it are
// Agentry's, and a synthetic turn is a notice: neither is returned.
func recallTranscript(path, query string, from, to time.Time) ([]recallHit, error) {
	f, err := os.Open(path) //nolint:gosec // the runner home's own file
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	words := strings.Fields(strings.ToLower(query))
	var hits []recallHit
	turns := map[int]int{}
	// who answers the user turn before it: "" after an upkeep turn, whose
	// answer is the handoff, not part of the conversation.
	answering := map[int]string{}
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := readLine(r, recallMaxLine)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		var e Entry
		if line == nil || json.Unmarshal(line, &e) != nil {
			continue
		}
		who := ""
		switch e.Kind {
		case session.KindUserTurn:
			turns[e.Segment]++
			switch {
			case e.Upkeep, e.Synthetic:
				answering[e.Segment] = ""
				continue
			case e.Task != "":
				who, answering[e.Segment] = "task "+e.Task, "you (to the task)"
			default:
				who, answering[e.Segment] = "the person", "you"
			}
		case session.KindAssistantTurn:
			if who = answering[e.Segment]; who == "" {
				continue
			}
		default:
			continue
		}
		if (!from.IsZero() && e.Time.Before(from)) || (!to.IsZero() && !e.Time.Before(to)) || !holdsAll(e.Text, words) {
			continue
		}
		hits = append(hits, recallHit{at: e.Time, who: who, segment: e.Segment, turn: turns[e.Segment], text: e.Text})
	}
	return hits, nil
}

// readLine reads one line of at most limit bytes, without its newline; a
// longer line is read past and returned as nil.
func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	over := false
	for {
		part, err := r.ReadSlice('\n')
		if !over {
			if len(line)+len(part) > limit {
				over, line = true, nil
			} else {
				line = append(line, part...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		switch {
		case err != nil && len(part) == 0 && len(line) == 0 && !over:
			return nil, err
		case over:
			return nil, nil
		}
		return bytes.TrimRight(line, "\r\n"), nil
	}
}

// holdsAll says text holds every word, in any case.
func holdsAll(text string, words []string) bool {
	low := strings.ToLower(text)
	for _, w := range words {
		if !strings.Contains(low, w) {
			return false
		}
	}
	return true
}

// recallText is recall's answer: the newest hits first, at most limit, each
// an excerpt around the query's first word, the whole within its bound,
// with every stored secret's value masked.
func recallText(hits []recallHit, query string, limit int, secrets []string) string {
	if len(hits) == 0 {
		return "Nothing in your earlier conversations matches " + quote(query) + "."
	}
	slices.Reverse(hits)
	words := strings.Fields(strings.ToLower(query))
	var b strings.Builder
	fmt.Fprintf(&b, "%d turn(s) of your earlier conversations match %s, newest first (from the transcript, not your memory):\n", len(hits), quote(query))
	shown := 0
	for _, h := range hits {
		if shown == limit {
			break
		}
		text := maskValues(excerpt(h.text, words[0], recallExcerpt), secrets)
		entry := fmt.Sprintf("\n[%s, %s, segment %d turn %d]\n%s\n", h.at.Local().Format("2006-01-02 15:04"), h.who, h.segment, h.turn, text)
		if b.Len()+len(entry) > recallMaxAnswer {
			break
		}
		b.WriteString(entry)
		shown++
	}
	if shown < len(hits) {
		fmt.Fprintf(&b, "\n(%d more not shown: narrow the query or the dates.)\n", len(hits)-shown)
	}
	return b.String()
}

// excerpt is at most n bytes of text around the first place word is, on
// rune boundaries, marked where it was cut.
func excerpt(text, word string, n int) string {
	text = strings.TrimSpace(text)
	if len(text) <= n {
		return text
	}
	at := max(0, strings.Index(strings.ToLower(text), word))
	end := min(len(text), max(0, at-n/3)+n)
	start := max(0, end-n)
	out := strings.ToValidUTF8(text[start:end], "")
	if start > 0 {
		out = "…" + out
	}
	if end < len(text) {
		out += "…"
	}
	return out
}

// maskValues replaces every value in text with "[redacted]".
func maskValues(text string, values []string) string {
	for _, v := range values {
		if len(v) >= MinSecretBytes {
			text = strings.ReplaceAll(text, v, "[redacted]")
		}
	}
	return text
}

func quote(s string) string { return "\"" + strings.ReplaceAll(s, "\"", "'") + "\"" }

// storedSecretValues are the agent's stored secrets' values, as the keychain
// holds them now; none when it cannot be read.
func (s *Supervisor) storedSecretValues() []string {
	cfg, err := LoadConfig(s.opts.Home, s.opts.Agent.Name)
	if err != nil || len(cfg.Secrets) == 0 {
		return nil
	}
	keys := s.opts.Secrets
	if keys == nil {
		keys = KeychainSecrets{}
	}
	names := make([]string, 0, len(cfg.Secrets))
	for _, sec := range cfg.Secrets {
		names = append(names, sec.Name)
	}
	values, _ := ReadSecrets(keys, s.opts.Agent.Name, names)
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, v)
	}
	return out
}
