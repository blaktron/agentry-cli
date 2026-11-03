package localrun

import (
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
	"github.com/blaktron/agentry/pkg/manifest"
)

// TestEveryPersistentNameMaps: each image server and mail provider the
// manifest may name maps to one this build carries, each image server to a
// different one; the mail providers reached through an image server are
// linked to the same server here as in the manifest's rules; and a name the
// manifest does not know is refused.
func TestEveryPersistentNameMaps(t *testing.T) {
	offered := assistant.ImageServers("gog-image", "ms365-image")
	seen := map[string]bool{}
	for _, n := range manifest.ImageServerNames {
		key, ok := PersistentImageServer(n)
		if !ok {
			t.Errorf("the image server %q maps to nothing", n)
			continue
		}
		if seen[key] {
			t.Errorf("two image servers map to %s", key)
		}
		seen[key] = true
		if !slices.ContainsFunc(offered, func(o offer.ImageServer) bool { return o.Key == key }) {
			t.Errorf("the image server %q maps to %s, which this build does not run", n, key)
		}
	}
	nameOf := map[string]string{}
	for _, n := range manifest.ImageServerNames {
		if key, ok := PersistentImageServer(n); ok {
			nameOf[key] = n
		}
	}
	for _, n := range manifest.MailProviderNames {
		if _, ok := PersistentMailProvider(n); !ok {
			t.Errorf("the mail provider %q maps to nothing", n)
		}
	}
	for _, p := range assistant.MailProviders() {
		if !slices.Contains(manifest.MailProviderNames, p.Key) {
			t.Errorf("the mail provider %s is not one the manifest names", p.Key)
		}
		var servers []string
		for _, k := range p.ImageServers {
			servers = append(servers, nameOf[k])
		}
		with := persistentManifest(t, p.Key, servers)
		if errs := manifest.Blocking(manifest.Validate(with)); len(errs) > 0 {
			t.Errorf("mail at %s with %v: %v", p.Key, servers, errs)
		}
		if len(servers) > 0 {
			without := persistentManifest(t, p.Key, nil)
			if errs := manifest.Blocking(manifest.Validate(without)); len(errs) == 0 {
				t.Errorf("mail at %s without %v validates, and this build reaches it through them", p.Key, servers)
			}
		}
	}
	// The providers and servers a block declares are the ones it names.
	providers := func(p *manifest.Persistent) []string {
		var out []string
		for _, m := range MailProvidersFor(p) {
			out = append(out, m.Key)
		}
		return out
	}
	if got := providers(&manifest.Persistent{Mail: []string{"fastmail", "google"}}); !slices.Equal(got, []string{assistant.MailGoogle, assistant.MailFastmail}) {
		t.Errorf("a block that names Google and Fastmail declares the providers %v", got)
	}
	if len(MailProvidersFor(nil)) != 0 || len(MailProvidersFor(&manifest.Persistent{})) != 0 {
		t.Error("a block with no mail declares providers")
	}
	for name, p := range map[string]*manifest.Persistent{"nothing": nil, "no mail": {}, "mail reached through an image server": {Mail: []string{"google", "microsoft"}}} {
		if got := MailServersFor(p); len(got) != 0 {
			t.Errorf("%s: the servers of mail providers are %+v", name, got)
		}
	}
	fastmail := MailServersFor(&manifest.Persistent{Mail: []string{"google", "fastmail"}})
	if len(fastmail) == 0 {
		t.Fatal("a block that names Fastmail is offered no server of it")
	}
	for _, s := range fastmail {
		if s.MailProvider != assistant.MailFastmail {
			t.Errorf("%s is offered for mail at %q", s.Name, s.MailProvider)
		}
	}
	if err := CheckPersistentNames(&manifest.Persistent{ImageServers: []string{"google-workspace"}, Mail: []string{"google"}}); err != nil {
		t.Errorf("known names refused: %v", err)
	}
	for _, p := range []*manifest.Persistent{{ImageServers: []string{"notion"}}, {Mail: []string{"yahoo"}}} {
		if err := CheckPersistentNames(p); err == nil || !strings.Contains(err.Error(), "update Agentry") {
			t.Errorf("%+v: err = %v, want the refusal", p, err)
		}
	}
}

// persistentManifest is a persistent release that declares mail at
// provider and the image servers servers.
func persistentManifest(t *testing.T, provider string, servers []string) *manifest.Manifest {
	t.Helper()
	text := "schemaVersion: \"2\"\nname: mailer\nnamespace: demo\nmodes: [persistent]\nversion: 1.0.0\nsummary: s\n" +
		"capabilities:\n  persistent:\n    mail: [" + provider + "]\n"
	if len(servers) > 0 {
		text += "    imageServers: [" + strings.Join(servers, ", ") + "]\n"
	}
	m, err := manifest.Parse([]byte(text + "publish:\n  include: [agent.yaml, AGENT.md]\n"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestTheWebToolsAndReviewAreDeclaredAlone: a block that declares coder
// beside web, review or calendars is refused; those three sit together.
func TestTheWebToolsAndReviewAreDeclaredAlone(t *testing.T) {
	for _, c := range []struct {
		p  manifest.Persistent
		ok bool
	}{
		{manifest.Persistent{Web: true}, true},
		{manifest.Persistent{Review: true, Memory: true}, true},
		{manifest.Persistent{Coder: true}, true},
		{manifest.Persistent{Coder: true, Calendars: true}, false},
		{manifest.Persistent{Coder: true, Review: true}, false},
		{manifest.Persistent{Web: true, Coder: true}, false},
		{manifest.Persistent{Web: true, Review: true}, true},
		{manifest.Persistent{Review: true, Calendars: true}, true},
		{manifest.Persistent{Web: true, Review: true, Calendars: true}, true},
		{manifest.Persistent{Coder: true, Memory: true, Helper: true, Skills: true}, true},
	} {
		if err := CheckPersistentNames(&c.p); (err == nil) != c.ok {
			t.Errorf("%v: %v", c.p.Keys(), err)
		}
	}
}
