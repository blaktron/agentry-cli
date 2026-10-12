package hostlink

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// The text view of a terminal (plan runtime-hosts.md D18): what a shell
// session printed, with the terminal's control sequences removed, as a
// person would read it on a line-by-line terminal. A carriage return that
// redraws a line (a progress bar, a spinner) leaves the line as it was last
// drawn, a backspace steps back over what it rubs out, and an erase to the
// end of the line takes effect; colours, cursor movement between lines,
// titles, modes and every other sequence are dropped. It is not a screen:
// a full-screen program reads as a stream of its text (a rendered screen
// is a follow-up, D18).

// vt renders a terminal's bytes as text, a write at a time: a sequence or a
// character a write splits is finished by the next.
type vt struct {
	line  []rune
	col   int
	saved int // the column a save-cursor sequence kept
	pend  []byte
}

// maxLineRunes bounds a line the renderer holds before it ends it: a
// program that redraws one line forever is not held whole.
const maxLineRunes = 64 << 10

// textView is b as text, in one go.
func textView(b []byte) string {
	var v vt
	var out strings.Builder
	v.write(b, &out)
	v.flush(&out)
	return out.String()
}

// write renders p, appending each line it ends to out.
func (v *vt) write(p []byte, out *strings.Builder) {
	b := p
	if len(v.pend) > 0 {
		b = append(v.pend, p...)
		v.pend = nil
	}
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == '\n':
			v.endLine(out)
			i++
		case c == '\r':
			v.col = 0
			i++
		case c == '\b':
			if v.col > 0 {
				v.col--
			}
			i++
		case c == '\t':
			v.put('\t')
			i++
		case c == 0x1b:
			n, complete := escapeLen(b[i:])
			if !complete {
				v.pend = append([]byte(nil), b[i:]...)
				return
			}
			v.escape(b[i : i+n])
			i += n
		case c < 0x20 || c == 0x7f:
			i++ // another control character: a bell, a shift
		default:
			if !utf8.FullRune(b[i:]) {
				v.pend = append([]byte(nil), b[i:]...)
				return
			}
			r, n := utf8.DecodeRune(b[i:])
			if r >= 0x80 && r <= 0x9f {
				r = -1 // a C1 control, dropped
			}
			if r >= 0 {
				v.put(r)
			}
			i += n
		}
	}
}

// flush ends what is left: a line not yet ended (a prompt) is written as
// it is, without a line end, and a sequence cut short is dropped.
func (v *vt) flush(out *strings.Builder) {
	if len(v.pend) > 0 {
		if v.pend[0] != 0x1b {
			out.WriteString(strings.ToValidUTF8(string(v.pend), "�"))
		}
		v.pend = nil
	}
	out.WriteString(string(v.line))
	v.line, v.col = v.line[:0], 0
}

func (v *vt) endLine(out *strings.Builder) {
	out.WriteString(strings.TrimRight(string(v.line), " "))
	out.WriteByte('\n')
	v.line, v.col, v.saved = v.line[:0], 0, 0
}

func (v *vt) put(r rune) {
	for len(v.line) < v.col {
		v.line = append(v.line, ' ')
	}
	if v.col < len(v.line) {
		v.line[v.col] = r
	} else {
		v.line = append(v.line, r)
	}
	v.col++
	if v.col >= maxLineRunes {
		v.line = v.line[:0]
		v.col = 0
	}
}

// escape applies the sequences that change the line's text, an erase
// within it, a move along it, or a save and a restore of the cursor (a
// progress bar's redraw: apk's, among others), and drops the rest.
func (v *vt) escape(seq []byte) {
	if len(seq) == 2 {
		switch seq[1] {
		case '7':
			v.saved = v.col
		case '8':
			v.col = min(v.saved, maxLineRunes-1)
		}
		return
	}
	if len(seq) < 3 || seq[1] != '[' {
		return
	}
	final := seq[len(seq)-1]
	params := string(seq[2 : len(seq)-1])
	n := 1
	if p, err := strconv.Atoi(params); err == nil && p > 0 {
		n = p
	}
	switch final {
	case 'K': // erase in line
		switch params {
		case "", "0":
			if v.col < len(v.line) {
				v.line = v.line[:v.col]
			}
		case "1":
			for i := 0; i < v.col && i < len(v.line); i++ {
				v.line[i] = ' '
			}
		case "2":
			v.line = v.line[:0]
		}
	case 'C': // forward
		v.col = min(v.col+n, maxLineRunes-1)
	case 'D': // back
		v.col = max(v.col-n, 0)
	case 'G': // to a column
		v.col = min(n-1, maxLineRunes-1)
	case 's':
		if params == "" {
			v.saved = v.col
		}
	case 'u':
		if params == "" {
			v.col = min(v.saved, maxLineRunes-1)
		}
	}
}

// escapeLen is the length of the escape sequence b begins with (b[0] is
// ESC), and whether b holds all of it: CSI (ESC [ … final), OSC (ESC ] …
// BEL or ST), the string sequences DCS, SOS, PM and APC (… ST), a charset
// designation (ESC ( x), or ESC and one more byte.
func escapeLen(b []byte) (int, bool) {
	if len(b) < 2 {
		return len(b), false
	}
	switch b[1] {
	case '[':
		for i := 2; i < len(b); i++ {
			if b[i] >= 0x40 && b[i] <= 0x7e {
				return i + 1, true
			}
			if i > 256 { // not a sequence any terminal sends: drop it
				return i, true
			}
		}
		return len(b), false
	case ']', 'P', 'X', '^', '_':
		for i := 2; i < len(b); i++ {
			if b[1] == ']' && b[i] == 0x07 {
				return i + 1, true
			}
			if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\' {
				return i + 2, true
			}
			if i > 4096 {
				return i, true
			}
		}
		return len(b), false
	case '(', ')', '*', '+', '-', '.', '/', '#', '%', ' ':
		if len(b) < 3 {
			return len(b), false
		}
		return 3, true
	}
	return 2, true
}

// unfinishedTail is how many bytes at b's end begin a sequence or a
// character that b does not finish, which a read keeps for the next.
func unfinishedTail(b []byte) int {
	from := max(len(b)-4096, 0)
	for i := len(b) - 1; i >= from; i-- {
		if b[i] == 0x1b {
			if _, complete := escapeLen(b[i:]); !complete {
				return len(b) - i
			}
			break
		}
	}
	for j := len(b) - 1; j >= 0 && j >= len(b)-utf8.UTFMax; j-- {
		if utf8.RuneStart(b[j]) {
			if !utf8.FullRune(b[j:]) {
				return len(b) - j
			}
			break
		}
	}
	return 0
}
