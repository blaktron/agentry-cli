package managed

// The machine settings file: settings the administrator of this computer
// deploys, with an MDM's configuration profile, Group Policy or their own
// tools, in a place only an administrator can write. It may pin the
// registry the client uses, require that the person be signed in as an
// active member of an organization before anything runs, and lock or
// default any catalogue setting. Its settings come before every
// organization's, so a lock the administrator set wins, and they survive
// signing out. It is read on every load, and needs no registry.
//
// The file is the administrator's word, not a registry's: it is not
// signed. What protects it is where it lives. A file a person who is not
// the administrator could have written is refused, and while it is
// refused no run starts.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry/pkg/managedsettings"
)

// MachineOrg names the machine settings file where a message names whose
// settings hold: "<MachineOrg>'s managed settings".
const MachineOrg = "this computer's administrator"

// Machine is the machine settings file as read.
type Machine struct {
	// Where is where it was read: a file's path, or the Windows registry
	// key.
	Where string
	// Registry pins the registry the client uses; "" pins none.
	Registry string
	// RequireOrg is the handle of the organization the person must be
	// signed in to as an active member before a run starts; "" requires
	// none.
	RequireOrg string
	// Settings are catalogue settings, each locked or a default, held
	// before any organization's.
	Settings map[string]managedsettings.Setting
}

// machineFile is the file's shape. On macOS the configuration profile's
// keys are the same, read through plutil; on Windows each is a value of
// the policy key, settings as a JSON string.
type machineFile struct {
	Registry   string                             `json:"registry,omitempty"`
	RequireOrg string                             `json:"requireOrg,omitempty"`
	Settings   map[string]managedsettings.Setting `json:"settings,omitempty"`
}

// orgHandle is the shape of an organization's handle.
var orgHandle = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?$`)

// readMachineSource reads the machine settings, as JSON, from where this
// system keeps them: found is false when there are none. Tests replace it.
var readMachineSource = platformMachineSource

// LoadMachine reads and checks the machine settings: nil when there are
// none. An error is a file that exists and cannot be used, which refuses
// every run.
func LoadMachine() (*Machine, error) {
	raw, where, found, err := readMachineSource()
	if err != nil {
		return nil, fmt.Errorf("the machine settings at %s cannot be used: %w", where, err)
	}
	if !found {
		return nil, nil
	}
	m, err := parseMachine(raw)
	if err != nil {
		return nil, fmt.Errorf("the machine settings at %s cannot be used: %w", where, err)
	}
	m.Where = where
	return m, nil
}

// parseMachine checks the machine settings: known fields alone, an https
// registry (http only for this computer's own address), a well-formed
// organization handle that comes with the registry it belongs to, and
// settings the catalogue accepts. A setting that names an organization's
// key is refused: only a registry delivers keys.
func parseMachine(raw []byte) (*Machine, error) {
	var f machineFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("it is not machine settings (registry, requireOrg, settings): %w", err)
	}
	m := &Machine{Settings: f.Settings}
	if f.Registry != "" {
		u, err := url.Parse(strings.TrimRight(strings.TrimSpace(f.Registry), "/"))
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
			(u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname()))) {
			return nil, fmt.Errorf("registry %q is not an https address", f.Registry)
		}
		m.Registry = normalize(u.String())
	}
	if f.RequireOrg != "" {
		if !orgHandle.MatchString(f.RequireOrg) {
			return nil, fmt.Errorf("requireOrg %q is not an organization's handle", f.RequireOrg)
		}
		if m.Registry == "" {
			return nil, errors.New("requireOrg names an organization, and registry does not say which registry it is on")
		}
		m.RequireOrg = f.RequireOrg
	}
	for _, id := range sortedIDs(m.Settings) {
		if err := managedsettings.ValidateSettings(map[string]managedsettings.Setting{id: m.Settings[id]}); err != nil {
			return nil, fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "managed settings: "))
		}
		// A source the machine settings name is the member's own or none:
		// an organization's key or credits need an organization, and the
		// machine settings belong to none.
		if row, ok := managedsettings.Lookup(id); ok && row.Kind == managedsettings.KindSource {
			var src managedsettings.Source
			if json.Unmarshal(m.Settings[id].Value, &src) == nil && src.Source != managedsettings.SourceMember && src.Source != managedsettings.SourceOff {
				return nil, fmt.Errorf("%s may be %s or %s in machine settings: an organization's key or credits come from a registry's managed settings", id, managedsettings.SourceMember, managedsettings.SourceOff)
			}
		}
	}
	return m, nil
}

func loopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func sortedIDs(m map[string]managedsettings.Setting) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// machineDocument is the machine settings as a document the engine holds
// a run to, first among the documents.
func (m *Machine) machineDocument() State {
	return State{Document: managedsettings.Document{Schema: managedsettings.Schema, Org: MachineOrg, Settings: m.Settings}}
}

// RequireOrgRefusal is why no run starts under e because the machine
// settings require an organization the person is not signed in to as an
// active member, or "". Membership is known by the organization's verified
// document from the pinned registry, so the organization must hold at
// least one setting for its members.
func (e Effective) RequireOrgRefusal() string {
	m := e.Machine
	if m == nil || m.RequireOrg == "" {
		return ""
	}
	for _, d := range e.Documents {
		if d.Registry == m.Registry && d.Document.Org == m.RequireOrg {
			return ""
		}
	}
	return fmt.Sprintf("this computer's administrator requires you to be signed in to %s as an active member of %s before anything runs: sign in with agentry login --registry %s, then agentry managed refresh", m.Registry, m.RequireOrg, m.Registry)
}

// pinTTL is how long a process keeps the machine settings' pin before it
// reads them again, so a long-lived process takes a change within it.
const pinTTL = time.Minute

var (
	pinMu  sync.Mutex
	pinAt  time.Time
	pinned string
	pinErr error
	pinNow = time.Now
)

// MachineRegistry is the registry the machine settings pin, or "" when
// they pin none. Machine settings that exist and cannot be used are an
// error: the administrator's registry is then unknown, so nothing that
// picks a registry may go on. It is read again after pinTTL.
func MachineRegistry() (string, error) {
	pinMu.Lock()
	defer pinMu.Unlock()
	if !pinAt.IsZero() && pinNow().Sub(pinAt) < pinTTL {
		return pinned, pinErr
	}
	pinned, pinErr = "", nil
	m, err := LoadMachine()
	switch {
	case err != nil:
		pinErr = err
	case m != nil:
		pinned = m.Registry
	}
	pinAt = pinNow()
	return pinned, pinErr
}

// forgetPin makes the next MachineRegistry read the settings again.
func forgetPin() {
	pinMu.Lock()
	defer pinMu.Unlock()
	pinAt, pinned, pinErr = time.Time{}, "", nil
}
