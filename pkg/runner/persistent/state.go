// Package persistent runs a persistent agent: a built-in QuickAgent the
// runner keeps between the person's conversations. Each start-to-stop is a
// segment, an ordinary session of the engine with its own run id, receipts
// and envelope; what outlives a segment is the agent's state folder under
// the runner home and its two volumes, which carry no run label, so neither a
// segment's teardown nor the sweep of a dead run removes them.
//
// One process owns a running agent, its supervisor: the desktop in-process,
// or the detached process `agentry agent start` launches. The supervisor
// holds the agent's owner lock and serves its owner socket, through which a
// chat in another process attaches, sends turns, reads the status and stops
// it. A second supervisor is refused, naming the holder.
package persistent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// Agent is one persistent agent this build knows.
type Agent struct {
	// Name is the agent's name, its built-in's name: what the commands and
	// the state folder are keyed by.
	Name string
	// Title is how the agent is named to a person until they name it.
	Title string
	// DesktopOnly says the agent runs only in Agentry Desktop; the CLI
	// lists it and refuses to start, chat with or configure it.
	DesktopOnly bool
	// Builtin is the package it runs.
	Builtin localrun.Builtin
	// ShareName is the default share folder's name, under ~/agentry.
	ShareName string
}

// Agents are the persistent agents this build knows, in the order they are
// listed.
func Agents() []Agent {
	return []Agent{
		{Name: localrun.Main().Name(), Title: "Agentry Helper", DesktopOnly: true, Builtin: localrun.Main(), ShareName: "mainshare"},
		{Name: localrun.Assistant().Name(), Title: "Assistant", Builtin: localrun.Assistant(), ShareName: "assistantshare"},
		{Name: localrun.Coder().Name(), Title: "Coding agent", Builtin: localrun.Coder(), ShareName: "codingshare"},
	}
}

// Lookup is the agent named name.
func Lookup(name string) (Agent, error) {
	var names []string
	for _, a := range Agents() {
		if a.Name == name {
			return a, nil
		}
		names = append(names, a.Name)
	}
	return Agent{}, fmt.Errorf("no persistent agent named %q (the agents are %s)", name, strings.Join(names, ", "))
}

// DisplayName is the name the agent goes by: the person's for it, else its
// title.
func (a Agent) DisplayName(c Config) string {
	if n := strings.TrimSpace(c.DisplayName); n != "" {
		return n
	}
	return a.Title
}

// Dir is the agent's state folder under the runner home.
func Dir(home, name string) string { return filepath.Join(home, "persistent", name) }

// SocketPath is the agent's owner socket. It lives beside the runs' sockets
// under the runner home rather than in the state folder, so its path stays
// short of the kernel's limit for a socket's name.
func SocketPath(home, name string) string { return filepath.Join(home, "s", "agent-"+name+".sock") }

// HomeID identifies a runner home among the homes that use one container
// engine: a short hash of its absolute path, links followed. The volumes of
// the agent a home runs carry it in their names and a label, so two homes
// (two people on one computer, a scratch home beside the person's own) never
// share a conversation or a data volume. A home moved to another path is a
// different home to the engine: its agents start on volumes of their own.
func HomeID(home string) string {
	p, err := filepath.Abs(home)
	if err != nil {
		p = filepath.Clean(home)
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		// These file systems are case-insensitive by default.
		p = strings.ToLower(p)
	}
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:])[:10]
}

// SessionVolumeIn is the agent's volume for the harness's conversation store
// in the runner home home, and DataVolumeIn the agent's volume for its own
// files.
func SessionVolumeIn(home, name string) string {
	return "agentry-persistent-" + name + "-" + HomeID(home) + "-session"
}

// DataVolumeIn is the agent's volume for its own files in the runner home
// home.
func DataVolumeIn(home, name string) string {
	return "agentry-persistent-" + name + "-" + HomeID(home) + "-data"
}

// SessionVolume and DataVolume are the legacy names of the agent's two
// volumes, by the agent alone: every runner home on one engine could use the
// same two. A home that has run the agent keeps its conversation by
// copying them into its own volumes at its next start, and leaves them be.
func SessionVolume(name string) string { return "agentry-persistent-" + name + "-session" }

// DataVolume is the agent's data volume under the earlier name.
func DataVolume(name string) string { return "agentry-persistent-" + name + "-data" }

// SharedVolumes are the volumes of the agent that exist under the legacy names
// (SessionVolume, DataVolume), which every runner home on the engine could
// use. A reset leaves them, as another home may still
// need them; nothing else removes them. A failed listing answers none.
func SharedVolumes(ctx context.Context, d dockerx.Client, name string) []string {
	have, err := d.VolumesByLabel(ctx, engine.PersistentLabel+"="+name)
	if err != nil {
		return nil
	}
	var out []string
	for _, v := range []string{SessionVolume(name), DataVolume(name)} {
		if slices.Contains(have, v) {
			out = append(out, v)
		}
	}
	return out
}

// The files of the state folder.
const (
	configFile     = "config.yaml"
	transcriptFile = "transcript.ndjson"
	usageFile      = "usage.json"
	lockFile       = "owner.lock"
	holderFile     = "owner.json"
	logFile        = "supervisor.log"
	segmentsDir    = "segments"
	// shareVerdictsFile keeps the share's malware verdicts between
	// segments.
	shareVerdictsFile = "share-verdicts.json"
	// shareTextDir keeps the text read out of the share's documents
	// between segments, by the digest each verdict is for.
	shareTextDir = "share-text"
	// shareMirrorDir is the share's mirror the MCP servers the person let
	// see the share mount, with its index and staging folder beside it
	// (engine.PersistentSegment.ShareMirror).
	shareMirrorDir = "share-mirror"
)

// shareMirror is the agent's share mirror folder.
func shareMirror(home, name string) string { return filepath.Join(Dir(home, name), shareMirrorDir) }

// removeShareMirror removes the share mirror, with what the engine keeps
// beside it: a segment none of whose servers sees it keeps no copy of the
// person's files.
func removeShareMirror(home, name string) error {
	dir := shareMirror(home, name)
	staging, index := engine.ShareMirrorBeside(dir)
	for _, p := range []string{dir, staging, index} {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("remove the %s agent's share mirror: %w", name, err)
		}
	}
	return nil
}

// LogPath is where a detached supervisor writes what it prints.
func LogPath(home, name string) string { return filepath.Join(Dir(home, name), logFile) }

// ConfigVersion is the version of config.yaml this build writes. Version 2
// added the share folder, the person's servers and packs, the switches and
// the consents, version 3 the Coding agent's dev server, version 4 its
// published ports, version 5 the skills the person added, version 6 the
// secrets they stored, version 7 the Coding agent's git host accounts and
// version 8 the sign-ins of the servers it runs from images; a
// build that reads an older version alone refuses a newer one rather than
// drop what it does not know on its next save.
const ConfigVersion = 8

// Config is the agent's config.yaml: what the person chose for it, and the
// conversation its next segment continues.
type Config struct {
	Version int `yaml:"version" json:"version"`
	// Enabled says the agent exists for the person: it may be started, and
	// a client lists it as theirs. Disabling it keeps its volumes.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// RunOnStart starts the agent when its client starts.
	RunOnStart bool `yaml:"runOnStart" json:"runOnStart"`
	// DisplayName and Personality are the person's for the agent.
	DisplayName string `yaml:"displayName,omitempty" json:"displayName,omitempty"`
	Personality string `yaml:"personality,omitempty" json:"personality,omitempty"`
	// Conversation is the harness's conversation the last segment was in.
	Conversation *Conversation `yaml:"conversation,omitempty" json:"conversation,omitempty"`
	// Segments counts the segments started.
	Segments int `yaml:"segments" json:"segments"`
	// Share is the share folder, absolute; empty is the agent's default,
	// ~/agentry/<ShareName>.
	Share string `yaml:"share,omitempty" json:"share,omitempty"`
	// Servers are the MCP servers the person added, in the manifest's own
	// shape; each is a roster entry.
	Servers []manifest.MCPServer `yaml:"servers,omitempty" json:"servers,omitempty"`
	// Knowledge names the knowledge packs the person added, from the local
	// store; each is a roster entry.
	Knowledge []string `yaml:"knowledge,omitempty" json:"knowledge,omitempty"`
	// Switches are the roster's switches the person set, by entry id; an
	// entry with none takes its default.
	Switches map[string]bool `yaml:"switches,omitempty" json:"switches,omitempty"`
	// Consented records, by entry id, the line the person consented to
	// when they turned the entry on: turning on an entry whose line differs
	// from it, or that has none, asks again.
	Consented map[string]string `yaml:"consented,omitempty" json:"consented,omitempty"`
	// PersonaGiven is the digest of the name and personality the agent was
	// last given, so a change reaches it at the next turn.
	PersonaGiven string `yaml:"personaGiven,omitempty" json:"personaGiven,omitempty"`
	// DevHost is the Coding agent's dev server, which the person saved; it
	// is a roster entry.
	DevHost *DevHost `yaml:"devHost,omitempty" json:"devHost,omitempty"`
	// Ports are the Coding agent's published ports, which the person
	// named; each is a roster entry.
	Ports []Port `yaml:"ports,omitempty" json:"ports,omitempty"`
	// ServerFailures are the servers the person added whose install failed,
	// by roster id: each stays failed, left out of every segment, until the
	// person switches it or removes it. A build that does not know the field
	// drops it on its next save, and the server is installed again at the
	// next start, so the field needs no version of its own.
	ServerFailures map[string]ServerFailure `yaml:"serverFailures,omitempty" json:"serverFailures,omitempty"`
	// Skills are the skills the person added, as they were checked; each is
	// a roster entry, its files in the agent's skills-added folder.
	Skills []AddedSkill `yaml:"skills,omitempty" json:"skills,omitempty"`
	// Secrets are the secrets the person stored for the agent, by name;
	// each is a roster entry, its value in the keychain (SecretAccount).
	Secrets []StoredSecret `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	// Accounts are the Coding agent's git host accounts, by host; each is a
	// roster entry, its token in the keychain (AccountKeychainAccount).
	Accounts []GitAccount `yaml:"accounts,omitempty" json:"accounts,omitempty"`
	// SignIns are the sign-ins of the agent's image servers, by the
	// server's key: who the server answers as once its own sign-in
	// finished, never a credential.
	SignIns map[string]ImageSignIn `yaml:"signIns,omitempty" json:"signIns,omitempty"`
}

// ServerFailure is why a server the person added could not be installed.
type ServerFailure struct {
	// Reason is the install's error and the builder's last lines, cut.
	Reason string    `yaml:"reason" json:"reason"`
	At     time.Time `yaml:"at" json:"at"`
	// Told says the agent was told, at a turn, that it does not have the
	// server.
	Told bool `yaml:"told,omitempty" json:"told,omitempty"`
}

// Conversation names a harness's stored conversation: the adapter that
// stored it, since another harness cannot read it, and its id.
type Conversation struct {
	Adapter string `yaml:"adapter" json:"adapter"`
	ID      string `yaml:"id" json:"id"`
}

// LoadConfig reads the agent's config.yaml; an agent never configured has
// the zero Config at this version.
func LoadConfig(home, name string) (Config, error) {
	b, err := os.ReadFile(filepath.Join(Dir(home, name), configFile)) //nolint:gosec // the runner home's own file
	if errors.Is(err, fs.ErrNotExist) {
		return Config{Version: ConfigVersion}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read the %s agent's configuration: %w", name, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("read the %s agent's configuration: %w", name, err)
	}
	if c.Version > ConfigVersion {
		return Config{}, fmt.Errorf("the %s agent's configuration is version %d, written by a newer Agentry than this one (version %d): update Agentry", name, c.Version, ConfigVersion)
	}
	c.Version = ConfigVersion
	return c, nil
}

// SaveConfig writes the agent's config.yaml (0600), replacing it whole.
func SaveConfig(home, name string, c Config) error {
	// Version 3 only when the config holds what version 2 lacks, so a
	// build that reads version 2 still reads every other agent's.
	c.Version = 2
	if c.DevHost != nil {
		c.Version = 3
	}
	if len(c.Ports) > 0 {
		c.Version = 4
	}
	// A build that does not know added skills would drop them at its next
	// save: it refuses the config instead. So for stored secrets and git
	// host accounts.
	if len(c.Skills) > 0 {
		c.Version = 5
	}
	if len(c.Secrets) > 0 {
		c.Version = 6
	}
	if len(c.Accounts) > 0 {
		c.Version = ConfigVersion
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	dir := Dir(home, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("the %s agent's folder: %w", name, err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return fmt.Errorf("write the %s agent's configuration: %w", name, err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write the %s agent's configuration: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, configFile)); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write the %s agent's configuration: %w", name, err)
	}
	return nil
}

// KindSegmentStart is the transcript's mark of a segment's start: Text says
// whether the conversation continued and, when it did not, why.
const KindSegmentStart session.Kind = "segment_start"

// Entry is one line of the agent's transcript.ndjson: a session event of a
// segment, as the engine emitted it (redacted), without the harness's raw
// message.
type Entry struct {
	Segment int    `json:"segment"`
	Run     string `json:"run"`
	session.Event
	// Resumed is set on a segment_start entry.
	Resumed *bool `json:"resumed,omitempty"`
	// Task is, on a user turn the heartbeat sent, the task it was for.
	Task string `json:"task,omitempty"`
}

// kept says whether the transcript keeps an event: the conversation and
// what bounds it, not each streamed chunk or the harness's own notes.
func kept(k session.Kind) bool {
	switch k {
	case session.KindUserTurn, session.KindAssistantTurn, session.KindToolCall, session.KindError,
		session.KindSessionEnd, session.KindInputAdded, session.KindPermissionDenied, session.KindNotice:
		return true
	}
	return false
}

// appendEntry adds one line to the transcript.
func appendEntry(home, name string, e Entry) error {
	e.Raw = nil
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(Dir(home, name), transcriptFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the runner home's own file
	if err != nil {
		return err
	}
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Close()
	return errors.Join(werr, cerr)
}

// Transcript is the last n lines of the agent's transcript (every line for
// n <= 0). A line that does not parse is skipped.
func Transcript(home, name string, n int) ([]Entry, error) {
	f, err := os.Open(filepath.Join(Dir(home, name), transcriptFile)) //nolint:gosec // the runner home's own file
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		out = append(out, e)
		if n > 0 && len(out) > 2*n {
			out = append([]Entry(nil), out[len(out)-n:]...)
		}
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, sc.Err()
}

// lastExchange is the last turn the person sent and the agent answered, in
// the transcript: what a new conversation is given when the stored one
// could not be continued.
func lastExchange(entries []Entry) (user, assistant string, ok bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Kind != session.KindAssistantTurn || strings.TrimSpace(entries[i].Text) == "" {
			continue
		}
		for j := i - 1; j >= 0; j-- {
			if entries[j].Kind == session.KindUserTurn && !entries[j].Synthetic {
				return entries[j].Text, entries[i].Text, true
			}
		}
		return "", "", false
	}
	return "", "", false
}

// maxCarried bounds each side of the exchange carried into a new
// conversation.
const maxCarried = 4000

// carried is the text a new conversation's first turn starts with when the
// stored conversation could not be continued. Each side is quoted, so
// nothing either said can pass for the frame around it.
func carried(user, assistant string) string {
	cut := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) > maxCarried {
			s = strings.ToValidUTF8(s[:maxCarried], "") + " [cut]"
		}
		return strconv.Quote(s)
	}
	return "(The runner restarted you without the rest of this conversation. The last exchange, each side quoted: the person said " +
		cut(user) + "; you answered " + cut(assistant) + ".)\n\n"
}

// Usage is the agent's lifetime usage, summed over its segments' envelopes
// (usage.json).
type Usage struct {
	Segments     int       `json:"segments"`
	InputTokens  int64     `json:"inputTokens"`
	OutputTokens int64     `json:"outputTokens"`
	TotalTokens  int64     `json:"totalTokens"`
	UserTurns    int       `json:"userTurns"`
	ToolCalls    int       `json:"toolCalls"`
	Updated      time.Time `json:"updated"`
}

// LoadUsage reads usage.json; none is the zero Usage.
func LoadUsage(home, name string) (Usage, error) {
	var u Usage
	b, err := os.ReadFile(filepath.Join(Dir(home, name), usageFile)) //nolint:gosec // the runner home's own file
	if errors.Is(err, fs.ErrNotExist) {
		return u, nil
	}
	if err != nil {
		return u, err
	}
	return u, json.Unmarshal(b, &u)
}

func saveUsage(home, name string, u Usage) error {
	b, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(Dir(home, name), usageFile), append(b, '\n'), 0o600)
}

// Reset puts a stopped agent back to its defaults, as it was when first
// enabled: its volumes, its transcript, its lifetime usage, its share
// verdicts, its share mirror, its installed servers, its tasks, the skills
// the person added, the secrets they stored and the git host accounts they
// gave it, from the keychain too, are removed; its configuration keeps only
// whether it is enabled, so its built-in skills are on again and its next
// start is a first segment that asks its consents again. The segments'
// receipts and the files in its share folder, which are the person's, are
// kept; so is a dev server's sign-in in the keychain, where a schedule of
// the same host may read it. It refuses a running agent, and holds the
// agent's lock while it works so none starts meanwhile. The containers of a
// segment whose supervisor died still hold the session volume, so they are
// swept first.
func Reset(ctx context.Context, home, name string, d dockerx.Client) error {
	return reset(ctx, home, name, d, resetAll)
}

// ResetHistory removes what a stopped agent did and keeps what the person
// chose: its conversation, files and memory volumes, its transcript, tasks,
// share verdicts, share text and share mirror go; its configuration, its
// consents, its installed servers and added skills, its secrets and
// sign-ins (their keychain entries and volumes) and its usage stay. Its
// next start is a new conversation that asks nothing again. Only an agent
// that resets in parts has it.
func ResetHistory(ctx context.Context, home, name string, d dockerx.Client) error {
	return reset(ctx, home, name, d, resetHistory)
}

// ResetAgent puts a stopped agent's choices back to their defaults and
// removes its history, as Reset does, but keeps the credentials the person
// gave it: its stored secrets, its git host accounts and its image servers'
// sign-ins stay, with their keychain entries and volumes, switched off and
// not agreed to, so turning one on asks again and needs no new set-up. Its
// share folder's location and its usage stay too. Only an agent that resets
// in parts has it.
func ResetAgent(ctx context.Context, home, name string, d dockerx.Client) error {
	return reset(ctx, home, name, d, resetAgent)
}

// resetKind is how much a reset removes.
type resetKind int

const (
	resetAll resetKind = iota
	resetHistory
	resetAgent
)

func reset(ctx context.Context, home, name string, d dockerx.Client, kind resetKind) error {
	// Only a known agent's folder is emptied.
	a, err := Lookup(name)
	if err != nil {
		return err
	}
	if kind != resetAll && !a.Builtin.ResetsInParts() {
		return fmt.Errorf("the %s agent has one Reset; Reset history and Reset agent are the Assistant's", name)
	}
	owner, err := Acquire(home, name, ClientCLI)
	if err != nil {
		return err
	}
	defer owner.Release()
	engine.SweepAbandoned(ctx, d, nil)
	cfg, err := LoadConfig(home, name)
	if err != nil {
		return err
	}
	// The next start must be a first segment whatever is removed below: one
	// that went on counting would take the legacy volumes' conversation
	// back in, so a reset stopped half way still begins a new one.
	cfg.Segments, cfg.Conversation, cfg.PersonaGiven = 0, nil, ""
	if err := SaveConfig(home, name, cfg); err != nil {
		return err
	}
	volumes := []string{SessionVolumeIn(home, name), DataVolumeIn(home, name), MemoryVolumeIn(home, name)}
	if kind == resetAll {
		volumes = append(volumes, imageVolumes(home, a)...)
	}
	for _, v := range volumes {
		if err := d.VolumeRemove(ctx, v); err != nil {
			return fmt.Errorf("remove the %s agent's volume %s: %w", name, v, err)
		}
	}
	files := []string{transcriptFile, shareVerdictsFile, tasksFile, memorySeededFile}
	if kind == resetAll {
		files = append(files, usageFile)
	}
	for _, f := range files {
		if err := os.Remove(filepath.Join(Dir(home, name), f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.RemoveAll(filepath.Join(Dir(home, name), shareTextDir)); err != nil {
		return fmt.Errorf("remove the %s agent's kept share text: %w", name, err)
	}
	if err := removeShareMirror(home, name); err != nil {
		return err
	}
	if kind == resetHistory {
		return nil
	}
	if err := os.RemoveAll(serversDir(home, name)); err != nil {
		return fmt.Errorf("remove the %s agent's installed servers: %w", name, err)
	}
	for _, dir := range []string{AddedSkillsPath(home, name), skillsPath(home, name)} {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove the %s agent's skills: %w", name, err)
		}
	}
	if kind == resetAgent {
		return SaveConfig(home, name, keptCredentials(cfg))
	}
	for _, sec := range cfg.Secrets {
		if err := resetSecrets.Delete(SecretAccount(name, sec.Name)); err != nil {
			return fmt.Errorf("remove the %s agent's secret %s: %w", name, sec.Name, err)
		}
	}
	for _, g := range cfg.Accounts {
		if err := resetSecrets.Delete(AccountKeychainAccount(g.Host, g.User)); err != nil {
			return fmt.Errorf("remove the %s agent's token for %s: %w", name, g.Host, err)
		}
	}
	// An image server's keyring password: its volume, which the password
	// opened, is gone above, so the password goes too.
	for _, s := range imageServersOf(a) {
		if len(s.Secrets) == 0 {
			continue
		}
		if err := resetSecrets.Delete(ImageKeyringAccount(name, s.Key)); err != nil {
			return fmt.Errorf("remove the %s agent's keyring password for %s: %w", name, s.Name, err)
		}
	}
	return SaveConfig(home, name, Config{Version: ConfigVersion, Enabled: cfg.Enabled})
}

// keptCredentials is the configuration Reset agent leaves: the defaults,
// with whether the agent is enabled, its share folder's location, and the
// credentials the person gave it. A stored secret or an account, on by
// default once stored, is switched off; an image server is off until
// switched on. None is agreed to, so turning one on asks again.
func keptCredentials(cfg Config) Config {
	out := Config{Version: ConfigVersion, Enabled: cfg.Enabled, Share: cfg.Share, Secrets: cfg.Secrets, Accounts: cfg.Accounts, SignIns: cfg.SignIns}
	off := map[string]bool{}
	for _, sec := range cfg.Secrets {
		off[SecretEntryID(sec.Name)] = false
	}
	for _, g := range cfg.Accounts {
		off[AccountEntryID(g.Host)] = false
	}
	if len(off) > 0 {
		out.Switches = off
	}
	return out
}

// resetSecrets is where Reset removes the stored secrets' values from: the
// keychain, but in a test.
var resetSecrets Secrets = KeychainSecrets{}
