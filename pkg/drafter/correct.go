package drafter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/manifest"
)

// Correct is the post-step's first step: the Drafter's mechanical slips
// corrected without the model, before the draft is checked. Each correction
// is a data edit of the manifest the draft already holds, never a value
// invented for it:
//
//   - a value named twice where the rules take it once (a mode, a tag, an
//     intake question's choice): the second dropped;
//   - an optional part left empty, or holding a placeholder such as "None"
//     or "n/a": removed;
//   - a list past its bound (the tags, the intake questions, the guide's
//     prepare items): cut to the bound;
//   - a text past its bound (the summary, a guide part, a key's help, the
//     interactive opening, the knowledge prompt): shortened at a word,
//     ending with "…";
//   - a knowledge block under capabilities: moved to the top level, where
//     the manifest takes it; knowledge.ships: removed, since a draft never
//     ships a pack.
//
// The bounds are pkg/manifest's own. It returns what it corrected, one plain
// sentence each, which Finish names in the draft's notes; the rules then
// check the corrected manifest as they would any other. A manifest that is
// not a JSON object is left as it came, for the rules to refuse.
func Correct(raw json.RawMessage) (json.RawMessage, []string) {
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return raw, nil
	}
	c := &corrector{}
	root := doc.Content[0]
	c.dedupe(root, "modes")
	c.dedupe(root, "tags")
	c.cut(root, "tags", "tags", manifest.MaxTags, "tags")
	c.shortenAt(root, "summary", "summary", manifest.MaxSummaryChars, false)
	if intake := value(root, "intake"); intake != nil && intake.Kind == yaml.SequenceNode {
		c.cut(root, "intake", "intake", manifest.MaxIntakeQuestions, "intake questions")
		for i, q := range intake.Content {
			if q.Kind != yaml.MappingNode {
				continue
			}
			at := fmt.Sprintf("intake[%d]", i)
			if ch := value(q, "choices"); ch != nil && ch.Kind == yaml.SequenceNode {
				c.dropEmptyItems(ch, at+".choices", "choice")
				c.dedupe(q, "choices", at)
				c.dropIfEmpty(q, "choices", at)
			}
		}
	}
	if g := value(root, "guide"); g != nil && g.Kind == yaml.MappingNode {
		c.guide(g)
	}
	c.dropIfEmpty(root, "guide", "")
	if in := value(root, "interactive"); in != nil && in.Kind == yaml.MappingNode {
		c.dropIfEmpty(in, "opening", "interactive")
		c.shortenAt(in, "opening", "interactive.opening", manifest.MaxOpeningChars, false)
	}
	c.dropIfEmpty(root, "interactive", "")
	c.lift(root, "knowledge")
	if k := value(root, "knowledge"); k != nil && k.Kind == yaml.MappingNode {
		c.noShips(k)
		if a := value(k, "accepts"); a != nil && a.Kind == yaml.MappingNode {
			c.shortenAt(a, "prompt", "knowledge.accepts.prompt", manifest.MaxKnowledgePromptChars, false)
		}
	}
	c.dropIfEmpty(root, "knowledge", "")
	c.lift(root, "inputs")
	if in := value(root, "inputs"); in != nil && in.Kind == yaml.MappingNode {
		c.inputs(in)
	}
	c.dropIfEmpty(root, "inputs", "")
	if len(c.did) == 0 {
		return raw, nil
	}
	out, err := jsonOf(root)
	if err != nil {
		return raw, nil
	}
	return out, c.did
}

// guide corrects the guide's parts.
func (c *corrector) guide(g *yaml.Node) {
	for _, k := range []string{"overview", "files", "task"} {
		c.dropIfEmpty(g, k, "guide")
	}
	c.shortenAt(g, "overview", "guide.overview", manifest.MaxGuideOverviewBytes, true)
	c.shortenAt(g, "files", "guide.files", manifest.MaxGuideFilesChars, false)
	c.shortenAt(g, "task", "guide.task", manifest.MaxGuideTaskChars, false)
	if p := value(g, "prepare"); p != nil && p.Kind == yaml.SequenceNode {
		c.dropEmptyItems(p, "guide.prepare", "prepare item")
		c.cut(g, "prepare", "guide.prepare", manifest.MaxGuidePrepare, "prepare items")
		for i := range p.Content {
			c.shorten(p.Content[i], fmt.Sprintf("guide.prepare[%d]", i), manifest.MaxGuidePrepareChars, false)
		}
	}
	c.dropIfEmpty(g, "prepare", "guide")
	if k := value(g, "connect"); k != nil && k.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(k.Content); i += 2 {
			name, help := k.Content[i].Value, k.Content[i+1]
			if emptyValue(help) {
				c.did = append(c.did, fmt.Sprintf("guide.connect.%s: the empty help %s was removed.", name, quoted(help)))
				k.Content = append(k.Content[:i], k.Content[i+2:]...)
				i -= 2
				continue
			}
			c.shorten(help, "guide.connect."+name, manifest.MaxGuideConnectChars, false)
		}
	}
	c.dropIfEmpty(g, "connect", "guide")
}

type corrector struct{ did []string }

// dedupe drops a string that a list at key names a second time.
func (c *corrector) dedupe(m *yaml.Node, key string, at ...string) {
	l := value(m, key)
	if l == nil || l.Kind != yaml.SequenceNode {
		return
	}
	where := path(key, at...)
	seen := map[string]bool{}
	kept := l.Content[:0]
	for _, it := range l.Content {
		if it.Kind == yaml.ScalarNode && it.Tag == "!!str" {
			if seen[it.Value] {
				c.did = append(c.did, fmt.Sprintf("%s: %q was named twice, so the second was dropped.", where, it.Value))
				continue
			}
			seen[it.Value] = true
		}
		kept = append(kept, it)
	}
	l.Content = kept
}

// cut holds a list at key to its first max entries.
func (c *corrector) cut(m *yaml.Node, key, where string, maxItems int, what string) {
	l := value(m, key)
	if l == nil || l.Kind != yaml.SequenceNode || len(l.Content) <= maxItems {
		return
	}
	c.did = append(c.did, fmt.Sprintf("%s: %d %s, over the limit of %d, so the first %d were kept.", where, len(l.Content), what, maxItems, maxItems))
	l.Content = l.Content[:maxItems]
}

// dropEmptyItems removes the empty and placeholder entries of a list.
func (c *corrector) dropEmptyItems(l *yaml.Node, where, what string) {
	kept := l.Content[:0]
	for _, it := range l.Content {
		if emptyValue(it) {
			c.did = append(c.did, fmt.Sprintf("%s: the empty %s %s was removed.", where, what, quoted(it)))
			continue
		}
		kept = append(kept, it)
	}
	l.Content = kept
}

// lift moves a block written under capabilities to the manifest's top
// level, where the manifest takes it: knowledge (a draft with a pack can put
// it there) and inputs, neither of which is a capability. A manifest that
// already has one keeps its own, and the misplaced one is removed.
func (c *corrector) lift(root *yaml.Node, key string) {
	caps := value(root, "capabilities")
	if caps == nil || caps.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(caps.Content); i += 2 {
		if caps.Content[i].Value != key {
			continue
		}
		k, v := caps.Content[i], caps.Content[i+1]
		caps.Content = append(caps.Content[:i], caps.Content[i+2:]...)
		if value(root, key) != nil {
			c.did = append(c.did, fmt.Sprintf("capabilities.%s was removed: %s is a field of the manifest itself, which already has one.", key, key))
			return
		}
		root.Content = append(root.Content, k, v)
		c.did = append(c.did, fmt.Sprintf("capabilities.%s was moved to %s: it is a field of the manifest itself, not a capability.", key, key))
		return
	}
}

// inputKindNames are the slips a kind is written with, and the kind's
// name (manifest.InputKinds): its singular, or another case.
var inputKindNames = map[string]manifest.InputKind{
	"document": manifest.InputKindDocuments,
	"image":    manifest.InputKindImages,
}

// inputs corrects the slips of the inputs block: a kind written in the
// singular or another case, a kind named twice, and any beside the kinds
// the draft names, which the rules refuse since any stands alone. A kind
// outside the set is left for the rules to refuse.
func (c *corrector) inputs(in *yaml.Node) {
	l := value(in, "kinds")
	if l == nil || l.Kind != yaml.SequenceNode {
		return
	}
	for _, it := range l.Content {
		if it.Kind != yaml.ScalarNode {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(it.Value))
		if k, ok := inputKindNames[name]; ok {
			name = string(k)
		}
		if name != it.Value && manifest.InputKind(name).Valid() {
			c.did = append(c.did, fmt.Sprintf("inputs.kinds: %q was written %q, the kind's name.", it.Value, name))
			it.Value = name
		}
	}
	c.dedupe(in, "kinds", "inputs")
	if len(l.Content) > 1 {
		kept := l.Content[:0]
		for _, it := range l.Content {
			if it.Kind == yaml.ScalarNode && it.Value == string(manifest.InputKindAny) {
				c.did = append(c.did, "inputs.kinds: any was dropped: it stands alone, and the draft names the kinds it takes.")
				continue
			}
			kept = append(kept, it)
		}
		l.Content = kept
	}
	c.dropIfEmpty(in, "kinds", "inputs")
}

// noShips removes knowledge.ships: a draft never ships a pack. The person's
// own packs stay theirs; a QuickAgent that needs one at run time asks for it
// with knowledge.accepts, and the person attaches it to the run.
func (c *corrector) noShips(k *yaml.Node) {
	for i := 0; i+1 < len(k.Content); i += 2 {
		if k.Content[i].Value != "ships" {
			continue
		}
		k.Content = append(k.Content[:i], k.Content[i+2:]...)
		c.did = append(c.did, "knowledge.ships was removed: a draft never ships a knowledge pack. "+
			"A QuickAgent that needs one asks for it with knowledge.accepts, and the person attaches their own when they run it.")
		return
	}
}

// dropIfEmpty removes the optional part at key when it says nothing: an
// empty or placeholder text, an empty list or object, or null.
func (c *corrector) dropIfEmpty(m *yaml.Node, key string, at string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key || !emptyValue(m.Content[i+1]) {
			continue
		}
		v := m.Content[i+1]
		what := "empty"
		if v.Kind == yaml.ScalarNode && v.Tag == "!!str" && strings.TrimSpace(v.Value) != "" {
			what = "placeholder"
		}
		c.did = append(c.did, fmt.Sprintf("%s: the %s value %s was removed.", path(key, at), what, quoted(v)))
		m.Content = append(m.Content[:i], m.Content[i+2:]...)
		return
	}
}

// shortenAt shortens the text at key to maxLen characters, or bytes.
func (c *corrector) shortenAt(m *yaml.Node, key, where string, maxLen int, inBytes bool) {
	if v := value(m, key); v != nil {
		c.shorten(v, where, maxLen, inBytes)
	}
}

// shorten shortens a text past its bound at a word, and names it.
func (c *corrector) shorten(v *yaml.Node, where string, maxLen int, inBytes bool) {
	if v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
		return
	}
	measure, unit := utf8.RuneCountInString, "characters"
	if inBytes {
		measure, unit = func(s string) int { return len(s) }, "bytes"
	}
	n := measure(v.Value)
	if n <= maxLen {
		return
	}
	v.Value = shortenAtWord(v.Value, func(s string) bool { return measure(s) <= maxLen })
	c.did = append(c.did, fmt.Sprintf("%s: %d %s, over the limit of %d, so it was shortened at a word to %d.", where, n, unit, maxLen, measure(v.Value)))
}

// ellipsis ends a shortened text, so a reader sees it was cut.
const ellipsis = "…"

// shortenAtWord is the longest start of s, ending at a space where one is
// near enough, that fits with the ellipsis after it.
func shortenAtWord(s string, fits func(string) bool) string {
	cut, space := 0, -1
	for i, r := range s {
		if !fits(s[:i] + ellipsis) {
			break
		}
		if unicode.IsSpace(r) {
			space = i
		}
		cut = i
	}
	if space > cut/2 {
		cut = space
	}
	head := strings.TrimRightFunc(s[:cut], func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(",;:-–—(", r) })
	return head + ellipsis
}

// placeholders are what a model writes for a part it had nothing to say in.
var placeholders = map[string]bool{"none": true, "n/a": true, "na": true, "null": true, "nil": true, "-": true, "—": true,
	"not applicable": true, "nothing": true, "not needed": true, "none needed": true}

// emptyValue says a value says nothing: null, an empty list or object, or an
// empty or placeholder text.
func emptyValue(v *yaml.Node) bool {
	switch v.Kind {
	case yaml.MappingNode, yaml.SequenceNode:
		return len(v.Content) == 0
	case yaml.ScalarNode:
		if v.Tag == "!!null" {
			return true
		}
		if v.Tag != "!!str" {
			return false
		}
		t := strings.ToLower(strings.TrimRight(strings.TrimSpace(v.Value), "."))
		return t == "" || placeholders[t]
	}
	return false
}

// value is the value at key of a mapping, or nil.
func value(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func path(key string, at ...string) string {
	if len(at) == 0 || at[0] == "" {
		return key
	}
	return at[0] + "." + key
}

// quoted names a value in a correction: its text, cut short, or its kind.
func quoted(v *yaml.Node) string {
	switch {
	case v.Kind == yaml.ScalarNode && v.Tag == "!!str":
		s := v.Value
		if utf8.RuneCountInString(s) > 40 {
			s = string([]rune(s)[:40]) + ellipsis
		}
		return strconv.Quote(s)
	case v.Kind == yaml.SequenceNode:
		return "[]"
	case v.Kind == yaml.MappingNode:
		return "{}"
	}
	return v.Value
}

// jsonOf writes a node read from JSON back as JSON, its keys in the order
// they came.
func jsonOf(n *yaml.Node) (json.RawMessage, error) {
	var b bytes.Buffer
	var write func(*yaml.Node) error
	write = func(n *yaml.Node) error {
		switch n.Kind {
		case yaml.MappingNode:
			b.WriteByte('{')
			for i := 0; i+1 < len(n.Content); i += 2 {
				if i > 0 {
					b.WriteByte(',')
				}
				if err := writeString(&b, n.Content[i].Value); err != nil {
					return err
				}
				b.WriteByte(':')
				if err := write(n.Content[i+1]); err != nil {
					return err
				}
			}
			b.WriteByte('}')
		case yaml.SequenceNode:
			b.WriteByte('[')
			for i, it := range n.Content {
				if i > 0 {
					b.WriteByte(',')
				}
				if err := write(it); err != nil {
					return err
				}
			}
			b.WriteByte(']')
		case yaml.ScalarNode:
			switch n.Tag {
			case "!!str":
				if err := writeString(&b, n.Value); err != nil {
					return err
				}
			case "!!null":
				b.WriteString("null")
			case "!!bool", "!!int", "!!float":
				if !json.Valid([]byte(n.Value)) {
					return fmt.Errorf("the value %q is not JSON", n.Value)
				}
				b.WriteString(n.Value)
			default:
				return fmt.Errorf("a value tagged %s", n.Tag)
			}
		default:
			return fmt.Errorf("a node of kind %d", n.Kind)
		}
		return nil
	}
	if err := write(n); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// writeString writes s as a JSON string, leaving <, > and & as they came.
func writeString(b *bytes.Buffer, s string) error {
	enc := json.NewEncoder(b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	b.Truncate(b.Len() - 1) // Encode's newline
	return nil
}
