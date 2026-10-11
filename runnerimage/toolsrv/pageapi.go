package toolsrv

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"unicode/utf8"
)

// The paging budget for another of the runner's MCP servers, the host link
// (runnerimage/hostlink, agentry-cli#861): its results are held to the same
// harness limit as this server's (plan runtime-hosts.md §5; plan
// input-sources.md D1), and they page the same way, so the one set of rules
// is exported here rather than copied.

// PageText is one page of the text data from offset, within the budget l
// stamps (and within limit, when it is positive), followed, when data is
// not read whole, by the page notice saying where to continue (contFmt as
// pageNote takes it). ok is false when the page is not UTF-8, so the
// caller can offer the bytes as base64 instead.
func (l Limits) PageText(data []byte, offset, limit int64, contFmt string) (text string, ok bool) {
	total := int64(len(data))
	page, err := l.budget().readPage(bytes.NewReader(data), offset, limit, total)
	if err != nil {
		return err.Error(), false
	}
	if !utf8.Valid(page) {
		return "", false
	}
	if len(page) == 0 && offset > 0 {
		return fmt.Sprintf("[offset %d is at or past the end, which is %d bytes]", offset, total), true
	}
	out := string(page)
	if end := offset + int64(len(page)); offset > 0 || end < total {
		out += pageNote(offset, end, total, contFmt)
	}
	return out, true
}

// PageBase64 is one page of data from offset as base64, within the budget
// (and limit, when positive): a header naming the bytes the page holds, the
// encoded page, and the page notice when the data is not read whole.
func (l Limits) PageBase64(data []byte, offset, limit int64, contFmt string) string {
	total := int64(len(data))
	if offset >= total && offset > 0 {
		return fmt.Sprintf("[offset %d is at or past the end, which is %d bytes]", offset, total)
	}
	room := l.budget().room()
	raw := room.bytes / 4 * 3
	if limit > 0 && limit < raw {
		raw = limit
	}
	end := min(offset+max(raw, 3), total)
	enc := base64.StdEncoding.EncodeToString(data[offset:end])
	head := fmt.Sprintf("[%d bytes, base64; this page holds bytes %d–%d]\n", total, offset, end)
	note := ""
	if offset > 0 || end < total {
		note = pageNote(offset, end, total, contFmt)
	}
	return head + enc + note
}

// FitResult holds a result to the budget, as every result of this server
// is held (fitResult).
func (l Limits) FitResult(tool, text string) string {
	return l.budget().fitResult(tool, text)
}

// Room is the bytes a page's text may take under the budget, the notice's
// room set aside.
func (l Limits) Room() int64 { return l.budget().room().bytes }
