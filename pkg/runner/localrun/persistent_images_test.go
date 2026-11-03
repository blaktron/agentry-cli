package localrun

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// TestAnInstallKeepsThePlansImageServers: after a package server's
// install, the plan's servers are its resolution's specs and, unchanged, the
// image servers it had, so a segment that installs a server in the
// background still has its Google Workspace when it compiles again.
func TestAnInstallKeepsThePlansImageServers(t *testing.T) {
	img := spec.MCPServerSpec{Name: "mcp-image-gog", Source: spec.SourceImage, Package: "img", Image: "img", Command: []string{"mcp"}, Volumes: []string{"v:/persist/gogcli"}}
	servers := []manifest.MCPServer{{NPM: "@scope/x", Version: "1.0.0", Egress: []string{"a.example.com"}}}
	resolved := []mcpinstall.Resolved{{Source: "npm", Package: "@scope/x", Version: "1.0.0", ID: "npm-scope-x", BinName: "x"}}
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}
	in := PlanInputs{Manifest: &manifest.Manifest{Capabilities: manifest.Capabilities{MCPServers: servers}}, Policy: &policy.Policy{Images: images}}
	in.Servers = append(mcpinstall.Specs(resolved, servers, images), img)
	got := installedSpecs(resolved, in)
	if !reflect.DeepEqual(got, in.Servers) {
		t.Fatalf("after the install the plan's servers are %+v, want %+v", got, in.Servers)
	}
	if !slices.ContainsFunc(got, func(s spec.MCPServerSpec) bool { return s.FromImage() && s.Name == "mcp-image-gog" }) {
		t.Fatal("the image server was dropped")
	}
}

// TestASegmentGivesItsImageServersFromThePolicysImage: a granted image server
// becomes a spec of the built-in's offer at the policy's image, with the
// switches' arguments and the grant's volume; one the built-in does not
// offer is refused.
func TestASegmentGivesItsImageServersFromThePolicysImage(t *testing.T) {
	b := Assistant()
	pol := policy.Default()
	pol.Images.Gog = "ghcr.io/example/gog:2@sha256:" + "b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4"
	s := Segment{Builtin: b, Policy: pol, Grants: Grants{ImageServers: []ImageGrant{{Key: "gog", Switches: []string{"writes"}, Volume: "vol-gog"}}}}
	specs, volumes, err := s.imageSpecs()
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Image != pol.Images.Gog || specs[0].Package != pol.Images.Gog || !slices.Equal(specs[0].Command, []string{"mcp", "--allow-write"}) ||
		!slices.Equal(specs[0].Volumes, []string{"vol-gog:/persist/gogcli"}) || !slices.Equal(volumes, []string{"vol-gog"}) {
		t.Fatalf("specs %+v volumes %v", specs, volumes)
	}
	// What the grant adds reaches the spec: the signed-in account in the
	// server's account variable, its sign-in environment and arguments, the
	// switches' arguments in order, and the offer's own egress and secret
	// slots; the grant's volume is mounted at the offer's target.
	served := assistant.ImageServers(pol.Images.GogImage(), pol.Images.MS365Image())[0]
	rich := Segment{Builtin: b, Policy: pol, Grants: Grants{ImageServers: []ImageGrant{{
		Key: "gog", Volume: "vol-gog", Account: "ada@example.com", Switches: []string{"writes", "send"},
		Env: map[string]string{"GOG_EXTRA": "1"}, Args: []string{"--signoff", "Ada"},
	}}}}
	richSpecs, richVolumes, err := rich.imageSpecs()
	if err != nil || len(richSpecs) != 1 || !slices.Equal(richVolumes, []string{"vol-gog"}) {
		t.Fatalf("specs %+v volumes %v: %v", richSpecs, richVolumes, err)
	}
	wantSpec := spec.MCPServerSpec{
		Name: "mcp-image-gog", Source: spec.SourceImage, Package: pol.Images.Gog, Image: pol.Images.Gog,
		Command: []string{"mcp", "--allow-write", "--allow-gmail-send", "--signoff", "Ada"},
		Egress:  assistant.GogEgress, Secrets: []string{assistant.GogKeyringSecret},
		Env:     map[string]string{"GOG_HOME": assistant.GogHome, "GOG_KEYRING_BACKEND": "file", "GOG_ACCOUNT": "ada@example.com", "GOG_EXTRA": "1"},
		Volumes: []string{"vol-gog:" + served.VolumeTarget}, Account: "ada@example.com",
	}
	if !reflect.DeepEqual(richSpecs[0], wantSpec) {
		t.Fatalf("spec %+v, want %+v", richSpecs[0], wantSpec)
	}
	// A package is offered the image servers it declares and no others.
	keys := func(servers []offer.ImageServer) []string {
		var out []string
		for _, o := range servers {
			out = append(out, o.Key)
		}
		return out
	}
	if got := keys(Assistant().ImageServers(pol.Images)); !slices.Equal(got, []string{assistant.GogKey, assistant.MS365Key}) {
		t.Errorf("the Assistant is offered %v", got)
	}
	for _, none := range []Builtin{Main(), Coder(), {}} {
		if got := none.ImageServers(pol.Images); len(got) != 0 {
			t.Errorf("%q declares no image server and is offered %v", none.Ref(), keys(got))
		}
	}
	if got := keys(ImageServersFor(&manifest.Persistent{ImageServers: []string{"google-workspace"}}, pol.Images)); !slices.Equal(got, []string{assistant.GogKey}) {
		t.Errorf("a block that names Google Workspace is offered %v", got)
	}
	s.Grants.ImageServers[0].Key = "other"
	if _, _, err := s.imageSpecs(); err == nil {
		t.Fatal("a server the built-in does not offer was given")
	}
}

// A server signed in with Agentry's own app is given only at the build's own
// image: a policy naming another image refuses the segment, saying why.
func TestASignInWithAgentrysAppIsGivenOnlyToTheBuildsImage(t *testing.T) {
	b := Assistant()
	pol := policy.Default()
	s := Segment{Builtin: b, Policy: pol, Grants: Grants{ImageServers: []ImageGrant{{Key: "ms365", Volume: "vol-ms365", AgentryApp: true}}}}
	if specs, _, err := s.imageSpecs(); err != nil || len(specs) != 1 || specs[0].Image != policy.DefaultMS365Image {
		t.Fatalf("at the build's image: %+v, %v", specs, err)
	}
	pol.Images.MS365 = "ghcr.io/example/ms365:1@sha256:" + "c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5"
	if _, _, err := s.imageSpecs(); err == nil || !strings.Contains(err.Error(), "Agentry's own app") {
		t.Fatalf("at another image: %v", err)
	}
	s.Grants.ImageServers[0].AgentryApp = false
	if _, _, err := s.imageSpecs(); err != nil {
		t.Fatalf("a sign-in with the person's own client at another image: %v", err)
	}
}
