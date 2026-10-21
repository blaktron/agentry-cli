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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

// SessionVolume and DataVolume are the agent's two volumes: the harness's
// conversation store, and the agent's own files.
func SessionVolume(name string) string { return "agentry-persistent-" + name + "-session" }

// DataVolume is the agent's volume for its own files.
func DataVolume(name string) string { return "agentry-persistent-" + name + "-data" }

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
)

// LogPath is where a detached supervisor writes what it prints.
func LogPath(home, name string) string { return filepath.Join(Dir(home, name), logFile) }

// ConfigVersion is the version of config.yaml this build writes. Version 2
// added the share folder, the person's servers and packs, the switches and
// the consents, version 3 the Coding agent's dev server and version 4 its
// published ports; a build that reads an older version alone refuses a newer
// one rather than drop what it does not know on its next save.
const ConfigVersion = 4

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
// enabled: its two volumes, its transcript, its lifetime usage, its share
// verdicts and its installed servers are removed, and its configuration
// keeps only whether it is enabled, so its next start is a first segment
// that asks its consents again. The segments' receipts and the files in its
// share folder, which are the person's, are kept; so is a dev server's
// sign-in in the keychain, where a schedule of the same host may read it.
// It refuses a running agent, and holds the agent's lock while it works so
// none starts meanwhile. The containers of a segment whose supervisor died
// still hold the session volume, so they are swept first.
func Reset(ctx context.Context, home, name string, d dockerx.Client) error {
	// Only a known agent's folder is emptied.
	if _, err := Lookup(name); err != nil {
		return err
	}
	owner, err := Acquire(home, name, ClientCLI)
	if err != nil {
		return err
	}
	defer owner.Release()
	engine.SweepAbandoned(ctx, d, nil)
	for _, v := range []string{SessionVolume(name), DataVolume(name)} {
		if err := d.VolumeRemove(ctx, v); err != nil {
			return fmt.Errorf("remove the %s agent's volume %s: %w", name, v, err)
		}
	}
	for _, f := range []string{transcriptFile, usageFile, shareVerdictsFile} {
		if err := os.Remove(filepath.Join(Dir(home, name), f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.RemoveAll(serversDir(home, name)); err != nil {
		return fmt.Errorf("remove the %s agent's installed servers: %w", name, err)
	}
	cfg, err := LoadConfig(home, name)
	if err != nil {
		return err
	}
	return SaveConfig(home, name, Config{Version: ConfigVersion, Enabled: cfg.Enabled})
}
