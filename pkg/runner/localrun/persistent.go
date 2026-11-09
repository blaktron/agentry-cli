package localrun

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/persistent/coder"
	mainagent "github.com/blaktron/agentry-cli/pkg/persistent/main"
	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/intake"
	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/osvdb"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/skill"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
	"github.com/blaktron/agentry-cli/pkg/trust"
	"github.com/blaktron/agentry-cli/runnerimage"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
	"github.com/blaktron/agentry/pkg/manifest"
)

// Assistant is the Assistant (pkg/persistent/assistant), the first built-in
// that runs as a persistent agent. A file in its share is read only once
// ClamAV has checked it. Its manifest declares what else it has: the time
// and its tasks, a memory, skills, stored secrets, its own name and
// persona, Agentry's helper tools and the meeting tools; it is granted the
// publisher tools by name (WithGrantedParts). It resets in
// parts: its history alone, or everything but the credentials it was given.
// It reads the calendar feeds the person gives it.
func Assistant() Builtin {
	return Builtin{namespace: assistant.Namespace, name: assistant.Name, version: assistant.Version, fsys: assistant.FS(),
		persistent: true, runnable: true, checkShare: true, resetsInParts: true,
		offers: offersAssistant, publisher: true}
}

// Main is Agentry Helper (pkg/persistent/main), the persistent agent of Agentry
// Desktop's Home: a file in its share is read only once ClamAV has checked
// it, and its manifest declares Agentry's helper tools and skills.
func Main() Builtin {
	return Builtin{namespace: mainagent.Namespace, name: mainagent.Name, version: mainagent.Version, fsys: mainagent.FS(),
		persistent: true, runnable: true, checkShare: true}
}

// Coder is the Coding agent (pkg/persistent/coder): its programs run in its
// own toolchain image, it may reach the internet, a browser and the person's
// dev server, each as its roster has them on, and it reads its share as it
// is: it builds and runs software. Its manifest declares skills and stored
// secrets, which reach its programs and the servers that declare them.
func Coder() Builtin {
	return Builtin{namespace: coder.Namespace, name: coder.Name, version: coder.Version, fsys: coder.FS(),
		persistent: true, runnable: true, offers: offersCoder}
}

// Declared is what the built-in declares under capabilities.persistent in its
// own manifest, read once; nil for none, and for the zero Builtin. A
// built-in's manifest that does not parse fails the build's tests, never a
// start.
func (b Builtin) Declared() *manifest.Persistent {
	if b.fsys == nil {
		return nil
	}
	if v, ok := builtinDeclared.Load(b.Ref()); ok {
		return v.(*manifest.Persistent)
	}
	var p *manifest.Persistent
	if raw, err := fs.ReadFile(b.fsys, "agent.yaml"); err == nil {
		if m, err := manifest.Parse(raw); err == nil {
			p = m.Capabilities.Persistent
		}
	}
	builtinDeclared.Store(b.Ref(), p)
	return p
}

// builtinDeclared are each built-in's declared capabilities by its ref.
var builtinDeclared sync.Map

// KeepsTime, KeepsMemory, KeepsSecrets and ArmsMeetings say the built-in
// declares clock, memory, secrets and meetings; AppLink is the app link
// tool set its declared keys compose. A published agent's are its
// release's (persistent.Agent).
func (b Builtin) KeepsTime() bool    { return b.Declared().Declares("clock") }
func (b Builtin) KeepsMemory() bool  { return b.Declared().Declares("memory") }
func (b Builtin) KeepsSecrets() bool { return b.Declared().Declares("secrets") }
func (b Builtin) ArmsMeetings() bool { return b.Declared().Declares("meetings") }
func (b Builtin) AppLink() string    { return b.WithGrantedParts(compile.AppLinkSet(b.Declared())) }

// ManagesQuickAgents says the built-in's app link has the publisher tools,
// through which it manages the person's QuickAgents: granted by name, to the
// Assistant alone.
func (b Builtin) ManagesQuickAgents() bool { return b.publisher }

// WithGrantedParts is set with the parts the built-in is granted by name
// rather than by a key it declares: the publisher tools, for the built-in
// that carries them, on an app link it has. The zero Builtin, which a
// published agent's segment carries, is granted none.
func (b Builtin) WithGrantedParts(set string) string {
	if set == "" || !b.publisher {
		return set
	}
	return applink.Compose(append(applink.SetParts(set), applink.PartPublisher)...)
}

// PersistentBuiltins are the built-ins that run as persistent agents.
func PersistentBuiltins() []Builtin { return []Builtin{Main(), Assistant(), Coder()} }

// Codes says the persistent built-in declares the coding toolchain
// (Grants.Coder).
func (b Builtin) Codes() bool { return b.Declared().Declares("coder") }

// Runnable says a persistent built-in runs as an agent; one that is not is
// published for reading alone.
func (b Builtin) Runnable() bool { return b.runnable }

// BuiltinSkill is one of a persistent built-in's own skills: its meta and
// its folder in the built-in's tree.
type BuiltinSkill struct {
	skill.Meta
	FS fs.FS
}

// Skills are the persistent built-in's own skills, each folder beneath its
// tree's skills/, in name order; none for a built-in without one. A folder
// that is not a skill fails the build's tests, never a start.
func (b Builtin) Skills() []BuiltinSkill {
	if v, ok := builtinSkills.Load(b.Ref()); ok {
		return v.([]BuiltinSkill)
	}
	out := b.readSkills()
	builtinSkills.Store(b.Ref(), out)
	return out
}

// builtinSkills are each built-in's skills by its ref, read once: a roster
// is built at every turn, and a built-in's tree never changes under its
// version.
var builtinSkills sync.Map

func (b Builtin) readSkills() []BuiltinSkill {
	if b.fsys == nil {
		// The zero Builtin, a published agent's, has no tree.
		return nil
	}
	return SkillsOf(b.fsys)
}

// SkillsOf are the skills in a package's tree: each folder beneath its
// skills/ that is a skill of its folder's name, in name order. A folder
// that is not one is left out.
func SkillsOf(fsys fs.FS) []BuiltinSkill {
	sub, err := fs.Sub(fsys, manifest.SkillsDir)
	if err != nil {
		return nil
	}
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		return nil
	}
	var out []BuiltinSkill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir, err := fs.Sub(sub, e.Name())
		if err != nil {
			continue
		}
		m, err := skill.Load(dir)
		if err != nil || m.Name != e.Name() {
			continue
		}
		out = append(out, BuiltinSkill{Meta: m, FS: dir})
	}
	return out
}

// OfferedServers are the MCP servers the persistent built-in's package comes
// with, each an entry of its roster, some drawn for the agent's git host
// accounts (hosts), and the servers of the mail providers it declares; a
// published agent's are its mail providers' alone (MailServersFor).
func (b Builtin) OfferedServers(hosts []offer.GitHost) []offer.Server {
	switch b.offers {
	case offersAssistant:
		return assistant.OfferedServers()
	case offersCoder:
		return coder.OfferedServers(hosts)
	}
	return nil
}

// offerSet is a built-in package's set of offered and suggested servers.
type offerSet string

// The sets there are.
const (
	offersAssistant offerSet = "assistant"
	offersCoder     offerSet = "coder"
)

// ImageServers are the servers the persistent built-in declares that run
// from an image the CLI pins (images, the policy's), each an entry of its
// roster (ImageServersFor).
func (b Builtin) ImageServers(images policy.Images) []offer.ImageServer {
	return ImageServersFor(b.Declared(), images)
}

// MailProviders are the services whose mail and calendar the persistent
// built-in declares (MailProvidersFor).
func (b Builtin) MailProviders() []offer.MailProvider { return MailProvidersFor(b.Declared()) }

// SuggestedServers are the MCP servers the persistent built-in's package
// suggests: not on its roster until the person adds one as their own.
func (b Builtin) SuggestedServers() []offer.Suggestion {
	switch b.offers {
	case offersAssistant:
		return assistant.SuggestedServers()
	case offersCoder:
		return coder.SuggestedServers()
	}
	return nil
}

// ReadsCalendars says the persistent built-in declares calendars: the person
// may give it calendar feeds (Grants.CalendarFeeds).
func (b Builtin) ReadsCalendars() bool { return b.Declared().Declares("calendars") }

// ImageServersFor are the servers run from an image the CLI pins (images,
// the policy's) that p declares under imageServers, for any agent: each an
// entry of its roster.
func ImageServersFor(p *manifest.Persistent, images policy.Images) []offer.ImageServer {
	declared := compile.DeclaredImageServers(p)
	var out []offer.ImageServer
	for _, s := range assistant.ImageServers(images.GogImage(), images.MS365Image()) {
		if slices.Contains(declared, s.Key) {
			out = append(out, s)
		}
	}
	return out
}

// MailProvidersFor are the mail providers p declares under mail, of the
// ones this build reaches.
func MailProvidersFor(p *manifest.Persistent) []offer.MailProvider {
	if p == nil {
		return nil
	}
	var out []offer.MailProvider
	for _, m := range assistant.MailProviders() {
		if slices.Contains(p.Mail, m.Key) {
			out = append(out, m)
		}
	}
	return out
}

// MailServersFor are the offered servers of the mail providers p declares
// that reach their mail through a server of their own rather than an image
// server (Fastmail's): a published agent's offered servers.
func MailServersFor(p *manifest.Persistent) []offer.Server {
	if p == nil {
		return nil
	}
	var out []offer.Server
	for _, s := range assistant.OfferedServers() {
		if s.MailProvider != "" && slices.Contains(p.Mail, s.MailProvider) {
			out = append(out, s)
		}
	}
	return out
}

// ResetsInParts says the persistent agent has Reset history and Reset
// agent beside its Reset.
func (b Builtin) ResetsInParts() bool { return b.resetsInParts }

// MemorySeed is what the persistent built-in's memory volume starts with:
// the files beneath its tree's memory/ folder, by path beneath it; none for
// a built-in that declares no memory.
func (b Builtin) MemorySeed() (map[string][]byte, error) {
	if !b.Declared().Declares("memory") {
		return nil, nil
	}
	return MemorySeedOf(b.fsys, b.Ref())
}

// MemorySeedOf is the memory seed in a package's tree: the files beneath
// its memory/ folder, by path beneath it; none when it has no such folder.
func MemorySeedOf(fsys fs.FS, ref string) (map[string][]byte, error) {
	sub, err := fs.Sub(fsys, manifest.MemorySeedDir)
	if err != nil {
		return nil, err
	}
	if _, err := fs.Stat(sub, "."); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	out := map[string][]byte{}
	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(sub, p)
		out[p] = data
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("the memory seed of %s: %w", ref, err)
	}
	return out, nil
}

// Persistent says the built-in runs as a persistent agent: only a Segment
// starts it, and Execute refuses it.
func (b Builtin) Persistent() bool { return b.persistent }

// ChecksShare says a file in the persistent built-in's share folder is read
// only once ClamAV has checked it; otherwise the share is read as it is.
func (b Builtin) ChecksShare() bool { return b.checkShare }

// Grants are what a persistent agent's roster turns on for one segment,
// beside what its built-in declares: the person's share folder, the
// knowledge packs it searches, and the MCP servers the person added. The
// persistent package's Apply is the one function that makes them.
type Grants struct {
	// Share is the share folder, absolute; empty when the agent has it off.
	Share string
	// Knowledge are the packs attached to the segment, opened by the
	// caller.
	Knowledge []engine.AttachedPack
	// Servers are the MCP servers the person added and has on, in the
	// manifest's shape.
	Servers []manifest.MCPServer
	// ShareServers are the servers among Servers, by source id
	// (manifest.MCPServer.SourceID), that the person let see the share
	// folder: read only, through its mirror where the share is checked.
	// Only with Share.
	ShareServers []string
	// ServersDir is the agent's own store a package server is installed
	// into, as a release's declared server is into its profile's.
	ServersDir string
	// Skills are the skills the roster has on, by name and description,
	// and SkillsDir the folder holding exactly them, filled by the caller;
	// both empty for none.
	Skills    []spec.Skill
	SkillsDir string
	// AppLink is the socket the supervisor answers the agent's app link on;
	// empty gives the segment no app link.
	AppLink string
	// Subagents says the roster names subagents, so the app link lists the
	// subagent tools beside the parts the package declares.
	Subagents bool
	// Coder is what the Coding agent's roster has on of its grant; nil for
	// any other agent, which is refused it.
	Coder *spec.CoderGrant
	// DevHost is the Coding agent's dev server, readied (its address, its
	// pinned host key, its sign-in): set exactly when Coder.DevHost is.
	DevHost *targethost.Host
	// Secrets are the person's stored secrets the roster has on, by name:
	// each is given to the servers that declare it and, for the Coding
	// agent with its commands on, to its programs' environment.
	// WithheldSecrets are those it has off: a server that declares one is
	// given an empty value, never one from the policy's secret source.
	Secrets         map[string]string
	WithheldSecrets []string
	// ServerSecrets fill the secret slots of the offered servers that sign
	// in with a git host account, by the slot's name: given to those
	// servers alone, never to the Coding agent's programs.
	ServerSecrets map[string]string
	// Accounts are the Coding agent's git host accounts the roster has on,
	// each with its token, given to its programs with its commands on.
	Accounts []Account
	// Desktop is the desktop line the roster's entries that are on make:
	// the application servers on the person's computer and computer use;
	// nil for none. DesktopExec are the servers whose code tools the person
	// agreed to without being asked each time.
	Desktop     *manifest.Desktop
	DesktopExec []string
	// ImageServers are the built-in's image servers the roster has on and
	// the person signed in, each with the switches it has on and its
	// volume; their secret slots are among ServerSecrets.
	ImageServers []ImageGrant
	// CalendarFeeds are the Assistant's calendar feeds the roster has on,
	// each with its address, given to its tools server alone; CalendarZone
	// is the person's IANA zone, in which the calendar tools answer.
	CalendarFeeds []CalendarFeed
	CalendarZone  string
	// Voice is the person's Writing Voice, when the roster has it on: read
	// through voice_read as a run's is (engine.Params.Voice). The engine
	// refuses it to a built-in.
	Voice *engine.AttachedVoice
	// Web and Review are the keys of the web tools and the review grant,
	// for a package that declares them and whose roster has them on; the
	// plan has each exactly when it is given.
	Web    *engine.WebRun
	Review *engine.ReviewRun
}

// CalendarFeed is one of the Assistant's calendar feeds, as a segment is
// given it: its name and host, and the address read from the keychain, with
// its browser sign-in when it has one.
type CalendarFeed struct {
	Name, Host, URL string
	// Session is the feed's browser sign-in, or empty.
	Session string
}

// ImageGrant is one image server a segment is given: the built-in's server
// by its key (Builtin.ImageServers), the keys of its switches that are on,
// and the agent's volume it keeps its state on.
type ImageGrant struct {
	Key      string
	Switches []string
	Volume   string
	// Account is the signed-in account, given to the server in its
	// AccountEnv.
	Account string
	// Env is what the server is told of its sign-in (its client and
	// tenant), beside its plain env; Args are added after its command (the
	// person's signoff). Neither holds a secret.
	Env  map[string]string
	Args []string
	// AgentryApp says the server was signed in with Agentry's own app,
	// which is given only to the build's own image of the server.
	AgentryApp bool
}

// imageSpecs are the plan's specs of the image servers the segment is
// given, from the servers its package declares at the policy's images, and
// their volumes. A grant naming a server the package does not declare is
// refused.
func (s Segment) imageSpecs() ([]spec.MCPServerSpec, []string, error) {
	if len(s.Grants.ImageServers) == 0 {
		return nil, nil, nil
	}
	declared := s.declared()
	offered := ImageServersFor(declared, s.Policy.Images)
	builds := ImageServersFor(declared, policy.Images{})
	var specs []spec.MCPServerSpec
	var volumes []string
	for _, g := range s.Grants.ImageServers {
		i := slices.IndexFunc(offered, func(o offer.ImageServer) bool { return o.Key == g.Key })
		if i < 0 {
			return nil, nil, fmt.Errorf("the segment is given the image server %s, which %s does not offer", g.Key, s.ref())
		}
		o := offered[i]
		if g.AgentryApp {
			// A sign-in made with Agentry's own app reaches only the
			// server Agentry built: an image the policy names instead
			// would be handed a token minted under Agentry's app.
			if b := slices.IndexFunc(builds, func(o offer.ImageServer) bool { return o.Key == g.Key }); b < 0 || builds[b].Image != o.Image {
				return nil, nil, fmt.Errorf("the %s server was signed in with Agentry's own app, which is given only to Agentry's image of it, and the policy names %s instead: sign it in again with your own client, or use Agentry's image", o.Name, o.Image)
			}
		}
		env := maps.Clone(o.Env)
		if env == nil {
			env = map[string]string{}
		}
		if o.AccountEnv != "" && g.Account != "" {
			env[o.AccountEnv] = g.Account
		}
		maps.Copy(env, g.Env)
		if len(env) == 0 {
			env = nil
		}
		specs = append(specs, spec.MCPServerSpec{
			Name: "mcp-image-" + o.Key, Source: spec.SourceImage, Package: o.Image, Image: o.Image,
			Command: append(o.Args(func(key string) bool { return slices.Contains(g.Switches, key) }), g.Args...),
			Egress:  slices.Clone(o.Egress), Secrets: slices.Clone(o.Secrets), Env: env,
			Volumes: []string{g.Volume + ":" + o.VolumeTarget}, Account: g.Account,
		})
		volumes = append(volumes, g.Volume)
	}
	return specs, volumes, nil
}

// Account is one of the Coding agent's git host accounts, as a segment is
// given it: the account (its TokenEnv is the segment's to set) and the
// token read from the keychain.
type Account struct {
	spec.CoderAccount
	Token string
}

// Installed is a persistent release installed in the runner home, from the
// registry or from an author's folder: the record it was laid down with and
// its release folder. A segment runs it under the attestation its install
// recorded and its integrity record vouches for, never as built in, and with
// none of a built-in's grants.
type Installed struct {
	Meta *installdir.Meta
	Dir  string
}

// verify holds the installed release to the runner home's integrity record
// before a segment trusts it, and returns the record it was laid down with:
// a signed release from the registry, or an author's folder installed on
// this computer. Anything else is refused: a copy that changed, a record
// that claims to be built in, an unsigned or unscanned registry release.
func (r Installed) verify() (*installdir.Meta, error) {
	if r.Meta == nil || r.Dir == "" {
		return nil, engine.Classify(engine.ExitCLI, errors.New("an installed release with no record"))
	}
	meta, err := installdir.Inspect(r.Dir, r.Meta.Digest)
	var changed *installdir.ChangedError
	switch {
	case errors.As(err, &changed):
		changed.Ref = r.Meta.Ref
		return nil, engine.Classify(engine.ExitVerify, changed)
	case errors.Is(err, installdir.ErrNoRecord):
		return nil, engine.Classify(engine.ExitVerify, fmt.Errorf("%s has no integrity record from this runner home: install it again with agentry agent install", r.Meta.Ref))
	case err != nil:
		return nil, engine.Classify(engine.ExitVerify, fmt.Errorf("installed %s: %w", r.Meta.Ref, err))
	}
	if meta.Namespace != r.Meta.Namespace || meta.Name != r.Meta.Name || meta.Version != r.Meta.Version {
		return nil, engine.Classify(engine.ExitVerify, fmt.Errorf("the release folder of %s holds %s", r.Meta.Ref, meta.Ref))
	}
	switch {
	case meta.Source == "builtin" || meta.Attestation.State == trust.StateBuiltin:
		// Only the application's own packages are built in, and their
		// record is made in memory (Materialize).
		return nil, engine.Classify(engine.ExitVerify, fmt.Errorf("%s is recorded as built in, which only the application's own packages are: install it again with agentry agent install", meta.Ref))
	case meta.Source == installed.SourceLocal && meta.Attestation.State == trust.StateLocal:
		// The author's folder: unsigned, which its install agreed to.
		meta.Attestation.Consent = receipt.ConsentAllowUnsigned
	case meta.Source == "registry" && meta.Attestation.State == trust.StateVerified:
	default:
		return nil, engine.Classify(engine.ExitVerify, fmt.Errorf("%s is %s (source %s): a persistent agent runs a signed release from the registry or an author's folder installed on this computer", meta.Ref, meta.Attestation.State, meta.Source))
	}
	return meta, nil
}

// copyCheck holds the segment's copy of the release's workspace to the
// release folder's integrity record, before anything reads it.
func (r Installed) copyCheck(ref string) func(string) error {
	return func(copyDir string) error {
		err := installdir.CheckCopy(r.Dir, copyDir)
		var changed *installdir.ChangedError
		if errors.As(err, &changed) {
			changed.Ref = ref
			return changed
		}
		return err
	}
}

// Segment is one start of a persistent agent: the session a persistent
// agent's supervisor owns until it stops it. It runs either a persistent
// built-in (Builtin) or an installed release (Release), never both.
type Segment struct {
	Builtin Builtin
	// Release is the installed release the segment runs in place of a
	// built-in; nil for a built-in. Builtin is then the zero value, which
	// carries none of a built-in's grants.
	Release *Installed
	Home    string
	// Policy is the policy the segment compiles from: the view of it the
	// agent's saved model reads (policy.ForRun), whose saved model the
	// segment's plan and receipt name.
	Policy     *policy.Policy
	PolicySHA  string
	PolicyFile *policy.File
	// Adapter is the harness the segment runs on, an image's; empty takes
	// the one Policy's credential takes.
	Adapter string
	// Build is the client's build identity, recorded in the receipt's
	// attestation and the plan.
	Build string
	// OutDir is the segment's folder (its receipts and envelope), absolute.
	OutDir string
	RunID  string
	Stderr io.Writer
	// OnStage, when set, is called as the start reaches each of its steps
	// (engine.Stage), here and in the engine; nil does nothing.
	OnStage func(engine.Stage)
	// Persistent is the agent's part of the engine's parameters: its name,
	// the segment's number, its volumes and the conversation to continue.
	Persistent engine.PersistentSegment
	// Grants are what the agent's roster turns on (Apply).
	Grants Grants
	// Inference is the agent's Agentry credits session, when its policy
	// pays with credits (model provider agentry): the supervisor opens it
	// once for the agent's life and renews and ends it itself, so the
	// grant carries no Renew or End.
	Inference *engine.InferenceGrant
	// BindDesktop binds a segment the roster gives the person's computer
	// to Agentry Desktop: in the app's own process for an agent it
	// supervises; nil binds over the link socket in Home.
	BindDesktop engine.DesktopBinder
}

// Start starts the segment: materialize, validate, plan the persistent mode,
// ensure the images, start the session. A persistent agent always runs its
// harness in a container: a host adapter is replaced by its image
// counterpart, and a plan that would run the harness on the host is refused.
// It has no session caps and no tool-call or cost budget: the person stops
// it.
func (s Segment) Start(ctx context.Context, d dockerx.Client) (engine.PersistentSession, error) {
	logw := s.Stderr
	if logw == nil {
		logw = io.Discard
	}
	sp, err := s.Plan(ctx, logw)
	if err != nil {
		return nil, err
	}
	plan, meta, workspace, installDir, ad := sp.Plan, sp.meta, sp.workspace, sp.installDir, sp.adapter
	if signsIn(plan) {
		engine.ReportStage(s.OnStage, engine.Stage{Name: engine.StageSignIn})
	}
	signIns := modelsignin.Open(s.Home, connection.OSKeychain{})
	if err := signIns.ReadyForRun(ctx, plan); err != nil {
		return nil, engine.Classify(engine.ExitPolicy, err)
	}
	signIn, renewSignIn, err := engine.ModelSignInFor(ctx, signIns, plan, false)
	if err != nil {
		return nil, err
	}
	runnerTag := s.Policy.RunnerImageTag(runnerimage.ContentVersion())
	ctxDir := filepath.Join(s.Home, "image")
	engine.ReportStage(s.OnStage, engine.Stage{Name: receipt.StageImages})
	if err := gateway.EnsureInstalledFor(ctx, d, s.Policy, runnerTag, ctxDir, logw, plan); err != nil {
		return nil, engine.Classify(engine.ExitGateway, err)
	}
	var execPackages []string
	if plan.ExecImage != nil {
		if execPackages, err = gateway.EnsureExecImage(ctx, d, runnerTag, *plan.ExecImage, ctxDir, logw); err != nil {
			return nil, engine.Classify(engine.ExitGateway, err)
		}
	}
	if toInstall := sp.uninstalled(s.Grants.ServersDir); len(toInstall) > 0 {
		engine.ReportStage(s.OnStage, engine.Stage{Name: receipt.StageMCPServers, Subject: strings.Join(toInstall, ", ")})
	}
	if err := sp.installServers(ctx, d, s.Grants.ServersDir); err != nil {
		return nil, err
	}
	// The install can move the plan to one compiled with the commands it
	// found (InstallServers).
	plan, planDir := sp.Plan, sp.Dir
	seg := s.Persistent
	seg.Share, seg.AppLink, seg.Skills = s.Grants.Share, s.Grants.AppLink, s.Grants.SkillsDir
	if _, seg.ServerVolumes, err = s.imageSpecs(); err != nil {
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	// The Coding agent's dependency scan reads OSV's databases under the
	// runner home, which its supervisor keeps (osvdb.Ensure).
	if g := s.Grants.Coder; g != nil && g.Commands && g.DependencyScan {
		seg.DependencyDB = osvdb.Dir(s.Home)
		if err := os.MkdirAll(seg.DependencyDB, 0o755); err != nil { //nolint:gosec // read by the scan, which runs as the person
			return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("the dependency databases' folder: %w", err))
		}
	}
	// The person's computer: bound lazily, so an application that is not
	// running is read at each call, and a segment starts without Agentry
	// Desktop answering. The engine unbinds at teardown.
	var desktopLink *engine.DesktopLink
	if d := plan.Capabilities.Desktop; d != nil {
		if desktopLink, err = s.bindDesktop(ctx, plan); err != nil {
			return nil, err
		}
	}
	var answers *intake.Answers
	if h := s.Grants.DevHost; h != nil {
		answers = &intake.Answers{Hosts: map[string]*targethost.Host{spec.DevHostID: h}}
	}
	sess, err := engine.StartSession(ctx, d, engine.Params{
		Intake: answers,
		Plan:   plan, PlanDir: planDir, WorkspaceSrc: workspace, RunRoot: installDir, RunnerHome: s.Home,
		OutDir: s.OutDir, TextInputs: true, Knowledge: sp.knowledge, ServersDir: s.Grants.ServersDir,
		Adapter: ad, Policy: s.Policy, PolicyFile: s.PolicyFile, Budgets: spec.Budgets{},
		Attestation: meta.Attestation, ExecPackages: execPackages,
		CheckWorkspace: s.copyCheck(meta),
		RunID:          s.RunID, Stderr: logw, OnStage: s.OnStage,
		ModelSignIn: signIn, RenewModelSignIn: renewSignIn, Inference: s.Inference,
		SecretOverrides: s.secretOverrides(), WithheldSecrets: slices.Clone(s.Grants.WithheldSecrets),
		AccountTokens: s.accountTokens(), CalendarURLs: s.calendarURLs(),
		Voice: s.Grants.Voice, Web: s.Grants.Web, Review: s.Grants.Review,
		Desktop:    desktopLink,
		Persistent: &seg,
	})
	if err != nil {
		return nil, err
	}
	ps, ok := sess.(engine.PersistentSession)
	if !ok {
		_, _ = sess.End(context.WithoutCancel(ctx), false)
		return nil, engine.Classify(engine.ExitCLI, errors.New("the engine's session is not a persistent agent's segment"))
	}
	return ps, nil
}

// declared is what the segment's package declares under
// capabilities.persistent, read before its start holds the release to its
// integrity record, so a grant it does not declare is refused at once; the
// compiler holds the plan to the verified manifest's block again.
func (s Segment) declared() *manifest.Persistent {
	if s.Release == nil {
		return s.Builtin.Declared()
	}
	m, err := ManifestFrom(filepath.Join(s.Release.Dir, "workspace"))
	if err != nil {
		return nil
	}
	return m.Capabilities.Persistent
}

// grantedBuiltin is the built-in whose by-name grants the segment's plan
// may carry: its Builtin when it runs one, and the zero Builtin, granted
// none, when it runs a release.
func (s Segment) grantedBuiltin() Builtin {
	if s.Release != nil {
		return Builtin{}
	}
	return s.Builtin
}

// ref is the package the segment runs, as ns/name@version.
func (s Segment) ref() string {
	if s.Release != nil && s.Release.Meta != nil {
		return s.Release.Meta.Ref
	}
	return s.Builtin.Ref()
}

// copyCheck is the engine's check of the segment's copy of the package's
// workspace: against the embedded tree for a built-in, against the release
// folder's integrity record for an installed release.
func (s Segment) copyCheck(meta *installdir.Meta) func(string) error {
	if s.Release != nil {
		return s.Release.copyCheck(meta.Ref)
	}
	return builtinCopyCheck(meta.Digest)
}

// materialize is the package the segment runs, laid down in the runner home,
// with its record: the embedded built-in, or the installed release held to
// its integrity record.
func (s Segment) materialize() (*installdir.Meta, string, error) {
	if s.Release != nil {
		if s.Builtin.fsys != nil {
			return nil, "", engine.Classify(engine.ExitCLI, errors.New("a segment runs a built-in or an installed release, not both"))
		}
		meta, err := s.Release.verify()
		if err != nil {
			return nil, "", err
		}
		return meta, s.Release.Dir, nil
	}
	if !s.Builtin.persistent || !s.Builtin.runnable {
		return nil, "", engine.Classify(engine.ExitCLI, fmt.Errorf("%s is not a persistent agent Agentry runs", s.Builtin.Ref()))
	}
	meta, dir, err := s.Builtin.Materialize(s.Home, s.Build)
	if err != nil {
		return nil, "", engine.Classify(engine.ExitCLI, err)
	}
	return meta, dir, nil
}

// knowledge is the segment's packs: the ones the package ships in its own
// tree, read from the copy materialize just held to its record, before the
// ones the person's roster attached. A pack of the person's that takes a
// shipped pack's name is refused.
func (s Segment) knowledge(workspace string, m *manifest.Manifest, meta *installdir.Meta) ([]engine.AttachedPack, error) {
	shipped, err := engine.ShippedPacks(workspace, m, "release "+meta.Ref+" ("+meta.Digest+")")
	if err != nil {
		return nil, engine.Classify(engine.ExitCompile, err)
	}
	packs, err := engine.WithShipped(shipped, s.Grants.Knowledge)
	if name, taken := engine.IsPackNameTaken(err); taken {
		return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("the knowledge pack %s: %s ships a knowledge pack of that name; remove yours from the agent or give it another name", name, meta.Ref))
	}
	return packs, err
}

// ImageAdapter is the adapter a persistent agent runs on for the machine's
// adapter: the same harness in a container, for one that would run it on the
// host.
func ImageAdapter(name string) string {
	switch name {
	case adapter.ClaudeAdapterName:
		return adapter.ClaudeImageAdapterName
	case adapter.CodexAdapterName:
		return adapter.CodexImageAdapterName
	}
	return name
}

// appLinkSet is the app link tool set the segment's plan has: the one the
// package's declared capabilities compose (compile.AppLinkSet), with the
// parts a built-in is granted by name, when the segment is given a socket
// for it. A release's segment carries the zero Builtin, granted none.
func (s Segment) appLinkSet(m *manifest.Manifest) string {
	if s.Grants.AppLink == "" {
		return ""
	}
	set := s.grantedBuiltin().WithGrantedParts(compile.AppLinkSet(m.Capabilities.Persistent))
	if s.Grants.Subagents {
		set = applink.Compose(append(applink.SetParts(set), applink.PartSubagents)...)
	}
	return set
}

// coderGrant is the Coding agent's grant the segment's plan has: the
// roster's, for the Coding agent alone, with its dev server readied exactly
// when the grant names it.
func (s Segment) coderGrant() (*spec.CoderGrant, error) {
	g := s.Grants.Coder
	switch {
	case g == nil && s.Grants.DevHost == nil:
		return nil, nil
	case !s.declared().Declares("coder"):
		return nil, engine.Classify(engine.ExitPolicy, fmt.Errorf("%s is not given the coding toolchain: it does not declare capabilities.persistent.coder", s.ref()))
	case g == nil || g.DevHost != (s.Grants.DevHost != nil):
		return nil, engine.Classify(engine.ExitCLI, errors.New("the Coding agent's dev server is given exactly when its roster has it on, readied"))
	}
	out := *g
	// The stored secrets that are on reach its programs by name; their
	// values are the segment's (Start).
	out.Secrets = slices.Sorted(maps.Keys(s.Grants.Secrets))
	// So do the git host accounts, by host, each token under a variable of
	// its own (accountTokens).
	out.Accounts = nil
	for i, a := range s.sortedAccounts() {
		acct := a.CoderAccount
		acct.TokenEnv = spec.AccountTokenEnv(i)
		out.Accounts = append(out.Accounts, acct)
	}
	return &out, nil
}

// secretOverrides are the values of the segment's declared secrets: the
// stored secrets that are on, and the offered servers' account tokens.
func (s Segment) secretOverrides() map[string]string {
	out := maps.Clone(s.Grants.Secrets)
	if len(s.Grants.ServerSecrets) > 0 && out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, s.Grants.ServerSecrets)
	return out
}

// sortedAccounts are the segment's git host accounts by host.
func (s Segment) sortedAccounts() []Account {
	out := slices.Clone(s.Grants.Accounts)
	slices.SortFunc(out, func(a, b Account) int { return strings.Compare(a.Host, b.Host) })
	return out
}

// accountTokens are the git host accounts' tokens by the variables
// coderGrant gave them.
func (s Segment) accountTokens() map[string]string {
	if len(s.Grants.Accounts) == 0 {
		return nil
	}
	out := map[string]string{}
	for i, a := range s.sortedAccounts() {
		out[spec.AccountTokenEnv(i)] = a.Token
	}
	return out
}

// sortedCalendarFeeds are the segment's calendar feeds by name.
func (s Segment) sortedCalendarFeeds() []CalendarFeed {
	out := slices.Clone(s.Grants.CalendarFeeds)
	slices.SortFunc(out, func(a, b CalendarFeed) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// calendarGrant is the Assistant's calendar grant the segment's plan has:
// its feeds by name, each address under a variable of its own
// (calendarURLs), and the person's zone.
func (s Segment) calendarGrant() (*spec.CalendarGrant, error) {
	if len(s.Grants.CalendarFeeds) == 0 {
		return nil, nil
	}
	if !s.declared().Declares("calendars") {
		return nil, engine.Classify(engine.ExitPolicy, fmt.Errorf("%s is not given calendar feeds: it does not declare capabilities.persistent.calendars", s.ref()))
	}
	g := &spec.CalendarGrant{TimeZone: s.Grants.CalendarZone}
	for i, f := range s.sortedCalendarFeeds() {
		feed := spec.CalendarFeed{Name: f.Name, Host: f.Host, URLEnv: spec.CalendarFeedEnv(i)}
		if f.Session != "" {
			feed.SessionEnv = spec.CalendarSessionEnv(i)
		}
		g.Feeds = append(g.Feeds, feed)
	}
	if err := spec.CheckCalendarGrant(g); err != nil {
		return nil, engine.Classify(engine.ExitPolicy, err)
	}
	return g, nil
}

// calendarURLs are the calendar feeds' addresses and browser sign-ins by
// the variables calendarGrant gave them.
func (s Segment) calendarURLs() map[string]string {
	if len(s.Grants.CalendarFeeds) == 0 {
		return nil
	}
	out := map[string]string{}
	for i, f := range s.sortedCalendarFeeds() {
		out[spec.CalendarFeedEnv(i)] = f.URL
		if f.Session != "" {
			out[spec.CalendarSessionEnv(i)] = f.Session
		}
	}
	return out
}

// checkSecrets refuses stored secrets for a package that declares none, and
// git host accounts for any but the Coding agent with its commands on.
func (s Segment) checkSecrets(m *manifest.Manifest) error {
	if (len(s.Grants.Secrets) > 0 || len(s.Grants.WithheldSecrets) > 0) && !m.Capabilities.Persistent.Declares("secrets") {
		return engine.Classify(engine.ExitPolicy, fmt.Errorf("%s is not given stored secrets: it does not declare capabilities.persistent.secrets", s.ref()))
	}
	if len(s.Grants.Accounts) > 0 && (!m.Capabilities.Persistent.Declares("coder") || s.Grants.Coder == nil || !s.Grants.Coder.Commands) {
		return engine.Classify(engine.ExitPolicy, fmt.Errorf("%s is not given git host accounts: they come with the coding toolchain, for the programs it runs", s.ref()))
	}
	return nil
}

// agentFiles are the folders the segment's plan mounts: the data volume,
// the share when the roster has it on, and the memory volume when the
// package declares a memory.
func (s Segment) agentFiles(m *manifest.Manifest) *spec.AgentFiles {
	return &spec.AgentFiles{Share: s.Grants.Share != "", CheckShare: s.Grants.Share != "" && s.checksShare(), Skills: s.Grants.Skills, Memory: m.Capabilities.Persistent.Declares("memory")}
}

// checksShare says a file of the segment's share is read only once it was
// checked: a built-in's as it declares, an installed release's always, so a
// publisher's agent reads its share as the Assistant does.
func (s Segment) checksShare() bool {
	return s.Release != nil || s.Builtin.checkShare
}

// shareServers are the servers the segment's plan lets see the share: the
// roster's, when the share is on.
func (s Segment) shareServers() []string {
	if s.Grants.Share == "" || len(s.Grants.ShareServers) == 0 {
		return nil
	}
	return slices.Clone(s.Grants.ShareServers)
}

// bindDesktop binds the segment to Agentry Desktop for its plan's desktop
// line, each server's exec agreement as the roster has it.
func (s Segment) bindDesktop(ctx context.Context, plan *spec.Plan) (*engine.DesktopLink, error) {
	d := plan.Capabilities.Desktop
	req := engine.DesktopBindRequest(d, s.RunID, plan.Ref, plan.Digest, s.OutDir)
	req.Agent = s.Persistent.Agent
	for i := range req.Servers {
		req.Servers[i].ExecAgreed = slices.Contains(s.Grants.DesktopExec, req.Servers[i].ID)
	}
	bind := s.BindDesktop
	if bind == nil {
		bind = engine.SocketDesktopBinder(desktop.SocketPath(s.Home))
	}
	return engine.PersistentDesktopLink(ctx, bind, req, d)
}

// withDesktop is the manifest with the desktop line the roster's entries
// that are on make, in place of its own: the release's line, when it has
// one, only narrows what the roster offers.
func (s Segment) withDesktop(m *manifest.Manifest) *manifest.Manifest {
	out := *m
	out.Capabilities.Desktop = s.Grants.Desktop
	return &out
}

// withServers is the built-in's manifest with the servers the person added
// and has on declared beside its own, so they compile, are consented and
// are held as a release's declared servers are. The built-in's own files
// are unchanged: the segment's receipt names what the roster added.
func (s Segment) withServers(m *manifest.Manifest) *manifest.Manifest {
	if len(s.Grants.Servers) == 0 {
		return m
	}
	out := *m
	out.Capabilities.MCPServers = append(append([]manifest.MCPServer(nil), m.Capabilities.MCPServers...), s.Grants.Servers...)
	return &out
}

// checkManifest refuses a segment whose release, as it ships, the
// manifest's rules refuse, or whose manifest with the roster's servers
// declared beside the release's own they refuse. A stored secret is given
// by name to every server that declares it (secretOverrides), so a roster
// server that declares one is how that secret reaches the server, not a
// secret the release asks for at each run: that one V16 finding is the
// release's alone, and the first check has it.
func (s Segment) checkManifest(ref string, m *manifest.Manifest) error {
	if err := manifestInvalid(ref, manifest.Validate(m)); err != nil || len(s.Grants.Servers) == 0 {
		return err
	}
	stored := map[string]bool{}
	if p := m.Capabilities.Persistent; p != nil {
		for _, srv := range s.Grants.Servers {
			for _, name := range srv.Secrets {
				if i := slices.Index(p.Secrets, name); i >= 0 {
					stored[fmt.Sprintf("capabilities.persistent.secrets[%d]", i)] = true
				}
			}
		}
	}
	var errs []error
	for _, e := range manifest.Validate(s.withServers(m)) {
		if is, ok := manifest.AsIssue(e); ok && is.Rule == "V16" && stored[is.Path] {
			continue
		}
		errs = append(errs, e)
	}
	return manifestInvalid(ref, errs)
}

// manifestInvalid is the start's refusal of a manifest with blocking
// findings among errs; nil when it has none.
func manifestInvalid(ref string, errs []error) error {
	blocking := manifest.Blocking(errs)
	if len(blocking) == 0 {
		return nil
	}
	msgs := make([]string, 0, len(blocking))
	for _, e := range blocking {
		msgs = append(msgs, e.Error())
	}
	return engine.Classify(engine.ExitCompile, fmt.Errorf("%s manifest invalid:\n  %s", ref, strings.Join(msgs, "\n  ")))
}

// resolveServers resolves the package servers among the manifest's, from
// the agent's store when it already holds each pin, and returns them with
// the plan's server specs.
func (s Segment) resolveServers(ctx context.Context, m *manifest.Manifest, logw io.Writer) ([]mcpinstall.Resolved, []spec.MCPServerSpec, error) {
	if len(m.Capabilities.MCPServers) == 0 {
		return nil, nil, nil
	}
	if len(mcpinstall.Packages(m.Capabilities.MCPServers)) > 0 && s.Grants.ServersDir == "" {
		return nil, nil, engine.Classify(engine.ExitCLI, errors.New("the agent has a package server on, and no store to install it into"))
	}
	resolved, err := mcpinstall.StoredOrResolved(ctx, logw, s.Grants.ServersDir, s.Policy.MCPRegistries, s.Policy.Images, m.Capabilities.MCPServers)
	if err != nil {
		return nil, nil, engine.Classify(engine.ExitVerify, err)
	}
	return resolved, mcpinstall.Specs(resolved, m.Capabilities.MCPServers, s.Policy.Images), nil
}

// SegmentPlan is a segment's compiled plan and what starting it needs.
type SegmentPlan struct {
	Plan *spec.Plan
	// Dir is the plan's folder: its gateway and harness files.
	Dir        string
	meta       *installdir.Meta
	workspace  string
	installDir string
	resolved   []mcpinstall.Resolved
	adapter    adapter.Adapter
	// inputs are what Plan was compiled from: the install compiles it
	// again from them when it changes a server's command.
	inputs PlanInputs
	// knowledge is the segment's packs, the package's shipped ones first.
	knowledge []engine.AttachedPack
}

// uninstalled names the segment's package servers the agent's store does
// not hold yet, which the start installs; a server it holds is reused at
// once and is not named.
func (sp *SegmentPlan) uninstalled(storeDir string) []string {
	var out []string
	for _, r := range sp.resolved {
		if storeDir != "" {
			if prior, err := mcpinstall.LoadMeta(storeDir, r.ID); err == nil && prior != nil {
				continue
			}
		}
		out = append(out, r.Package)
	}
	return out
}

// signsIn says the plan's model is reached through a subscription's sign-in,
// which the start takes from the store and readies before the session.
func signsIn(plan *spec.Plan) bool { return plan.Harness.ModelSignIn != nil }

// installServers installs the segment's package servers into the agent's
// store and moves Plan and Dir to the plan compiled with the commands the
// install found (InstallServers).
func (sp *SegmentPlan) installServers(ctx context.Context, d dockerx.Client, storeDir string) error {
	plan, dir, err := InstallServers(ctx, d, sp.inputs, sp.Plan, sp.Dir, sp.resolved, storeDir)
	if err != nil {
		return err
	}
	sp.Plan, sp.Dir = plan, dir
	return nil
}

// InstallServer installs one package server into a persistent agent's store
// outside any segment's start: resolved from the store when it already holds
// the pin, else from the policy's registries, then installed as a segment's
// start installs it (mcpinstall.EnsureInstalled), whose record carries the
// command the install found for the next segment's plan. The runner image
// must be built, as a segment's start leaves it; the caller holds the
// server's store lock.
func InstallServer(ctx context.Context, d dockerx.Client, pol *policy.Policy, storeDir string, srv manifest.MCPServer, logw io.Writer) error {
	if logw == nil {
		logw = io.Discard
	}
	resolved, err := mcpinstall.StoredOrResolved(ctx, logw, storeDir, pol.MCPRegistries, pol.Images, []manifest.MCPServer{srv})
	if err != nil {
		return engine.Classify(engine.ExitVerify, err)
	}
	if err := mcpinstall.EnsureInstalled(ctx, d, resolved, storeDir, pol.MCPRegistries, pol.Images, pol.Docker, logw); err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	return nil
}

// Plan materializes the built-in and compiles the segment's plan from it and
// the roster's grants: the servers the person has on are declared beside the
// built-in's own, and its folders are mounted, the share when it is on. It
// touches no container engine; a package server is resolved from the agent's
// store, or from the registries when the store does not hold its pin.
func (s Segment) Plan(ctx context.Context, logw io.Writer) (*SegmentPlan, error) {
	engine.ReportStage(s.OnStage, engine.Stage{Name: receipt.StageCompile})
	coderGrant, err := s.coderGrant()
	if err != nil {
		return nil, err
	}
	calendarGrant, err := s.calendarGrant()
	if err != nil {
		return nil, err
	}
	meta, installDir, err := s.materialize()
	if err != nil {
		return nil, err
	}
	workspace := filepath.Join(installDir, "workspace")
	m, err := ManifestFrom(workspace)
	if err != nil {
		return nil, engine.Classify(engine.ExitCompile, err)
	}
	if !m.Supports(manifest.ModePersistent) {
		return nil, engine.Classify(engine.ExitCompile, fmt.Errorf("%s does not declare the persistent mode", meta.Ref))
	}
	if err := s.checkSecrets(m); err != nil {
		return nil, err
	}
	knowledge, err := s.knowledge(workspace, m, meta)
	if err != nil {
		return nil, err
	}
	if err := s.checkManifest(meta.Ref, m); err != nil {
		return nil, err
	}
	m = s.withDesktop(s.withServers(m))
	adapterName := cmp.Or(s.Adapter, ImageAdapter(adapter.DefaultFor(s.Policy)))
	ad, err := adapter.Get(adapterName)
	if err != nil {
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	resolved, servers, err := s.resolveServers(ctx, m, logw)
	if err != nil {
		return nil, err
	}
	images, _, err := s.imageSpecs()
	if err != nil {
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	servers = append(servers, images...)
	in := PlanInputs{
		Home: s.Home, Manifest: m, Meta: meta, InstallDir: installDir, WorkspaceDir: workspace,
		Policy: s.Policy, PolicySHA: s.PolicySHA, Adapter: adapterName, Mode: manifest.ModePersistent,
		Servers: servers, AgentFiles: s.agentFiles(m), AppLink: s.appLinkSet(m), Subagents: s.Grants.Subagents, Publisher: s.grantedBuiltin().ManagesQuickAgents(), Coder: coderGrant, Calendar: calendarGrant, ShareServers: s.shareServers(), CompiledBy: s.Build, Log: logw,
		Web: s.Grants.Web != nil, Review: s.Grants.Review != nil,
	}
	plan, planDir, err := EnsurePlan(in)
	if err != nil {
		return nil, err
	}
	if plan.Harness.Host != nil {
		return nil, engine.Classify(engine.ExitPolicy, errors.New("a persistent agent runs its harness in a container, and this plan runs it on the host"))
	}
	return &SegmentPlan{Plan: plan, Dir: planDir, meta: meta, workspace: workspace, installDir: installDir, resolved: resolved, adapter: ad, inputs: in, knowledge: knowledge}, nil
}

// PersistentImageServer is the key of the image server this build runs for
// name, an entry of capabilities.persistent.imageServers; false for a name
// it does not carry (compile.PersistentImageServer).
func PersistentImageServer(name string) (string, bool) { return compile.PersistentImageServer(name) }

// PersistentMailProvider is the key of the mail provider this build reaches
// for name, an entry of capabilities.persistent.mail; false for a name it
// does not carry.
func PersistentMailProvider(name string) (string, bool) {
	for _, p := range assistant.MailProviders() {
		if p.Key == name {
			return p.Key, true
		}
	}
	return "", false
}

// CheckPersistentNames refuses a capabilities.persistent block that names an
// image server or a mail provider this build does not carry: a release
// written for a newer Agentry is refused, never run without what it asks.
func CheckPersistentNames(p *manifest.Persistent) error {
	if p == nil {
		return nil
	}
	for _, n := range p.ImageServers {
		if _, ok := PersistentImageServer(n); !ok {
			return fmt.Errorf("it asks for the image server %q, which this version of Agentry does not run: update Agentry", n)
		}
	}
	for _, n := range p.Mail {
		if _, ok := PersistentMailProvider(n); !ok {
			return fmt.Errorf("it asks for mail at %q, which this version of Agentry does not reach: update Agentry", n)
		}
	}
	return checkNetworkShapes(p)
}

// coderAloneBeside are the keys whose grants put the person's keys or feed
// addresses in the agent's tools container. They sit beside one another,
// and never beside coder, whose programs run in that container.
var coderAloneBeside = []string{"web", "review", "calendars"}

// checkNetworkShapes refuses a block that declares coder beside a key of
// coderAloneBeside: its segments could never be given both.
func checkNetworkShapes(p *manifest.Persistent) error {
	if !p.Declares("coder") {
		return nil
	}
	var beside []string
	for _, k := range coderAloneBeside {
		if p.Declares(k) {
			beside = append(beside, k)
		}
	}
	if len(beside) > 0 {
		return fmt.Errorf("it declares coder beside %s, which Agentry never gives together: the coding toolchain's programs run in the tools container, which would hold the person's keys and feed addresses", strings.Join(beside, " and "))
	}
	return nil
}
