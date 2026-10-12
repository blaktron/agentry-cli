package toolsrv

// The result budget (plan input-sources.md §4, D1; agentry-cli#831). The
// harness that receives a tool's result bounds it too, and its bound is the
// smaller one: docker-agent (1.128.0, and 1.150.0 since agentry-cli#836)
// replaces any MCP result over 50 KiB, or over 2,000 lines, with its last
// 50 KiB, and the other harnesses have limits of their own. A result the
// harness cuts loses its head, and with it what the agent needed first,
// without the agent being told where. So every result this server returns
// fits the budget the compiler stamps for the run's harness
// (Limits.MaxResultBytes, MaxResultLines): a file is read
// a page at a time from the offset asked, each page saying where the next
// begins; a listing or a search stops at the budget and says what it left
// out; and fitResult holds every other result, an error's included, to it.

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The default budget, docker-agent's (the default harness): its hook's
// 50 KiB and 2,000 lines, less a margin for the notice a page ends with and
// for what the harness adds around the text.
const (
	DefaultMaxResultBytes = 48 << 10
	DefaultMaxResultLines = 1900
)

// pageNoteRoom is what a page keeps free of its budget for the lines this
// server adds around the text it pages: the page notice, a binary page's
// header, a section's heading.
const (
	pageNoteRoom  = 512
	pageNoteLines = 4
)

// budget is the most one tool result may hold, in bytes and in lines.
type budget struct {
	bytes int64
	lines int
}

func (l Limits) budget() budget {
	b := budget{bytes: DefaultMaxResultBytes, lines: DefaultMaxResultLines}
	if l.MaxResultBytes > 0 {
		b.bytes = l.MaxResultBytes
	}
	if l.MaxResultLines > 0 {
		b.lines = l.MaxResultLines
	}
	return b
}

// room is the part of the budget a page's text may take, the notice's room
// set aside.
func (b budget) room() budget {
	return budget{bytes: max(b.bytes-pageNoteRoom, 1), lines: max(b.lines-pageNoteLines, 1)}
}

// less is the budget left beside text that goes in the same result.
func (b budget) less(text string) budget {
	return budget{bytes: max(b.bytes-int64(len(text)), 1), lines: max(b.lines-lineCount(text), 1)}
}

// String says the budget as the notices say it.
func (b budget) String() string { return BudgetWords(b.bytes, b.lines) }

// BudgetWords says a result budget as the System Prompt and the notices
// say it: "48 KiB and 1,900 lines".
func BudgetWords(bytes int64, lines int) string {
	size := fmt.Sprintf("%d bytes", bytes)
	if bytes%1024 == 0 {
		size = fmt.Sprintf("%d KiB", bytes>>10)
	}
	n := strconv.Itoa(lines)
	for i := len(n) - 3; i > 0; i -= 3 {
		n = n[:i] + "," + n[i:]
	}
	return size + " and " + n + " lines"
}

// lineCount counts lines as docker-agent's hook does: every newline, and a
// last line without one.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// fits says whether text is within the budget.
func (b budget) fits(text string) bool {
	return int64(len(text)) <= b.bytes && lineCount(text) <= b.lines
}

// cut is the longest head of data within the budget, ending after a line
// end where one falls in the cut's latter half, else on a rune boundary.
// When more is false, data is the whole of what is cut and is returned whole
// if it fits; when more is true, more follows it, so even a data that fits
// is cut back to such a boundary.
func (b budget) cut(data []byte, more bool) []byte {
	if !more && int64(len(data)) <= b.bytes && lineCount(string(data)) <= b.lines {
		return data
	}
	if int64(len(data)) > b.bytes {
		data = data[:b.bytes]
	}
	// At most b.lines newlines: a cut after the last one kept leaves a head
	// of b.lines lines.
	seen := 0
	for i, c := range data {
		if c == '\n' {
			if seen++; seen == b.lines {
				return data[:i+1]
			}
		}
	}
	if nl := lastNewline(data); nl >= len(data)/2 {
		return data[:nl+1]
	}
	return trimToRuneEnd(data)
}

func lastNewline(data []byte) int {
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] == '\n' {
			return i
		}
	}
	return -1
}

// trimToRuneEnd drops the bytes of a rune the cut split, so that a text page
// stays UTF-8. Bytes that were not UTF-8 before the cut are left as they are:
// the caller tells a binary file by them.
func trimToRuneEnd(data []byte) []byte {
	end := len(data)
	for i := end - 1; i >= 0 && i > end-utf8.UTFMax; i-- {
		if !utf8.RuneStart(data[i]) {
			continue
		}
		if !utf8.FullRune(data[i:end]) {
			return data[:i]
		}
		break
	}
	return data
}

// pageNote is the line a page of a file ends with: the bytes it holds, the
// file's size, and how to read on (contFmt names the call, a format whose
// %d is the next offset, "offset %d" by default) or that the file ends here.
func pageNote(from, to, total int64, contFmt string) string {
	if to >= total {
		return fmt.Sprintf("\n[page: bytes %d–%d of %d; end of file]", from, to, total)
	}
	if contFmt == "" {
		contFmt = "offset %d"
	}
	return fmt.Sprintf("\n[page: bytes %d–%d of %d; continue with "+contFmt+"]", from, to, total, to)
}

// readPage reads one page of the file r, total bytes long, from offset: at
// most limit bytes (the caller's own cap) and within the budget's room, cut
// as budget.cut cuts. The page is empty at or past the end.
func (b budget) readPage(r io.ReadSeeker, offset, limit, total int64) ([]byte, error) {
	room := b.room()
	if limit > 0 && limit < room.bytes {
		room.bytes = limit
	}
	if offset >= total {
		return nil, nil
	}
	if offset > 0 {
		if _, err := r.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
	}
	data, err := io.ReadAll(io.LimitReader(r, room.bytes))
	if err != nil {
		return nil, err
	}
	return room.cut(data, offset+int64(len(data)) < total), nil
}

// textPage is the answer of a file's read tool for one page: the page's
// text, followed, when the file is not read whole, by its page notice. A
// page that is not UTF-8 is a binary file's, answered by its size as an
// error. It never exceeds the budget.
func textPage(data []byte, offset, total int64, contFmt string) (string, bool) {
	if !utf8.Valid(data) {
		return fmt.Sprintf("binary file; %d bytes", total), true
	}
	if len(data) == 0 && offset > 0 {
		return fmt.Sprintf("[offset %d is at or past the end of the file, which is %d bytes]", offset, total), false
	}
	text := string(data)
	end := offset + int64(len(data))
	if offset > 0 || end < total {
		text += pageNote(offset, end, total, contFmt)
	}
	return text, false
}

// listPage is a listing within the budget: header (when set) and then the
// lines from offset that fit, followed, when it is not the whole listing, by
// a notice saying which entries it holds and how to read on (narrow says how
// to narrow it instead, "pass path to list one directory").
func (b budget) listPage(header string, lines []string, offset int, narrow string) string {
	room := b.room()
	var out strings.Builder
	used := 0
	if header != "" {
		out.WriteString(header)
		used = 1
	}
	if offset >= len(lines) && offset > 0 {
		return fmt.Sprintf("(offset %d is past the last of the %d entries)", offset, len(lines))
	}
	i := offset
	for ; i < len(lines); i++ {
		add := len(lines[i])
		if out.Len() > 0 {
			add++
		}
		if int64(out.Len()+add) > room.bytes || used+1 > room.lines {
			break
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(lines[i])
		used++
	}
	if offset == 0 && i == len(lines) {
		return out.String()
	}
	note := fmt.Sprintf("[entries %d–%d of %d", offset+1, i, len(lines))
	if i < len(lines) {
		note += fmt.Sprintf("; continue with offset %d", i)
		if narrow != "" {
			note += ", or " + narrow
		}
	}
	note += "]"
	if out.Len() > 0 {
		out.WriteByte('\n')
	}
	out.WriteString(note)
	return out.String()
}

// fitResult holds any tool's result to the budget: a result over it is cut
// as budget.cut cuts, with a notice naming what was cut and why. The tools
// that page do so within the budget themselves, so for them this never
// cuts; it holds every other result, and every error, to the same bound.
func (b budget) fitResult(tool, text string) string {
	if b.fits(text) {
		return text
	}
	head := string(b.room().cut([]byte(text), true))
	how := "narrow the request"
	if tool == ToolRunCommand {
		how = "narrow the command's output, or have the command write it to a file in the scratch folder and page through that with scratch_read"
	}
	return head + fmt.Sprintf("\n[result cut at byte %d of %d (%d of %d lines) to fit the harness's tool-result limit of %s; %s]",
		len(head), len(text), lineCount(head), lineCount(text), b, how)
}
