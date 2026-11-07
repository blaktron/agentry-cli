package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// The person's screen. A run with computer use sees screenshots of the
// person's windows: each comes back from the desktop link as an image the
// harness passes to the model and then reports in its event stream. Those
// lines are kept as the run's events receipt and handed to --events, so in
// such a run every image in them is dropped first, its data replaced by a
// note of its size: nothing of the person's screen rests in the run's
// folder, as the receipt says. A run without computer use keeps its event
// lines as the harness sent them.

// minImageData is the shortest data an image item is taken to carry: a
// field this short is no picture of a screen.
const minImageData = 256

// dropImages is an event line with the data of every image item in it
// replaced. A line that is not JSON, or carries no image, comes back as it
// was.
func dropImages(line []byte) []byte {
	if !bytes.Contains(line, []byte(`"data"`)) {
		return line
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return line
	}
	v, changed := dropImageData(v, false)
	if !changed {
		return line
	}
	out, err := json.Marshal(v)
	if err != nil {
		return line
	}
	return out
}

// dropImageData walks a decoded line; inImages says v sits in a list named
// images.
func dropImageData(v any, inImages bool) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		changed := false
		if data, ok := t["data"].(string); ok && len(data) >= minImageData && (inImages || isImageItem(t)) {
			t["data"] = ""
			t["omitted"] = fmt.Sprintf("an image of %d base64 characters, not kept: the run keeps no screenshot", len(data))
			changed = true
		}
		for k, child := range t {
			if k == "data" {
				continue
			}
			nv, c := dropImageData(child, k == "images")
			if c {
				t[k], changed = nv, true
			}
		}
		return t, changed
	case []any:
		changed := false
		for i, child := range t {
			nv, c := dropImageData(child, inImages)
			if c {
				t[i], changed = nv, true
			}
		}
		return t, changed
	}
	return v, false
}

// isImageItem says a map is an image item: an MCP image content item, a
// base64 source block, or anything whose media type is an image.
func isImageItem(m map[string]any) bool {
	for _, k := range []string{"type", "mimeType", "mime_type", "media_type", "mediaType"} {
		s, _ := m[k].(string)
		if s == "image" || s == "base64" || strings.HasPrefix(s, "image/") {
			return true
		}
	}
	return false
}
