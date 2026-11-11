package harness

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// cutNotice ends a result that was longer than the limit. It gives the two
// sizes and says how to read the rest.
const cutNotice = "\n[Note from the agent runtime: this result was %d bytes long and ends here, after its first %d. " +
	"To read the rest, ask the tool for a smaller part: a narrower query, a range, or the next page.]"

// cutResult holds text to limit bytes: a longer text keeps its start, cut
// after a whole line where one ends in its last quarter and at a character
// otherwise, and ends with cutNotice.
func cutResult(text string, limit int) (string, bool) {
	if limit <= 0 || len(text) <= limit {
		return text, false
	}
	// The notice's own length, with both figures at their widest.
	room := limit - len(fmt.Sprintf(cutNotice, len(text), len(text)))
	if room < 0 {
		room = 0
	}
	keep := room
	for keep > 0 && !utf8.RuneStart(text[keep]) {
		keep--
	}
	if line := strings.LastIndexByte(text[:keep], '\n'); line >= room-room/4 {
		keep = line
	}
	return text[:keep] + fmt.Sprintf(cutNotice, len(text), keep), true
}
