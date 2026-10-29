package setup

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// Vultr's listing, recorded from its /models endpoint, names speech,
// image, video, embedding, rerank and decision models beside the text
// models; only a text model that takes tools is offered, with its price a
// million tokens, and the key is a bearer.
func TestListModelsVultrOffersOnlyTextModelsThatTakeTools(t *testing.T) {
	recorded, err := os.ReadFile("testdata/vultr-models.json")
	if err != nil {
		t.Fatal(err)
	}
	f, srv := newFakeModels(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(recorded)
	})
	pointProviderAt(t, policy.ProviderVultr, srv.URL+"/v1/models")
	c, err := ListModels(context.Background(), "vultr", "", "vultr-secret")
	if err != nil {
		t.Fatal(err)
	}
	want := "deepseek-v4.1-flash-1017,glm-5.2,glm-5.3,glm-5.3-flash,mimo-v2.6-flash,mimo-v2.6-pro,minimax-m3.1-flash," +
		"muse-glimmer-30b,nemotron-3-nano-omni-30b-a3b-reasoning,qwen3.8-27b,qwen3.8-flash-next,raon-1-mini-preview"
	if got := strings.Join(ids(c), ","); got != want {
		t.Fatalf("ids =\n%s\nwant\n%s", got, want)
	}
	var flash Model
	for _, m := range c.Models {
		if m.ID == "qwen3.8-flash-next" {
			flash = m
		}
	}
	if flash.DisplayName != "Qwen 3.8 Flash Next" || flash.InputUSD != "0.10" || flash.OutputUSD != "0.20" {
		t.Errorf("qwen3.8-flash-next = %+v", flash)
	}
	if f.headers[0].Get("Authorization") != "Bearer vultr-secret" {
		t.Errorf("headers = %v", f.headers[0])
	}
}

// A listing with no model a run can use is an error, not an empty form.
func TestListModelsVultrWithNoToolModelIsAnError(t *testing.T) {
	_, srv := newFakeModels(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []map[string]any{{"id": "qwen3-embedding-4b", "output_modalities": []map[string]any{{"type": "embeddings"}}}}})
	})
	pointProviderAt(t, policy.ProviderVultr, srv.URL+"/v1/models")
	if _, err := ListModels(context.Background(), "vultr", "", "vultr-secret"); err == nil {
		t.Fatal("an embeddings-only listing was accepted")
	}
}

// Setup by name writes Vultr's provider, its key variable and its egress
// host, and no base URL: the adapter supplies Vultr's endpoint.
func TestConfigureModelVultr(t *testing.T) {
	pol := policy.Default()
	err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "vultr", Model: "qwen3.8-flash-next", Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	m := pol.Model
	if m.Provider != "vultr" || m.Name != "qwen3.8-flash-next" || m.APIKeyEnv != "VULTR_INFERENCE_API_KEY" || m.BaseURL != "" {
		t.Errorf("model = %+v", m)
	}
	if strings.Join(m.ProxyAllow, ",") != "api.vultrinference.com:443" {
		t.Errorf("proxyAllow = %v", m.ProxyAllow)
	}
}
