package policy

// What an organization's managed settings hold a policy to, beside the
// file. The managed settings are read and verified elsewhere
// (pkg/managed); this is the part every reader of the policy consults, so
// that a check the policy answers (may this release use "*" egress, may a
// messenger reach this agent, is the malware check on) answers the same
// way wherever it is asked. Held values are never written to the file:
// Save writes the file's own fields, and a member's own choice stays
// theirs for the day the organization lets go of it.

// Managed setting ids the policy's own checks consult. They are the ids of
// the managed settings catalogue's rows that narrow a field of this file.
const (
	HeldWildcardEgress     = "egress.wildcard"
	HeldPublicWeb          = "browser.publicWeb"
	HeldVulnerableServers  = "mcpServers.allowVulnerable"
	HeldDesktop            = "desktop.applications"
	HeldScreen             = "desktop.screen"
	HeldCustomServers      = "desktop.customServers"
	HeldMessengers         = "messengers"
	HeldPersistentReleases = "persistentReleases"
)

// Held is what managed settings hold the policy to. A Held only ever
// refuses or turns a check on: nothing in it allows what the file does not.
type Held struct {
	// Refused maps a Held* id to the sentence that refuses what an
	// organization locked off. A refusal holds whatever the file says.
	Refused map[string]string
	// Defaulted maps a Held* id to the sentence of an organization default
	// that turns it off. It applies only where the file leaves the field
	// unset, so a member who chose otherwise keeps their choice.
	Defaulted map[string]string
	// MalwareCheck is the organization that requires the malware check, by
	// lock; the check is then on whatever the file says.
	MalwareCheck string
	// MalwareCheckDefault turns the malware check on where the file leaves
	// malwareCheck unset.
	MalwareCheckDefault bool
}

// any says h holds anything.
func (h Held) any() bool {
	return len(h.Refused) > 0 || len(h.Defaulted) > 0 || h.MalwareCheck != "" || h.MalwareCheckDefault
}

// WithHeld is a copy of p held to h. The copy is a view for checks and
// runs, never saved: Save writes only the file's own fields.
func (p *Policy) WithHeld(h Held) *Policy {
	if p == nil {
		return nil
	}
	q := *p
	q.held = h
	return &q
}

// Held is what p is held to.
func (p *Policy) Held() Held {
	if p == nil {
		return Held{}
	}
	return p.held
}

// Refused is the sentence an organization's lock refuses the id with, or
// "".
func (p *Policy) Refused(id string) string {
	if p == nil {
		return ""
	}
	return p.held.Refused[id]
}

// refusedOrDefaulted is Refused, or the organization default's sentence
// when the file leaves the field unset (unset true).
func (p *Policy) refusedOrDefaulted(id string, unset bool) string {
	if r := p.Refused(id); r != "" {
		return r
	}
	if p == nil || !unset {
		return ""
	}
	return p.held.Defaulted[id]
}

// PersistentReleasesRefusal is why a persistent release that is not built
// into Agentry may not run as a persistent agent on this computer, or ""
// when it may.
func (p *Policy) PersistentReleasesRefusal() string {
	if r := p.refusedOrDefaulted(HeldPersistentReleases, p != nil && p.AllowPersistentReleases == nil); r != "" {
		return r
	}
	if !p.PersistentReleasesAllowed() {
		return PersistentReleasesRefused
	}
	return ""
}

// DesktopRefusal is why a release may not use an application on the
// person's computer, or "" when it may.
func (p *Policy) DesktopRefusal() string {
	if r := p.refusedOrDefaulted(HeldDesktop, p != nil && p.Desktop.Allow == nil); r != "" {
		return r
	}
	if p != nil && !p.Desktop.Allowed() {
		return DesktopRefused
	}
	return ""
}

// CustomServersRefusal is the organization's refusal of an application the
// person set up themself, or "". The file's own setting is answered by
// DesktopPolicy.CustomServersAllowed, which Unsafe Mode may lift; an
// organization's refusal holds in Unsafe Mode too.
func (p *Policy) CustomServersRefusal() string {
	return p.refusedOrDefaulted(HeldCustomServers, p != nil && p.Desktop.AllowCustomServers == nil)
}

// Merge is h with o's holds added: a refusal or a default h does not have
// of its own, o's malware check lock when h has none, and o's malware check
// default. The result refuses whatever either refuses.
func (h Held) Merge(o Held) Held {
	out := Held{MalwareCheck: h.MalwareCheck, MalwareCheckDefault: h.MalwareCheckDefault || o.MalwareCheckDefault}
	if out.MalwareCheck == "" {
		out.MalwareCheck = o.MalwareCheck
	}
	out.Refused = mergeSentences(h.Refused, o.Refused)
	out.Defaulted = mergeSentences(h.Defaulted, o.Defaulted)
	return out
}

func mergeSentences(a, b map[string]string) map[string]string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]string, len(a)+len(b))
	for id, r := range b {
		out[id] = r
	}
	for id, r := range a {
		out[id] = r
	}
	return out
}
