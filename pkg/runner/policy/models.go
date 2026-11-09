package policy

// Providers and saved models. A provider is a credential: a vendor's key, a
// model sign-in, a connected account or Agentry credits, and for an endpoint
// of the person's own its address. A saved model is a provider plus a model
// id, under a name the person gives it. A computer keeps up to MaxSavedModels
// of them, and the first one saved is the default, which every run uses
// unless it names another.
//
// The file has two shapes. Version 1 holds one model block (and per-adapter
// harness blocks), which reads as one provider and one saved model, plus one
// saved model for each harness block that names a model. Version 2 holds the
// providers and the saved models themselves. Save writes version 1 whenever
// the lists say nothing version 1 cannot, so a computer whose policy holds
// one provider and one unnamed model stays readable by a build that knows
// version 1 alone; it writes version 2 only for more than one provider or
// saved model, a custom provider of its own name, or a saved model's own
// name. Either way, Model is the default saved model as the compiler reads
// it.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxSavedModels is how many saved models a computer keeps.
const MaxSavedModels = 10

// Version1 is the schema that holds one model block.
const Version1 = "1"

// ProviderCustom is the vendor of an OpenAI-compatible endpoint of the
// person's own. A provider of that vendor is named custom-<slug>, one per
// endpoint; a version 1 policy's custom model is the provider "custom".
const ProviderCustom = "custom"

// customPrefix starts the id of a custom provider of the person's own name.
const customPrefix = ProviderCustom + "-"

// maxModelNameLength bounds a saved model's name, in characters.
const maxModelNameLength = 64

// providerIDRE is a provider id: a vendor's name, or custom-<slug>. It
// names the keychain account provider:<id>, so it never holds a colon or
// whitespace.
var providerIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// customSlugRE is the slug of custom-<slug>.
var customSlugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// modelIDRE is a saved model's id.
var modelIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Provider is one credential the saved models are paid with. The key, when
// there is one, is never in the file: ProviderAPIKeyEnv names the variable,
// and the keychain entry is provider:<id>. A sign-in is kept in the model
// sign-in store, one per vendor.
type Provider struct {
	// Name is what the person called an endpoint of their own; empty for a
	// named vendor.
	Name string `yaml:"name,omitempty"`
	// APIKeyEnv, APIKeySource, APIKeyKind, BaseURL and ProxyAllow are as
	// the model block's.
	APIKeyEnv    string   `yaml:"apiKeyEnv,omitempty"`
	APIKeySource string   `yaml:"apiKeySource,omitempty"`
	APIKeyKind   string   `yaml:"apiKeyKind,omitempty"`
	BaseURL      string   `yaml:"baseUrl,omitempty"`
	ProxyAllow   []string `yaml:"proxyAllow"`
}

// SavedModel is a provider plus a model id, under the person's name for it.
type SavedModel struct {
	// ID is generated when the model is saved and never changes: what a
	// schedule or an agent names it by.
	ID string `yaml:"id"`
	// Name is the person's, unique on the computer.
	Name string `yaml:"name"`
	// Provider is a key of the policy's providers.
	Provider string `yaml:"provider"`
	// Model is the model id the provider is asked for.
	Model string `yaml:"model"`
	// ContextWindow is as the model block's, for an endpoint of the
	// person's own.
	ContextWindow int64 `yaml:"contextWindow,omitempty"`
}

// ProviderVendor is the vendor a provider id belongs to: custom for
// custom and custom-<slug>, else the id itself.
func ProviderVendor(id string) string {
	if id == ProviderCustom || strings.HasPrefix(id, customPrefix) {
		return ProviderCustom
	}
	return id
}

// CustomProviderID is the provider id of an endpoint of the person's own
// called name: custom-<slug>, the slug the name's letters and digits.
func CustomProviderID(name string) (string, error) {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	slug := strings.TrimRight(b.String(), "-")
	if slug == "" {
		return "", fmt.Errorf("the endpoint's name %q needs a letter or digit (a-z, 0-9): its provider is named custom-<those>", name)
	}
	id := customPrefix + slug
	if len(id) > 63 {
		id = strings.TrimRight(id[:63], "-")
	}
	return id, nil
}

// KeyProvider is the provider whose keychain entry, provider:<id>, holds
// m's key: the saved model's provider, or for a model block the vendor.
func (m Model) KeyProvider() string {
	if m.ProviderID != "" {
		return m.ProviderID
	}
	return m.Provider
}

// HasSavedModels reports whether the policy holds its models as lists, as a
// version 2 policy does or a version 1 policy once a list was changed.
func (p *Policy) HasSavedModels() bool { return len(p.Models) > 0 }

// SavedModels are the policy's saved models, in order, and the providers
// they name. For a version 1 policy, nothing changed, they are what the
// model block and the harness blocks mean.
func (p *Policy) SavedModels() (map[string]Provider, []SavedModel, string) {
	if p.HasSavedModels() {
		return maps.Clone(p.Providers), slices.Clone(p.Models), p.Default
	}
	return migrateV1(p.Model, p.Harness)
}

// DefaultModel is the default saved model.
func (p *Policy) DefaultModel() SavedModel {
	_, models, def := p.SavedModels()
	for _, m := range models {
		if m.ID == def {
			return m
		}
	}
	return SavedModel{}
}

// FindModel is the saved model named ref, by its name (any case) or its id.
func (p *Policy) FindModel(ref string) (SavedModel, bool) {
	_, models, _ := p.SavedModels()
	ref = strings.TrimSpace(ref)
	for _, m := range models {
		if m.ID == ref {
			return m, true
		}
	}
	for _, m := range models {
		if strings.EqualFold(m.Name, ref) {
			return m, true
		}
	}
	return SavedModel{}, false
}

// ModelView is sm as the compiler reads a model: its provider's credential
// and endpoint, and its own model id and context window.
func (p *Policy) ModelView(sm SavedModel) (Model, error) {
	providers, _, _ := p.SavedModels()
	pr, ok := providers[sm.Provider]
	if !ok {
		return Model{}, fmt.Errorf("saved model %q names the provider %q, which this computer does not have", sm.Name, sm.Provider)
	}
	return viewOf(sm, sm.Provider, pr), nil
}

// viewOf is the model block a saved model on provider id reads as.
func viewOf(sm SavedModel, id string, pr Provider) Model {
	return Model{
		Provider:      ProviderVendor(id),
		ProviderID:    id,
		Name:          sm.Model,
		APIKeyEnv:     pr.APIKeyEnv,
		APIKeySource:  pr.APIKeySource,
		APIKeyKind:    pr.APIKeyKind,
		BaseURL:       pr.BaseURL,
		ContextWindow: sm.ContextWindow,
		ProxyAllow:    slices.Clone(pr.ProxyAllow),
	}
}

// providerOf is the provider a model block's credential and endpoint are.
func providerOf(m Model) Provider {
	return Provider{
		APIKeyEnv:    m.APIKeyEnv,
		APIKeySource: m.APIKeySource,
		APIKeyKind:   m.APIKeyKind,
		BaseURL:      m.BaseURL,
		ProxyAllow:   slices.Clone(m.ProxyAllow),
	}
}

// migratedID is the id a saved model read from version 1 gets: fixed by
// where it came from, so every read gives the same one.
func migratedID(from string) string {
	sum := sha256.Sum256([]byte("agentry saved model: " + from))
	return hex.EncodeToString(sum[:4])
}

// migrateV1 is what a version 1 policy's blocks mean as lists: the model
// block is a provider named for its vendor and the default saved model,
// named for the provider; each harness block that names a model is a saved
// model of its own, on the same provider, named for the harness.
func migrateV1(m Model, harness map[string]HarnessPolicy) (map[string]Provider, []SavedModel, string) {
	if m.Provider == "" && m.Name == "" {
		return nil, nil, ""
	}
	id := m.KeyProvider()
	providers := map[string]Provider{id: providerOf(m)}
	def := SavedModel{ID: migratedID("model"), Name: id, Provider: id, Model: m.Name, ContextWindow: m.ContextWindow}
	models := []SavedModel{def}
	names := []string{}
	for name, hp := range harness {
		if strings.TrimSpace(hp.Model) != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		n := name
		for i := 2; slices.ContainsFunc(models, func(sm SavedModel) bool { return strings.EqualFold(sm.Name, n) }); i++ {
			n = fmt.Sprintf("%s-%d", name, i)
		}
		models = append(models, SavedModel{ID: migratedID("harness:" + name), Name: n, Provider: id, Model: strings.TrimSpace(harness[name].Model)})
	}
	return providers, models, def.ID
}

// materialize turns a version 1 policy's blocks into the lists, before a
// list is changed: the model block keeps reading as the default.
func (p *Policy) materialize() {
	if p.HasSavedModels() {
		p.foldModel()
		return
	}
	p.Providers, p.Models, p.Default = migrateV1(p.Model, p.Harness)
	p.refreshView()
}

// refreshView sets Model to the default saved model as the compiler reads
// it, and remembers it, so a caller that changes Model afterwards (setup,
// written before the lists) is seen and folded into the default.
func (p *Policy) refreshView() {
	if !p.HasSavedModels() {
		return
	}
	for _, sm := range p.Models {
		if sm.ID == p.Default {
			if pr, ok := p.Providers[sm.Provider]; ok {
				p.Model = viewOf(sm, sm.Provider, pr)
			}
			break
		}
	}
	p.view = p.Model
	p.view.ProxyAllow = slices.Clone(p.Model.ProxyAllow)
}

// foldModel carries a change made to Model since the lists were read into
// the default saved model and its provider: setup changes the default this
// way. A change of vendor puts the default on that vendor's provider.
func (p *Policy) foldModel() {
	if !p.HasSavedModels() || reflect.DeepEqual(p.Model, p.view) {
		return
	}
	m := p.Model
	id := m.ProviderID
	if id == "" || ProviderVendor(id) != m.Provider {
		id = m.Provider
	}
	pr := providerOf(m)
	if old, ok := p.Providers[id]; ok {
		pr.Name = old.Name
	}
	if p.Providers == nil {
		p.Providers = map[string]Provider{}
	}
	p.Providers[id] = pr
	for i := range p.Models {
		if p.Models[i].ID == p.Default {
			p.Models[i].Provider = id
			p.Models[i].Model = m.Name
			p.Models[i].ContextWindow = m.ContextWindow
		}
	}
	p.refreshView()
}

// newModelID is a fresh saved model id, unused in models.
func newModelID(models []SavedModel) (string, error) {
	for range 16 {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		id := hex.EncodeToString(b)
		if !slices.ContainsFunc(models, func(sm SavedModel) bool { return sm.ID == id }) {
			return id, nil
		}
	}
	return "", errors.New("could not make a saved model id")
}

// AddProvider adds a provider under id. A named vendor has one provider;
// an endpoint of the person's own is a custom-<slug> provider each.
func (p *Policy) AddProvider(id string, pr Provider) error {
	p.materialize()
	if _, ok := p.Providers[id]; ok {
		return fmt.Errorf("this computer already has the provider %s: one provider per vendor (remove it first to set it up again)", id)
	}
	if err := checkProvider(id, pr); err != nil {
		return err
	}
	if p.Providers == nil {
		p.Providers = map[string]Provider{}
	}
	p.Providers[id] = pr
	return nil
}

// AddModel saves sm under a new id and returns it as saved. The first
// saved model is the default.
func (p *Policy) AddModel(sm SavedModel) (SavedModel, error) {
	p.materialize()
	if len(p.Models) >= MaxSavedModels {
		return SavedModel{}, fmt.Errorf("this computer has %d saved models, the most it keeps: remove one first (agentry models remove)", MaxSavedModels)
	}
	if err := p.checkSavedModel(sm, ""); err != nil {
		return SavedModel{}, err
	}
	id, err := newModelID(p.Models)
	if err != nil {
		return SavedModel{}, err
	}
	sm.ID = id
	p.Models = append(p.Models, sm)
	if p.Default == "" {
		p.Default = id
	}
	p.refreshView()
	return sm, nil
}

// EditModel replaces the saved model ref with sm, keeping its id.
func (p *Policy) EditModel(ref string, sm SavedModel) (SavedModel, error) {
	old, ok := p.FindModel(ref)
	if !ok {
		return SavedModel{}, fmt.Errorf("no saved model %q on this computer (agentry models list)", ref)
	}
	p.materialize()
	if err := p.checkSavedModel(sm, old.ID); err != nil {
		return SavedModel{}, err
	}
	sm.ID = old.ID
	for i := range p.Models {
		if p.Models[i].ID == old.ID {
			p.Models[i] = sm
		}
	}
	p.refreshView()
	return sm, nil
}

// SetDefault makes the saved model ref the default.
func (p *Policy) SetDefault(ref string) (SavedModel, error) {
	sm, ok := p.FindModel(ref)
	if !ok {
		return SavedModel{}, fmt.Errorf("no saved model %q on this computer (agentry models list)", ref)
	}
	p.materialize()
	p.Default = sm.ID
	p.refreshView()
	return sm, nil
}

// RemoveModel removes the saved model ref. Removing the default needs the
// new default, newDefault, named in the same act.
func (p *Policy) RemoveModel(ref, newDefault string) (SavedModel, error) {
	sm, ok := p.FindModel(ref)
	if !ok {
		return SavedModel{}, fmt.Errorf("no saved model %q on this computer (agentry models list)", ref)
	}
	p.materialize()
	if err := p.replaceDefault([]string{sm.ID}, newDefault); err != nil {
		return SavedModel{}, err
	}
	p.Models = slices.DeleteFunc(p.Models, func(m SavedModel) bool { return m.ID == sm.ID })
	p.refreshView()
	return sm, nil
}

// RemoveProvider removes the provider id and every saved model on it, which
// it returns. Removing the default's needs the new default, newDefault, on
// another provider.
func (p *Policy) RemoveProvider(id, newDefault string) ([]SavedModel, error) {
	providers, models, _ := p.SavedModels()
	if _, ok := providers[id]; !ok {
		return nil, fmt.Errorf("no provider %q on this computer (agentry providers list)", id)
	}
	var on []SavedModel
	var ids []string
	for _, m := range models {
		if m.Provider == id {
			on = append(on, m)
			ids = append(ids, m.ID)
		}
	}
	p.materialize()
	if err := p.replaceDefault(ids, newDefault); err != nil {
		return nil, err
	}
	p.Models = slices.DeleteFunc(p.Models, func(m SavedModel) bool { return slices.Contains(ids, m.ID) })
	delete(p.Providers, id)
	p.refreshView()
	return on, nil
}

// replaceDefault makes newDefault the default when removing the saved
// models ids takes the default away; a new default that is itself among
// them, or none, is refused.
func (p *Policy) replaceDefault(ids []string, newDefault string) error {
	if !slices.Contains(ids, p.Default) {
		return nil
	}
	cur := p.DefaultModel()
	if strings.TrimSpace(newDefault) == "" {
		if len(p.Models) == len(ids) {
			return fmt.Errorf("%q is this computer's only saved model: save another first (agentry models add), then remove this one", cur.Name)
		}
		return fmt.Errorf("%q is the default saved model: name the new default (--move-to <name>), or make another the default first (agentry models default)", cur.Name)
	}
	next, ok := p.FindModel(newDefault)
	if !ok {
		return fmt.Errorf("no saved model %q on this computer to be the new default", newDefault)
	}
	if slices.Contains(ids, next.ID) {
		return fmt.Errorf("%q is removed too, so it cannot be the new default", next.Name)
	}
	p.Default = next.ID
	return nil
}

// checkProvider holds one provider to the rules the file is held to.
func checkProvider(id string, pr Provider) error {
	field := "providers." + id
	if !providerIDRE.MatchString(id) {
		return fmt.Errorf("provider id %q must be lowercase letters, digits, '.', '_' or '-' (it names the keychain account provider:<id>)", id)
	}
	if strings.HasPrefix(id, customPrefix) && !customSlugRE.MatchString(strings.TrimPrefix(id, customPrefix)) {
		return fmt.Errorf("provider id %q must be custom-<slug>, lowercase letters and digits joined by '-'", id)
	}
	if ProviderVendor(id) == ProviderCustom && strings.TrimSpace(pr.BaseURL) == "" {
		return fmt.Errorf("%s.baseUrl is required: an endpoint of your own is reached at its address", field)
	}
	if pr.Name != "" && ProviderVendor(id) != ProviderCustom {
		return fmt.Errorf("%s.name is for an endpoint of your own (a custom-<slug> provider)", field)
	}
	return checkModelBlock(field, Model{Provider: ProviderVendor(id), ProviderID: id, Name: "-",
		APIKeyEnv: pr.APIKeyEnv, APIKeySource: pr.APIKeySource, APIKeyKind: pr.APIKeyKind, BaseURL: pr.BaseURL, ProxyAllow: pr.ProxyAllow})
}

// checkSavedModel holds sm to the rules beside the other saved models,
// itself (by id self) left out.
func (p *Policy) checkSavedModel(sm SavedModel, self string) error {
	name := strings.TrimSpace(sm.Name)
	switch {
	case name == "":
		return errors.New("a saved model needs a name")
	case name != sm.Name:
		return fmt.Errorf("saved model name %q must not start or end with a space", sm.Name)
	case utf8.RuneCountInString(name) > maxModelNameLength:
		return fmt.Errorf("saved model name %q is longer than %d characters", sm.Name, maxModelNameLength)
	case strings.ContainsFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }):
		return fmt.Errorf("saved model name %q holds a character that is not printable", sm.Name)
	case modelIDRE.MatchString(name) && slices.ContainsFunc(p.Models, func(m SavedModel) bool { return m.ID == name && m.ID != self }):
		return fmt.Errorf("saved model name %q is another saved model's id", sm.Name)
	}
	for _, m := range p.Models {
		if m.ID != self && strings.EqualFold(m.Name, name) {
			return fmt.Errorf("there is already a saved model named %q", m.Name)
		}
	}
	if _, ok := p.Providers[sm.Provider]; !ok {
		return fmt.Errorf("saved model %q names the provider %q, which this computer does not have (agentry providers list)", sm.Name, sm.Provider)
	}
	if strings.TrimSpace(sm.Model) == "" || strings.ContainsAny(sm.Model, "\n\r\t") {
		return fmt.Errorf("saved model %q needs a model id", sm.Name)
	}
	if err := CheckContextWindow(sm.ContextWindow); err != nil {
		return fmt.Errorf("saved model %q: contextWindow: %w", sm.Name, err)
	}
	return nil
}

// validateSavedModels holds the lists to the file's rules: at most
// MaxSavedModels, unique ids and names, each on a provider the policy has,
// and a default that is one of them.
func (p *Policy) validateSavedModels() error {
	if len(p.Models) > MaxSavedModels {
		return fmt.Errorf("models: %d saved models, more than the %d a computer keeps", len(p.Models), MaxSavedModels)
	}
	ids := sortedKeys(p.Providers)
	for _, id := range ids {
		if err := checkProvider(id, p.Providers[id]); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for i, sm := range p.Models {
		if !modelIDRE.MatchString(sm.ID) {
			return fmt.Errorf("models[%d].id %q must be lowercase letters, digits and '-'", i, sm.ID)
		}
		if seen[sm.ID] {
			return fmt.Errorf("models: two saved models have the id %q", sm.ID)
		}
		seen[sm.ID] = true
		others := &Policy{Providers: p.Providers, Models: p.Models[:i]}
		if err := others.checkSavedModel(sm, ""); err != nil {
			return fmt.Errorf("models[%d]: %w", i, err)
		}
	}
	if !seen[p.Default] {
		return fmt.Errorf("default %q names no saved model (it must be one of the models' ids)", p.Default)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// readModels settles a policy just read: a version 2 file holds its models
// as lists and no model block, and Model becomes the default saved model; a
// version 1 file holds the model block and no lists.
func (p *Policy) readModels() error {
	switch p.PolicyVersion {
	case Version:
		if !reflect.DeepEqual(p.Model, Model{}) {
			return errors.New("policyVersion 2 holds its models in providers and models, and the model block is version 1's: move it into them (or set policyVersion to 1)")
		}
		if len(p.Models) == 0 {
			return errors.New("policyVersion 2 needs at least one saved model in models, and default naming one")
		}
		if err := p.validateSavedModels(); err != nil {
			return err
		}
		p.refreshView()
	case Version1:
		if len(p.Providers) > 0 || len(p.Models) > 0 || p.Default != "" {
			return errors.New("providers, models and default need policyVersion 2: version 1 holds one model block")
		}
	}
	return nil
}

// fileForm is the policy as Save writes it: version 1, with the lists left
// out, when the model block and the harness blocks say all they say, else
// version 2, with the model block left out.
func (p *Policy) fileForm() *Policy {
	q := *p
	if !p.HasSavedModels() {
		q.PolicyVersion = Version1
		return &q
	}
	m := p.Model
	m.ProviderID = ""
	providers, models, def := migrateV1(m, p.Harness)
	if def == p.Default && reflect.DeepEqual(providers, p.Providers) && reflect.DeepEqual(models, p.Models) {
		q.PolicyVersion = Version1
		q.Model.ProviderID = ""
		q.Providers, q.Models, q.Default = nil, nil, ""
		return &q
	}
	q.PolicyVersion = Version
	q.Model = Model{}
	return &q
}
