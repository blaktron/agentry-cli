package setup

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// The answers below are the shapes each server publishes, trimmed to the
// fields the probe reads and one neighbour.
func TestProbeContextWindowReadsEachServersShape(t *testing.T) {
	cases := []struct {
		name   string
		routes map[string]string
		want   int64
		source string
	}{
		{"vllm models listing", map[string]string{
			"GET /v1/models": `{"object":"list","data":[{"id":"other","max_model_len":8192},{"id":"qwen3","object":"model","max_model_len":262144}]}`,
		}, 262144, "/v1/models"},
		{"a listing's context_length", map[string]string{
			"GET /v1/models": `{"data":[{"id":"qwen3","context_length":131072}]}`,
		}, 131072, "/v1/models"},
		{"lm studio, loaded", map[string]string{
			"GET /v1/models":           `{"data":[{"id":"qwen3","object":"model","owned_by":"organization_owner"}]}`,
			"GET /api/v0/models/qwen3": `{"id":"qwen3","state":"loaded","max_context_length":262144,"loaded_context_length":65536}`,
		}, 65536, "/api/v0/models/qwen3"},
		{"lm studio, not loaded", map[string]string{
			"GET /api/v0/models/qwen3": `{"id":"qwen3","state":"not-loaded","max_context_length":262144}`,
		}, 262144, "/api/v0/models/qwen3"},
		{"llama.cpp server", map[string]string{
			"GET /v1/models": `{"data":[{"id":"qwen3","meta":{"n_ctx_train":262144}}]}`,
			"GET /props":     `{"default_generation_settings":{"n_ctx":32768,"temperature":0.8},"total_slots":1}`,
		}, 32768, "/props"},
		{"ollama, running", map[string]string{
			"GET /api/ps": `{"models":[{"name":"other:7b","model":"other:7b","context_length":4096},{"name":"qwen3:30b","model":"qwen3:30b","context_length":40960}]}`,
		}, 40960, "/api/ps"},
		{"ollama, num_ctx in the modelfile", map[string]string{
			"GET /api/ps":    `{"models":[]}`,
			"POST /api/show": `{"parameters":"stop \"<|im_end|>\"\nnum_ctx                        16384","model_info":{"qwen3.context_length":262144}}`,
		}, 16384, "/api/show"},
		{"nothing to read", map[string]string{
			"GET /v1/models": `{"data":[{"id":"qwen3"}]}`,
		}, 0, ""},
	}
	model := "qwen3"
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := model
			if strings.HasPrefix(c.name, "ollama") {
				m = "qwen3:30b"
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, ok := c.routes[r.Method+" "+r.URL.EscapedPath()]
				if !ok {
					http.NotFound(w, r)
					return
				}
				if r.URL.Path == "/api/show" {
					b, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(b), `"model":"`+m+`"`) {
						http.Error(w, "which model?", http.StatusBadRequest)
						return
					}
				}
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			got, err := ProbeContextWindow(context.Background(), srv.URL+"/v1", m, "")
			if err != nil {
				t.Fatal(err)
			}
			if got.Tokens != c.want || (c.source != "" && got.Source != srv.URL+c.source) {
				t.Errorf("got %+v, want %d from %s", got, c.want, srv.URL+c.source)
			}
		})
	}
}

// A model the listing does not name is not sized by another model's entry,
// and the training window a llama.cpp listing carries is not the window the
// server runs with.
func TestProbeContextWindowReadsOnlyTheModelsOwnLoadedWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"other","max_model_len":8192},{"id":"qwen3","meta":{"n_ctx_train":262144}}]}`)
		case "/api/show":
			_, _ = io.WriteString(w, `{"parameters":"","model_info":{"qwen3.context_length":262144}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	got, err := ProbeContextWindow(context.Background(), srv.URL+"/v1", "qwen3", "")
	if err != nil || got.Tokens != 0 {
		t.Fatalf("got %+v %v, want nothing", got, err)
	}
}

// The key goes only where the listing sends it (the endpoint's own host),
// as a bearer, and never into an error.
func TestProbeContextWindowSendsTheKeyAsABearerAndScrubsIt(t *testing.T) {
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		http.Redirect(w, r, "http://elsewhere.invalid/", http.StatusFound)
	}))
	defer srv.Close()
	got, err := ProbeContextWindow(context.Background(), srv.URL+"/v1", "qwen3", "sk-local-secret")
	if err != nil || got.Tokens != 0 {
		t.Fatalf("a redirect is not followed and finds nothing: %+v %v", got, err)
	}
	if len(auths) == 0 {
		t.Fatal("no request was made")
	}
	for _, a := range auths {
		if a != "Bearer sk-local-secret" {
			t.Errorf("Authorization %q, want the bearer", a)
		}
	}
}

func TestProbeContextWindowRefusesANonHTTPEndpoint(t *testing.T) {
	if _, err := ProbeContextWindow(context.Background(), "file:///etc/v1", "m", ""); err == nil {
		t.Fatal("a file URL was probed")
	}
}

func probeAnswering(w ContextWindow, err error, asked *int) func(context.Context, string, string, string) (ContextWindow, error) {
	return func(context.Context, string, string, string) (ContextWindow, error) {
		*asked++
		return w, err
	}
}

// Setup writes the window a custom model needs: the flag's, else the
// endpoint's, else the person's answer; and says so when none is known.
func TestConfigureModelSettlesTheContextWindow(t *testing.T) {
	t.Setenv("K", "set")
	const base = "http://127.0.0.1:1234/v1"
	reported := ContextWindow{Tokens: 262144, Source: "http://127.0.0.1:1234/v1/models"}
	cases := []struct {
		name    string
		opts    ModelOpts
		answers []string
		probe   ContextWindow
		before  *policy.Model
		want    int64
		probed  int
		warned  bool
	}{
		{name: "the flag wins, unasked", opts: ModelOpts{ContextWindow: 65536, Yes: true}, probe: reported, want: 65536},
		{name: "the endpoint's, under yes", opts: ModelOpts{Yes: true}, probe: reported, want: 262144, probed: 1},
		{name: "nothing reported, under yes", opts: ModelOpts{Yes: true}, want: 0, probed: 1, warned: true},
		{name: "the endpoint's is the default", answers: []string{""}, probe: reported, want: 262144, probed: 1},
		{name: "the person's answer", answers: []string{"131_072"}, want: 131072, probed: 1},
		{name: "the person does not know", answers: []string{"0"}, probe: reported, want: 0, probed: 1, warned: true},
		{name: "the same model keeps its window, unprobed",
			opts:   ModelOpts{Yes: true},
			before: &policy.Model{Provider: "custom", Name: "m", APIKeyEnv: "K", BaseURL: base, ContextWindow: 40960, ProxyAllow: []string{"127.0.0.1:1234"}},
			probe:  reported, want: 40960},
		{name: "another model is probed again",
			opts:   ModelOpts{Yes: true, Model: "n"},
			before: &policy.Model{Provider: "custom", Name: "m", APIKeyEnv: "K", BaseURL: base, ContextWindow: 40960, ProxyAllow: []string{"127.0.0.1:1234"}},
			probe:  reported, want: 262144, probed: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pol := policy.Default()
			if c.before != nil {
				pol.Model = *c.before
			}
			opts := c.opts
			opts.Provider, opts.APIKeyEnv, opts.BaseURL, opts.APIKeySource = "custom", "K", base, "env"
			if opts.Model == "" {
				opts.Model = "m"
			}
			probed := 0
			opts.ProbeContextWindow = probeAnswering(c.probe, nil, &probed)
			p := &scriptPrompter{answers: c.answers}
			if err := ConfigureModel(p, pol, opts); err != nil {
				t.Fatal(err)
			}
			if pol.Model.ContextWindow != c.want || probed != c.probed {
				t.Errorf("window %d after %d probes, want %d after %d", pol.Model.ContextWindow, probed, c.want, c.probed)
			}
			warned := strings.Contains(strings.Join(p.said, "\n"), "context window is unknown")
			if warned != c.warned {
				t.Errorf("warned=%v, want %v (said %q)", warned, c.warned, p.said)
			}
			if err := pol.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// openai pointed at a base URL is an endpoint of the person's own too.
func TestConfigureModelAsksTheWindowOfAnOpenAIBaseURL(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "set")
	pol := policy.Default()
	probed := 0
	if err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "openai", Model: "qwen3", BaseURL: "http://127.0.0.1:8000/v1", Yes: true,
		ProbeContextWindow: probeAnswering(ContextWindow{Tokens: 32768}, nil, &probed)}); err != nil {
		t.Fatal(err)
	}
	if pol.Model.ContextWindow != 32768 || probed != 1 {
		t.Fatalf("window %d after %d probes", pol.Model.ContextWindow, probed)
	}
}

// A table provider is not asked: the harness sizes its models itself, and a
// window the policy names for the same model is kept.
func TestConfigureModelAsksNoWindowForATableProvider(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "set")
	pol := policy.Default()
	probed := 0
	p := &scriptPrompter{}
	if err := ConfigureModel(p, pol, ModelOpts{Provider: "openai", Yes: true, ProbeContextWindow: probeAnswering(ContextWindow{Tokens: 9999}, nil, &probed)}); err != nil {
		t.Fatal(err)
	}
	if pol.Model.ContextWindow != 0 || probed != 0 || strings.Contains(strings.Join(p.said, "\n"), "context window") {
		t.Fatalf("window %d, %d probes, said %q", pol.Model.ContextWindow, probed, p.said)
	}
	if err := ConfigureModel(p, pol, ModelOpts{Provider: "openai", ContextWindow: 400000, Yes: true}); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureModel(p, pol, ModelOpts{Provider: "openai", Yes: true}); err != nil || pol.Model.ContextWindow != 400000 {
		t.Fatalf("the same model's window is kept: %d %v", pol.Model.ContextWindow, err)
	}
}

// A window no model has is an endpoint's mistake: it is said and not taken.
func TestConfigureModelDoesNotTakeAWindowOutOfBounds(t *testing.T) {
	t.Setenv("K", "set")
	pol := policy.Default()
	probed := 0
	p := &scriptPrompter{}
	if err := ConfigureModel(p, pol, ModelOpts{Provider: "custom", Model: "m", APIKeyEnv: "K", BaseURL: "http://127.0.0.1:1234/v1", Yes: true,
		ProbeContextWindow: probeAnswering(ContextWindow{Tokens: 1 << 62, Source: "http://127.0.0.1:1234/props"}, nil, &probed)}); err != nil {
		t.Fatal(err)
	}
	said := strings.Join(p.said, "\n")
	if pol.Model.ContextWindow != 0 || !strings.Contains(said, "not taken") || !strings.Contains(said, "context window is unknown") {
		t.Fatalf("window %d, said %q", pol.Model.ContextWindow, p.said)
	}
	if err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "custom", Model: "m", APIKeyEnv: "K", BaseURL: "http://127.0.0.1:1234/v1", ContextWindow: policy.MaxContextWindow + 1, Yes: true}); err == nil {
		t.Fatal("a flag outside the bounds was taken")
	}
}

// The source setup prints carries no user information or query from the
// base URL.
func TestProbeContextWindowSourceCarriesNoCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"m","max_model_len":8192}]}`)
	}))
	defer srv.Close()
	base := strings.Replace(srv.URL, "http://", "http://user:pw@", 1) + "/v1?token=abc"
	got, err := ProbeContextWindow(context.Background(), base, "m", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Tokens != 8192 || strings.Contains(got.Source, "pw") || strings.Contains(got.Source, "abc") {
		t.Fatalf("got %+v", got)
	}
}

func TestConfigureModelRefusesAWindowTooSmall(t *testing.T) {
	pol := policy.Default()
	err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "custom", Model: "m", APIKeyEnv: "K", BaseURL: "http://127.0.0.1:1234/v1", ContextWindow: 512, Yes: true})
	if err == nil || !strings.Contains(err.Error(), "context window") {
		t.Fatalf("got %v", err)
	}
	p := &scriptPrompter{answers: []string{"lots"}}
	err = ConfigureModel(p, pol, ModelOpts{Provider: "custom", Model: "m", APIKeyEnv: "K", APIKeySource: "env", BaseURL: "http://127.0.0.1:1234/v1"})
	if err == nil || !strings.Contains(err.Error(), "context window") {
		t.Fatalf("got %v", err)
	}
}
