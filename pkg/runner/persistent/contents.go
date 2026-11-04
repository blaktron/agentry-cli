package persistent

// What a running segment was given, entry by entry, beyond the roster's
// switches: an image server's set-up, the dev server's last save, and the
// content of the knowledge packs the person attached and of their Writing
// Voice. A set-up done, a dev server saved again, a pack built again or a
// voice improved while the agent runs leaves every switch as it was, so the
// supervisor compares these signatures as well as the grants: a changed
// one makes the next turn start a segment with it, and the roster names its
// entry pending until then.

import (
	"context"
	"encoding/json"
	"slices"
	"time"
)

// Contents read the digest of what a segment attaches, as the client's
// stores hold it now, without opening it. A client that gives Packs or
// Voice gives these too, or a pack built again or a voice improved reaches
// the agent only when something else restarts it.
type Contents struct {
	// Pack is the digest of the knowledge pack name in the local store.
	Pack func(ctx context.Context, name string) (string, error)
	// Voice is the digest of the person's Writing Voice.
	Voice func(ctx context.Context) (string, error)
}

// Signatures are what each roster entry gives a segment beyond being on,
// by entry id. They never hold a secret or what a pack or voice says.
type Signatures map[string]string

// configSignatures are the signatures config c alone decides: each image
// server given, by its grant (switches, account, client, tenant, signoff,
// none of them a secret) and when its client was set and it was signed in;
// and the dev server while on, by when it was last saved.
func configSignatures(a Agent, c Config, r Roster) Signatures {
	on := func(id string) bool { e, ok := r.Entry(id); return ok && e.On }
	out := Signatures{}
	grants, _ := imageGrants("", a, c, on)
	for _, g := range grants {
		in := c.SignIns[g.Key]
		// The volume is the runner home's path to the server's state,
		// fixed for the agent: not something a set-up changes.
		g.Volume = ""
		b, _ := json.Marshal(struct {
			Grant      any
			Client, At time.Time
		}{g, in.Client, in.At})
		out[ImageServerEntryID(g.Key)] = string(b)
	}
	if d := c.DevHost; d != nil && a.Codes() && on(EntryDevHost) {
		out[EntryDevHost] = d.SavedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// contentSignatures are the digests of the content a segment of a with
// config c attaches: each pack the person added and has on, and the voice
// when it is on. A digest that cannot be read is empty, which counts as
// unchanged: a pack gone from the store is the next start's to refuse, not
// a reason to restart a running agent onto a segment that cannot open it.
func contentSignatures(ctx context.Context, a Agent, c Config, r Roster, cs Contents) Signatures {
	on := func(id string) bool { e, ok := r.Entry(id); return ok && e.On }
	out := Signatures{}
	read := func(d string, err error) string {
		if err != nil {
			return ""
		}
		return d
	}
	if cs.Pack != nil {
		for _, name := range c.Knowledge {
			if on(KnowledgeEntryID(name)) && !slices.Contains(a.Packs, name) {
				out[KnowledgeEntryID(name)] = read(cs.Pack(ctx, name))
			}
		}
	}
	if cs.Voice != nil && a.Published() && on(EntryVoice) {
		out[EntryVoice] = read(cs.Voice(ctx))
	}
	return out
}

// changed are the entries whose signature now differs from what the
// segment was given; an entry the segment had no signature for is the
// grants' to tell, and an empty one now is unreadable, so unchanged.
func (given Signatures) changed(now Signatures) []string {
	var out []string
	for id, sig := range now {
		was, ok := given[id]
		if ok && sig != "" && was != sig {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}
