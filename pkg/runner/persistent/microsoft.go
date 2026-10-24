package persistent

// Microsoft 365 and Outlook.com's sign-in, through Softeria's server in
// one-shots of its own image on its own volume. The person's own Entra
// client (an application id, no secret) is recorded in the configuration;
// the sign-in is the server's device-code flow, run with --login reaching
// Microsoft's sign-in host alone, its code relayed to the person and only
// Microsoft's device-login page opened. The server keeps its tokens in its
// cache on the volume, sealed by the image's keeper under a key kept in the
// keychain, so the volume alone opens nothing. The account is recorded only
// once the server lists it.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// The bounds of a Microsoft sign-in.
const (
	// MicrosoftSignInTimeout is how long the sign-in waits for the person
	// to enter the code: a device code lasts fifteen minutes.
	MicrosoftSignInTimeout = 15 * time.Minute
	// microsoftLoginHost is the one host the sign-in reaches.
	microsoftLoginHost = "login.microsoftonline.com:443"
	// maxSignoff bounds the signoff mail ends with.
	maxSignoff = 200
)

// The tenants a sign-in is made on: personal Microsoft accounts
// (Outlook.com, Hotmail, Live) on consumers, work and school accounts on
// organizations; a tenant's own id names one organization.
const (
	TenantConsumers     = "consumers"
	TenantOrganizations = "organizations"
)

// softeriaClients are the clients Softeria's server signs in as when it is
// given none: its own apps, which the person neither registered nor
// controls, so a sign-in through them is refused.
var softeriaClients = []string{"084a3e9f-a9f4-43f7-89f9-d229cf97853e", "f3e61a6e-bc26-4281-8588-2c7359a02141"}

// guidRE is an Entra application or tenant id.
var guidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// microsoftEmailRE is an address a Microsoft account signs in with, held to
// the same shape as a Google one: one @, a dotted domain, no space or control
// character, and no leading "-".
var microsoftEmailRE = googleEmailRE

// deviceCodeRE finds the device-code sentence Microsoft's sign-in library
// prints: the page to open and the code to enter there.
var deviceCodeRE = regexp.MustCompile(`(https://[^\s]+)\s+and enter the code\s+([A-Z0-9]{6,16})\b`)

// CheckMicrosoftClient refuses an Entra application id that is not one, or
// that is Softeria's own.
func CheckMicrosoftClient(id string) error {
	id = strings.ToLower(strings.TrimSpace(id))
	if !guidRE.MatchString(id) {
		return fmt.Errorf("%q is not an Entra application (client) id, which looks like 00000000-0000-0000-0000-000000000000", id)
	}
	if slices.Contains(softeriaClients, id) {
		return errors.New("that is the server's own default client, which you neither registered nor control: register your own Entra app and give its id")
	}
	return nil
}

// checkTenant refuses a tenant that is not consumers, organizations or a
// tenant id.
func checkTenant(t string) error {
	if t == TenantConsumers || t == TenantOrganizations || guidRE.MatchString(t) {
		return nil
	}
	return fmt.Errorf("%q is not a tenant: give consumers (a personal Microsoft account), organizations (a work or school account) or your organization's tenant id", t)
}

// deviceLoginPage says page is Microsoft's own device-login page, the only
// address the sign-in opens.
func deviceLoginPage(page string) bool {
	u, err := url.Parse(page)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	switch u.Host {
	case "microsoft.com", "www.microsoft.com":
		return u.Path == "/devicelogin"
	case "login.microsoftonline.com", "login.microsoft.com":
		return true
	}
	return false
}

// RealmTenant is the tenant an address signs in on, from Microsoft's own
// home-realm answer for it: an address whose domain Microsoft federates to
// its consumer sign-in (live.com), or that no organization holds, is a
// personal account's; one an organization holds signs in on organizations.
// A test replaces it.
var RealmTenant = func(ctx context.Context, email string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://login.microsoftonline.com/common/userrealm/"+url.PathEscape(email)+"?api-version=2.1", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ask Microsoft which kind of account %s is: %w", email, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ask Microsoft which kind of account %s is: %s", email, resp.Status)
	}
	var realm struct {
		NameSpaceType string `json:"NameSpaceType"`
		DomainName    string `json:"DomainName"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&realm); err != nil {
		return "", fmt.Errorf("read Microsoft's answer for %s: %w", email, err)
	}
	return realmTenant(realm.NameSpaceType, realm.DomainName), nil
}

// realmTenant is the tenant of a home-realm answer.
func realmTenant(namespace, domain string) string {
	if strings.EqualFold(namespace, "Unknown") || strings.EqualFold(domain, "live.com") {
		return TenantConsumers
	}
	return TenantOrganizations
}

// Microsoft is what Microsoft 365's sign-in needs, as Google is
// Google Workspace's.
type Microsoft struct {
	Docker      dockerx.Client
	Home        string
	Agent       Agent
	Policy      *policy.Policy
	RunnerImage string
	Keys        Secrets
	// Client is who calls, ClientCLI or ClientDesktop (ClientCLI when
	// unset).
	Client string
	// Say is where the steps, the code among them, are told.
	Say io.Writer
}

func (m Microsoft) acquire() (*Owner, error) {
	client := m.Client
	if client == "" {
		client = ClientCLI
	}
	return Acquire(m.Home, m.Agent.Name, client)
}

func (m Microsoft) say(format string, args ...any) {
	if m.Say != nil {
		_, _ = fmt.Fprintf(m.Say, format+"\n", args...)
	}
}

// server is the agent's Microsoft 365 server at the policy's image.
func (m Microsoft) server() (offer.ImageServer, error) {
	images := policy.Images{}
	if m.Policy != nil {
		images = m.Policy.Images
	}
	for _, s := range m.Agent.Builtin.ImageServers(images) {
		if s.Key == assistant.MS365Key {
			return s, nil
		}
	}
	return offer.ImageServer{}, fmt.Errorf("the %s agent has no Microsoft 365", m.Agent.Name)
}

// record changes the server's sign-in record in the configuration, enables
// the Microsoft provider, and saves it.
func (m Microsoft) record(key string, change func(*ImageSignIn)) error {
	c, err := LoadConfig(m.Home, m.Agent.Name)
	if err != nil {
		return err
	}
	in := c.SignIns[key]
	change(&in)
	if c.SignIns == nil {
		c.SignIns = map[string]ImageSignIn{}
	}
	c.SignIns[key] = in
	if p, ok := mailProviderOf(m.Agent, key); ok && enableMailProvider(&c, p.Key) {
		m.say("%s is now one of the %s agent's mail providers (agentry agent configure %s --no-mail-provider %s takes it off its roster).", p.Name, m.Agent.Name, m.Agent.Name, p.Key)
	}
	return SaveConfig(m.Home, m.Agent.Name, c)
}

// SetClient records the person's own Entra client, and the tenant to sign
// in on when they name one ("" asks Microsoft at the sign-in). A sign-in
// made with another client is kept until the next one. It is refused while
// the agent runs.
func (m Microsoft) SetClient(clientID, tenant string) error {
	clientID = strings.ToLower(strings.TrimSpace(clientID))
	tenant = strings.ToLower(strings.TrimSpace(tenant))
	if err := CheckMicrosoftClient(clientID); err != nil {
		return err
	}
	if tenant != "" {
		if err := checkTenant(tenant); err != nil {
			return err
		}
	}
	owner, err := m.acquire()
	if err != nil {
		return err
	}
	defer owner.Release()
	s, err := m.server()
	if err != nil {
		return err
	}
	return m.record(s.Key, func(in *ImageSignIn) {
		in.ClientID, in.Client = clientID, time.Now().UTC()
		if tenant != "" {
			in.Tenant = tenant
		}
	})
}

// SetSignoff records what mail the server sends or drafts ends with ("" for
// nothing): one line of printable text, at most 200 bytes.
func (m Microsoft) SetSignoff(text string) error {
	text = strings.TrimSpace(text)
	if len(text) > maxSignoff || strings.IndexFunc(text, func(r rune) bool { return r < ' ' || r == 0x7f }) >= 0 {
		return fmt.Errorf("a signoff is one line of at most %d bytes", maxSignoff)
	}
	owner, err := m.acquire()
	if err != nil {
		return err
	}
	defer owner.Release()
	s, err := m.server()
	if err != nil {
		return err
	}
	c, err := LoadConfig(m.Home, m.Agent.Name)
	if err != nil {
		return err
	}
	in := c.SignIns[s.Key]
	in.Signoff = text
	if c.SignIns == nil {
		c.SignIns = map[string]ImageSignIn{}
	}
	c.SignIns[s.Key] = in
	return SaveConfig(m.Home, m.Agent.Name, c)
}

// prepare makes the server's volume and its cache key when either is
// missing, and returns the volume and the key. A volume whose key the
// keychain lost cannot be opened, so it is made again: it holds nothing but
// the server's own sealed cache, which the person signs in to again.
func (m Microsoft) prepare(ctx context.Context, s offer.ImageServer) (volume, key string, err error) {
	if m.Keys == nil {
		return "", "", errors.New("the Microsoft 365 server keeps its sign-in sealed under a key in the keychain, and this client cannot reach the keychain")
	}
	volume = ImageVolumeIn(m.Home, m.Agent.Name, s.Key)
	have, err := m.Docker.VolumesByLabel(ctx, engine.PersistentHomeLabel+"="+HomeID(m.Home))
	if err != nil {
		return "", "", fmt.Errorf("list the %s agent's volumes: %w", m.Agent.Name, err)
	}
	account := ImageKeyringAccount(m.Agent.Name, s.Key)
	key, err = m.Keys.Get(account)
	if err != nil || key == "" {
		if slices.Contains(have, volume) {
			m.say("The keychain no longer has the key of the Microsoft sign-in kept on this computer, so it starts again.")
			if err := m.Docker.VolumeRemove(ctx, volume); err != nil {
				return "", "", fmt.Errorf("remove the Microsoft sign-in that can no longer be opened: %w", err)
			}
			have = slices.DeleteFunc(have, func(v string) bool { return v == volume })
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", "", err
		}
		key = base64.StdEncoding.EncodeToString(b)
		if err := m.Keys.Set(account, key); err != nil {
			return "", "", fmt.Errorf("keep the Microsoft sign-in's key in the keychain: %w", err)
		}
	}
	if !slices.Contains(have, volume) {
		if err := m.Docker.VolumeCreate(ctx, volume, engine.PersistentVolumeLabels(m.Agent.Name, HomeID(m.Home))); err != nil {
			return "", "", fmt.Errorf("make the volume %s: %w", volume, err)
		}
	}
	return volume, key, nil
}

// SignIn signs Microsoft 365 in to email by the server's device code: the
// one-shot reaches Microsoft's sign-in host alone, the code it prints is
// relayed through Say and its page opened by browse, and the sign-in asks
// only for what the switches on need. It returns the account the server
// lists, and records it only then.
func (m Microsoft) SignIn(ctx context.Context, email string, browse func(string) error) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !microsoftEmailRE.MatchString(email) {
		return "", fmt.Errorf("%q is not an email address a Microsoft account signs in with", email)
	}
	owner, err := m.acquire()
	if err != nil {
		return "", err
	}
	defer owner.Release()
	s, err := m.server()
	if err != nil {
		return "", err
	}
	c, err := LoadConfig(m.Home, m.Agent.Name)
	if err != nil {
		return "", err
	}
	in := c.SignIns[s.Key]
	if in.ClientID == "" {
		return "", fmt.Errorf("the Microsoft 365 server has no Entra client yet: agentry agent configure %s --microsoft-client <client id>", m.Agent.Name)
	}
	if err := CheckMicrosoftClient(in.ClientID); err != nil {
		return "", err
	}
	tenant := in.Tenant
	if tenant == "" {
		if tenant, err = RealmTenant(ctx, email); err != nil {
			return "", fmt.Errorf("%w (name the tenant with --microsoft-tenant consumers or organizations)", err)
		}
	}
	var granted []string
	for _, w := range s.Switches {
		if w.SignedFor && switchedOn(c, ImageSwitchEntryID(s.Key, w.Key), false) {
			granted = append(granted, w.Key)
		}
	}
	volume, key, err := m.prepare(ctx, s)
	if err != nil {
		return "", err
	}
	env := map[string]string{
		s.ClientEnv: in.ClientID, s.TenantEnv: tenant, assistant.MS365CacheKey: key,
		// The one-shot reaches the sign-in host through a CONNECT proxy
		// named in HTTPS_PROXY, which Node follows only when told to.
		"NODE_USE_ENV_PROXY": "1",
	}
	args := s.Args(func(k string) bool { return slices.Contains(granted, k) })

	wait, cancel := context.WithTimeout(ctx, MicrosoftSignInTimeout)
	defer cancel()
	relay := &deviceCodeRelay{say: m.say, browse: browse}
	_, stderr, code, err := engine.RunImageOneShot(wait, m.Docker, engine.ImageOneShot{
		Image: s.Image, Argv: slices.Concat([]string{"--login"}, args), Volume: volume, Target: s.VolumeTarget, Env: env,
		Egress: []string{microsoftLoginHost}, ProxyImage: m.RunnerImage, Runtime: m.Policy.Docker.Runtime, Limits: m.Policy.Docker.Limits,
		Stdout: relay,
	})
	if err != nil {
		if wait.Err() != nil && ctx.Err() == nil {
			return "", fmt.Errorf("no sign-in at Microsoft within %s", MicrosoftSignInTimeout)
		}
		return "", err
	}
	if code != 0 {
		return "", errors.New(microsoftRefusal(stderr))
	}
	if !relay.shown() {
		return "", errors.New("the Microsoft 365 server gave no sign-in code")
	}

	out, stderr, code, err := engine.RunImageOneShot(ctx, m.Docker, engine.ImageOneShot{
		Image: s.Image, Argv: slices.Concat([]string{"--list-accounts"}, args), Volume: volume, Target: s.VolumeTarget,
		Env:     map[string]string{s.ClientEnv: in.ClientID, s.TenantEnv: tenant, assistant.MS365CacheKey: key},
		Runtime: m.Policy.Docker.Runtime, Limits: m.Policy.Docker.Limits,
	})
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("list the server's accounts: %s", microsoftRefusal(stderr))
	}
	account, err := ms365Account(out, email)
	if err != nil {
		return "", err
	}
	if err := m.record(s.Key, func(r *ImageSignIn) {
		r.Account, r.At, r.Granted, r.Tenant = account, time.Now().UTC(), granted, tenant
	}); err != nil {
		return "", err
	}
	return account, nil
}

// ms365Account is the account of the server's account list that is email;
// a list that does not name it is an error, so a sign-in is recorded only
// once the server holds it.
func ms365Account(list, email string) (string, error) {
	var accounts []struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(lastLine(list))), &accounts); err != nil {
		return "", fmt.Errorf("read the server's accounts: %w", err)
	}
	for _, a := range accounts {
		if strings.EqualFold(a.Username, email) {
			return strings.ToLower(a.Username), nil
		}
	}
	if len(accounts) > 0 {
		return "", fmt.Errorf("the sign-in at Microsoft was made as %s, not %s: sign in again with the account you named", accounts[0].Username, email)
	}
	return "", errors.New("the Microsoft 365 server holds no account after the sign-in")
}

// lastLine is s's last non-empty line, where the server prints its answer.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// microsoftRefusal is why a sign-in failed, in the person's words where it
// is one Microsoft gives often: an organization that blocks sign-in by
// device code, an app not set up for it, or consent refused.
func microsoftRefusal(stderr string) string {
	switch {
	case strings.Contains(stderr, "AADSTS50199") || strings.Contains(stderr, "AADSTS53003") || strings.Contains(strings.ToLower(stderr), "device code flow is blocked"):
		return "your organization does not allow signing in by device code (a Conditional Access policy): ask its administrator, or wait for Agentry's own Microsoft app"
	case strings.Contains(stderr, "AADSTS7000218") || strings.Contains(stderr, "AADSTS700016"):
		return "Microsoft refused the Entra app: check its id, that it allows public client flows, and that it accepts the kind of account you signed in with"
	case strings.Contains(stderr, "AADSTS65004") || strings.Contains(stderr, "AADSTS65001"):
		return "the permission the sign-in asks for was not granted"
	case strings.Contains(stderr, "authorization_pending") || strings.Contains(stderr, "expired_token") || strings.Contains(stderr, "code_expired"):
		return "the code was not entered before it expired: sign in again"
	}
	msg := strings.TrimSpace(lastLine(stderr))
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	if msg == "" {
		msg = "the server exited with no reason given"
	}
	return "the sign-in at Microsoft failed: " + msg
}

// deviceCodeRelay reads the sign-in's output as it comes and relays the
// device code once: it says the code and the page, and opens the page if
// it is Microsoft's own. Nothing else of the output is shown.
type deviceCodeRelay struct {
	say    func(string, ...any)
	browse func(string) error

	mu   sync.Mutex
	buf  strings.Builder
	done bool
}

func (r *deviceCodeRelay) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return len(p), nil
	}
	if r.buf.Len() < 64<<10 {
		r.buf.Write(p)
	}
	sc := bufio.NewScanner(strings.NewReader(r.buf.String()))
	for sc.Scan() {
		m := deviceCodeRE.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		page, code := strings.TrimRight(m[1], ".,"), m[2]
		if !deviceLoginPage(page) {
			r.say("The sign-in named a page that is not Microsoft's own (%s); it is not opened.", page)
			continue
		}
		r.done = true
		r.say("To sign in, open %s and enter the code %s, then sign in with your Microsoft account and allow Agentry. This waits for you.", page, code)
		if r.browse != nil {
			if err := r.browse(page); err != nil {
				r.say("The browser did not open (%v); go to the address above.", err)
			}
		}
		break
	}
	return len(p), nil
}

func (r *deviceCodeRelay) shown() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}
