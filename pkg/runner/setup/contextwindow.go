package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ContextWindow is a model's context window as an endpoint reported it: the
// tokens, and the URL that said so. Tokens 0 is an endpoint that did not say.
type ContextWindow struct {
	Tokens int64
	Source string
}

// contextProbeTimeout bounds each of the probe's requests: a server that does
// not answer one of them is taken to have nothing to say there.
const contextProbeTimeout = 10 * time.Second

// ProbeContextWindow asks an OpenAI-compatible endpoint how large model's
// context window is, in the shapes the common servers publish, and returns
// the first it finds:
//
//   - the model's entry in <baseURL>/models: max_model_len (vLLM),
//     loaded_context_length, context_length, max_context_length or
//     context_window;
//   - LM Studio's /api/v0/models/<model>: the loaded window, else the
//     model's largest;
//   - llama.cpp's /props: the window the server was started with;
//   - Ollama's /api/ps (the running model's window), then /api/show's
//     num_ctx parameter.
//
// The server paths are read beside the base URL's /v1, on the same host. A
// training window (llama.cpp's n_ctx_train, Ollama's <arch>.context_length)
// is not read: a server runs with less, and a window larger than the one it
// enforces would hold compaction back past the point it refuses a request.
//
// The requests follow ListModels' rules: http or https only, no redirect
// followed, the key (when there is one) sent as a bearer to the endpoint's
// host alone and scrubbed from any error. Only a base URL that cannot be
// asked is an error; an endpoint that answers none of the shapes is a zero
// ContextWindow.
func ProbeContextWindow(ctx context.Context, baseURL, model, key string) (ContextWindow, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if _, err := checkScheme(base + "/models"); err != nil {
		return ContextWindow{}, fmt.Errorf("context window: %w", withoutKey(err, key))
	}
	if model == "" {
		return ContextWindow{}, nil
	}
	root := strings.TrimSuffix(base, "/v1")
	key = strings.TrimSpace(key)
	tries := []struct {
		method, url string
		body        any
		read        func([]byte) int64
	}{
		{http.MethodGet, base + "/models", nil, func(b []byte) int64 { return listedWindow(b, model) }},
		{http.MethodGet, root + "/api/v0/models/" + url.PathEscape(model), nil, lmStudioWindow},
		{http.MethodGet, root + "/props", nil, llamaCppWindow},
		{http.MethodGet, root + "/api/ps", nil, func(b []byte) int64 { return ollamaRunningWindow(b, model) }},
		{http.MethodPost, root + "/api/show", map[string]string{"model": model}, ollamaShowWindow},
	}
	for _, t := range tries {
		if err := ctx.Err(); err != nil {
			return ContextWindow{}, err
		}
		body, ok := probeGet(ctx, t.method, t.url, t.body, key)
		if !ok {
			continue
		}
		if n := t.read(body); n > 0 {
			return ContextWindow{Tokens: n, Source: t.url}, nil
		}
	}
	return ContextWindow{}, nil
}

// probeGet makes one of the probe's requests and returns the body of a 200.
func probeGet(ctx context.Context, method, endpoint string, payload any, key string) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(ctx, contextProbeTimeout)
	defer cancel()
	var rd io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, false
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rd)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		setKey(req, keyHeaderBearer, key)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, modelsMaxBody))
	return b, err == nil
}

// listedWindow reads model's entry in an OpenAI-shaped listing.
func listedWindow(body []byte, model string) int64 {
	var page struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &page) != nil {
		return 0
	}
	for _, m := range page.Data {
		var id string
		if json.Unmarshal(m["id"], &id) != nil || id != model {
			continue
		}
		for _, field := range []string{"max_model_len", "loaded_context_length", "context_length", "max_context_length", "context_window"} {
			if n := positiveInt(m[field]); n > 0 {
				return n
			}
		}
	}
	return 0
}

func lmStudioWindow(body []byte) int64 {
	var m struct {
		Loaded json.RawMessage `json:"loaded_context_length"`
		Max    json.RawMessage `json:"max_context_length"`
	}
	if json.Unmarshal(body, &m) != nil {
		return 0
	}
	if n := positiveInt(m.Loaded); n > 0 {
		return n
	}
	return positiveInt(m.Max)
}

func llamaCppWindow(body []byte) int64 {
	var p struct {
		Settings struct {
			NCtx json.RawMessage `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if json.Unmarshal(body, &p) != nil {
		return 0
	}
	return positiveInt(p.Settings.NCtx)
}

// ollamaRunningWindow reads the window a loaded model runs with. Ollama
// names a model without a tag as its :latest.
func ollamaRunningWindow(body []byte, model string) int64 {
	var ps struct {
		Models []struct {
			Name          string          `json:"name"`
			Model         string          `json:"model"`
			ContextLength json.RawMessage `json:"context_length"`
		} `json:"models"`
	}
	if json.Unmarshal(body, &ps) != nil {
		return 0
	}
	want := model
	if !strings.Contains(want, ":") {
		want += ":latest"
	}
	for _, m := range ps.Models {
		if m.Name == model || m.Model == model || m.Name == want || m.Model == want {
			return positiveInt(m.ContextLength)
		}
	}
	return 0
}

// ollamaShowWindow reads the num_ctx a model's Modelfile sets, the window
// Ollama loads it with.
func ollamaShowWindow(body []byte) int64 {
	var s struct {
		Parameters string `json:"parameters"`
	}
	if json.Unmarshal(body, &s) != nil {
		return 0
	}
	for line := range strings.SplitSeq(s.Parameters, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "num_ctx" {
			if n, err := strconv.ParseInt(f[1], 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// positiveInt reads a JSON number that is a whole, positive count.
func positiveInt(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil || f < 1 || f != float64(int64(f)) {
		return 0
	}
	return int64(f)
}
