package persistent

// Google Workspace's sign-in, through gog in one-shots of its own image on
// its own volume: the person's Google Cloud OAuth client, read from a file
// and handed to gog on stdin, which keeps the client id on the volume and
// the secret in its encrypted file keyring; then gog's two-step remote
// sign-in, its first step with no network and its second reaching Google's
// token endpoint alone, with the CLI itself listening on this computer's
// 127.0.0.1 for the browser's redirect. The keyring's password is kept in
// the keychain. Nothing of the client or the tokens is written anywhere
// else, and the sign-in is recorded only once gog names the account.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// The bounds of a Google sign-in.
const (
	// maxGoogleClient bounds the client file: Google's are under 1 KiB.
	maxGoogleClient = 64 << 10
	// GoogleSignInTimeout is how long the sign-in waits for the browser.
	GoogleSignInTimeout = 10 * time.Minute
	// googleCallbackPath is the redirect's path, as gog builds it.
	googleCallbackPath = "/oauth2/callback"
	// googleTokenHost is the one host the code's exchange reaches.
	googleTokenHost = "oauth2.googleapis.com:443"
)

// googleClientIDRE is a Google Cloud OAuth client id.
var googleClientIDRE = regexp.MustCompile(`^[0-9]+-[a-z0-9]+\.apps\.googleusercontent\.com$`)

// googleEmailRE is an email address gog can sign in with: one @, a dotted
// domain, no space or control character, and a first character that is not
// "-", so the address is never read as one of gog's options.
var googleEmailRE = regexp.MustCompile(`^[^@\s\x00-\x1f\x7f-][^@\s\x00-\x1f\x7f]{0,63}@[a-z0-9.-]{1,189}\.[a-z]{2,63}$`)

// CheckGoogleClient refuses a file that is not a Google Cloud OAuth client
// for a desktop app: a JSON object with an "installed" client naming its id
// and secret. A web application's client cannot sign in to a redirect on
// 127.0.0.1, and a service account is no client at all.
func CheckGoogleClient(b []byte) error {
	if len(b) == 0 || len(b) > maxGoogleClient {
		return fmt.Errorf("the Google client file must hold between 1 byte and %d KiB", maxGoogleClient>>10)
	}
	var f struct {
		Installed *struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		} `json:"installed"`
		Web  json.RawMessage `json:"web"`
		Type string          `json:"type"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("the Google client file is not JSON: %w", err)
	}
	switch {
	case f.Type == "service_account":
		return errors.New("the file is a service account's key, not an OAuth client: make an OAuth client ID of type Desktop app")
	case f.Web != nil:
		return errors.New("the file is a web application's client: make an OAuth client ID of type Desktop app, whose sign-in can come back to this computer")
	case f.Installed == nil:
		return errors.New("the file is not a Google Cloud OAuth client for a desktop app (it has no \"installed\" client)")
	case !googleClientIDRE.MatchString(f.Installed.ClientID) || f.Installed.ClientSecret == "":
		return errors.New("the Google client file names no client id and secret of a Google Cloud OAuth client")
	}
	return nil
}

// Google is what Google Workspace's sign-in needs: the engine, the runner
// home and the agent, the policy (the server's image, the runner image and
// the containers' limits), and the keychain.
type Google struct {
	Docker      dockerx.Client
	Home        string
	Agent       Agent
	Policy      *policy.Policy
	RunnerImage string
	Keys        Secrets
	// Client is who calls, ClientCLI or ClientDesktop (ClientCLI when
	// unset): the agent's lock names it while the client or the sign-in
	// holds the agent.
	Client string
	// Say is where the steps are narrated.
	Say io.Writer
}

// acquire takes the agent's lock in the caller's name.
func (g Google) acquire() (*Owner, error) {
	client := g.Client
	if client == "" {
		client = ClientCLI
	}
	return Acquire(g.Home, g.Agent.Name, client)
}

func (g Google) say(format string, args ...any) {
	if g.Say != nil {
		_, _ = fmt.Fprintf(g.Say, format+"\n", args...)
	}
}

// server is the agent's Google Workspace server at the policy's image.
func (g Google) server() (offer.ImageServer, error) {
	for _, s := range g.Agent.Builtin.ImageServers(g.Policy.Images) {
		if s.Key == assistant.GogKey {
			return s, nil
		}
	}
	return offer.ImageServer{}, fmt.Errorf("the %s agent has no Google Workspace", g.Agent.Name)
}

// prepare makes the server's volume and its keyring password when either is
// missing, and returns the volume and the one-shots' environment. A volume
// whose password the keychain lost cannot be opened, so it is made again:
// it holds nothing but gog's own state, which the person signs in to again.
func (g Google) prepare(ctx context.Context, s offer.ImageServer) (volume string, env map[string]string, err error) {
	if g.Keys == nil {
		return "", nil, errors.New("the Google Workspace server keeps its sign-in under a password in the keychain, and this client cannot reach the keychain")
	}
	volume = ImageVolumeIn(g.Home, g.Agent.Name, s.Key)
	have, err := g.Docker.VolumesByLabel(ctx, engine.PersistentHomeLabel+"="+HomeID(g.Home))
	if err != nil {
		return "", nil, fmt.Errorf("list the %s agent's volumes: %w", g.Agent.Name, err)
	}
	account := ImageKeyringAccount(g.Agent.Name, s.Key)
	password, err := g.Keys.Get(account)
	if err != nil || password == "" {
		if slices.Contains(have, volume) {
			g.say("The keychain no longer has the password of the Google sign-in kept on this computer, so it starts again.")
			if err := g.Docker.VolumeRemove(ctx, volume); err != nil {
				return "", nil, fmt.Errorf("remove the Google sign-in that can no longer be opened: %w", err)
			}
			have = slices.DeleteFunc(have, func(v string) bool { return v == volume })
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", nil, err
		}
		password = base64.RawURLEncoding.EncodeToString(b)
		if err := g.Keys.Set(account, password); err != nil {
			return "", nil, fmt.Errorf("keep the Google sign-in's password in the keychain: %w", err)
		}
	}
	if !slices.Contains(have, volume) {
		if err := g.Docker.VolumeCreate(ctx, volume, engine.PersistentVolumeLabels(g.Agent.Name, HomeID(g.Home))); err != nil {
			return "", nil, fmt.Errorf("make the volume %s: %w", volume, err)
		}
	}
	env = map[string]string{}
	for k, v := range s.Env {
		env[k] = v
	}
	env[assistant.GogKeyringSecret] = password
	return volume, env, nil
}

// run runs gog with argv on the server's volume and returns its stdout.
func (g Google) run(ctx context.Context, s offer.ImageServer, volume string, env map[string]string, argv []string, stdin io.Reader, egress ...string) (string, error) {
	stdout, stderr, code, err := engine.RunImageOneShot(ctx, g.Docker, engine.ImageOneShot{
		Image: s.Image, Argv: argv, Volume: volume, Target: s.VolumeTarget, Env: env, Stdin: stdin,
		Egress: egress, ProxyImage: g.RunnerImage, Runtime: g.Policy.Docker.Runtime, Limits: g.Policy.Docker.Limits,
	})
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("gog %s exited %d: %s", argv[0]+" "+argv[1], code, gogMessage(stdout, stderr))
	}
	return stdout, nil
}

// gogMessage is the reason gog gave: its JSON error's message, else its
// last line of stderr, bounded.
func gogMessage(stdout, stderr string) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := ""
	if json.Unmarshal([]byte(stdout), &e) == nil {
		msg = e.Error.Message
	}
	if msg == "" {
		lines := strings.Split(strings.TrimSpace(stderr), "\n")
		msg = lines[len(lines)-1]
	}
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	return msg
}

// SetClient hands the person's Google Cloud OAuth client to gog, which
// keeps its id on the server's volume and its secret in its encrypted
// keyring; the file is read here and sent on stdin, never mounted. It is
// refused while the agent runs, since its server holds the volume.
func (g Google) SetClient(ctx context.Context, client []byte) error {
	if err := CheckGoogleClient(client); err != nil {
		return err
	}
	owner, err := g.acquire()
	if err != nil {
		return err
	}
	defer owner.Release()
	s, err := g.server()
	if err != nil {
		return err
	}
	volume, env, err := g.prepare(ctx, s)
	if err != nil {
		return err
	}
	if _, err := g.run(ctx, s, volume, env, []string{"auth", "credentials", "set", "-", "--json", "--no-input"}, strings.NewReader(string(client))); err != nil {
		return err
	}
	c, err := LoadConfig(g.Home, g.Agent.Name)
	if err != nil {
		return err
	}
	in := c.SignIns[s.Key]
	in.Client = time.Now().UTC()
	if c.SignIns == nil {
		c.SignIns = map[string]ImageSignIn{}
	}
	c.SignIns[s.Key] = in
	return SaveConfig(g.Home, g.Agent.Name, c)
}

// SignIn signs Google Workspace in to email with gog's two-step remote
// flow: the consent's address made with no network, opened in the browser
// by browse, the browser's redirect caught on 127.0.0.1, and the code
// exchanged reaching oauth2.googleapis.com alone. It asks for read-only
// scopes unless the agent has "allow changes" on. It returns the account
// gog names, and records it only then.
func (g Google) SignIn(ctx context.Context, email string, browse func(string) error) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !googleEmailRE.MatchString(email) {
		return "", fmt.Errorf("%q is not an email address a Google account signs in with", email)
	}
	owner, err := g.acquire()
	if err != nil {
		return "", err
	}
	defer owner.Release()
	s, err := g.server()
	if err != nil {
		return "", err
	}
	c, err := LoadConfig(g.Home, g.Agent.Name)
	if err != nil {
		return "", err
	}
	in := c.SignIns[s.Key]
	if in.Client.IsZero() {
		return "", fmt.Errorf("the Google Workspace server has no Google Cloud client yet: agentry agent configure %s --google-client <client file>", g.Agent.Name)
	}
	var granted []string
	for _, w := range s.Switches {
		if w.SignedFor && switchedOn(c, ImageSwitchEntryID(s.Key, w.Key), false) {
			granted = append(granted, w.Key)
		}
	}
	scope := []string{"--services", strings.Join(assistant.GogServices, ",")}
	if len(granted) == 0 {
		scope = append(scope, "--readonly")
	}
	volume, env, err := g.prepare(ctx, s)
	if err != nil {
		return "", err
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("listen for the sign-in's redirect: %w", err)
	}
	defer func() { _ = l.Close() }()
	redirect := fmt.Sprintf("http://%s%s", l.Addr().String(), googleCallbackPath)

	out, err := g.run(ctx, s, volume, env, slices.Concat([]string{"auth", "add", email}, scope,
		[]string{"--remote", "--step", "1", "--redirect-uri", redirect, "--force-consent", "--json", "--no-input"}), nil)
	if err != nil {
		return "", err
	}
	var step1 struct {
		AuthURL string `json:"auth_url"`
	}
	if err := json.Unmarshal([]byte(out), &step1); err != nil || !strings.HasPrefix(step1.AuthURL, "https://accounts.google.com/") {
		return "", errors.New("gog did not give a Google sign-in address")
	}

	got := make(chan string, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != googleCallbackPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<!doctype html><title>Agentry</title><p>Google answered. You can close this tab and go back to Agentry.</p>")
		select {
		case got <- "http://" + l.Addr().String() + r.URL.RequestURI():
		default:
		}
	})}
	go func() { _ = srv.Serve(l) }()
	defer func() { _ = srv.Close() }()

	g.say("Opening Google's sign-in in your browser. If it does not open, go to:\n%s", step1.AuthURL)
	if browse != nil {
		if err := browse(step1.AuthURL); err != nil {
			g.say("The browser did not open (%v); go to the address above.", err)
		}
	}
	wait, cancel := context.WithTimeout(ctx, GoogleSignInTimeout)
	defer cancel()
	var back string
	select {
	case back = <-got:
	case <-wait.Done():
		return "", fmt.Errorf("no answer from Google's sign-in within %s", GoogleSignInTimeout)
	}
	if u, err := url.Parse(back); err == nil {
		if e := u.Query().Get("error"); e != "" {
			return "", fmt.Errorf("the sign-in at Google answered %s", e)
		}
	}

	if _, err := g.run(ctx, s, volume, env, slices.Concat([]string{"auth", "add", email}, scope,
		// gog matches the second step to the first by these, as given.
		[]string{"--remote", "--step", "2", "--redirect-uri", redirect, "--force-consent", "--auth-url", back, "--json", "--no-input"}), nil, googleTokenHost); err != nil {
		return "", err
	}
	out, err = g.run(ctx, s, volume, env, []string{"auth", "list", "--json", "--no-input"}, nil)
	if err != nil {
		return "", err
	}
	account, err := gogAccount(out, email)
	if err != nil {
		return "", err
	}
	c, err = LoadConfig(g.Home, g.Agent.Name)
	if err != nil {
		return "", err
	}
	if c.SignIns == nil {
		c.SignIns = map[string]ImageSignIn{}
	}
	c.SignIns[s.Key] = ImageSignIn{Client: in.Client, Account: account, At: time.Now().UTC(), Granted: granted}
	if err := SaveConfig(g.Home, g.Agent.Name, c); err != nil {
		return "", err
	}
	return account, nil
}

// gogAccount is the account of gog's account list that is email, as gog
// names it; an answer that does not name it is an error, so a sign-in is
// recorded only once gog holds it.
func gogAccount(list, email string) (string, error) {
	var l struct {
		Accounts []struct {
			Email   string `json:"email"`
			Account string `json:"account"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(list), &l); err != nil {
		return "", fmt.Errorf("read gog's accounts: %w", err)
	}
	for _, a := range l.Accounts {
		for _, name := range []string{a.Email, a.Account} {
			if strings.EqualFold(name, email) {
				return name, nil
			}
		}
	}
	return "", fmt.Errorf("gog does not hold %s after the sign-in", email)
}
