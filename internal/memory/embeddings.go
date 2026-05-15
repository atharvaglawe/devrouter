package memory

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"sync"
	"time"
)

// EmbedDim is the vector dimension every Provider must produce.
//
// It is hardcoded because the Redis FT.SEARCH index is created with this
// dim baked in (see Store.EnsureIndexes). Switching to a model whose
// vectors have a different dim requires recreating the index — there is
// no way to migrate it in place.
const EmbedDim = 768

// Provider produces a fixed-dim embedding for a single text.
//
// All implementations must return a vector of length EmbedDim. The
// package-level Embed() wrapper enforces this and surfaces a clear error
// if the contract is violated, so a misconfigured model fails loudly
// instead of silently corrupting the Redis index.
type Provider interface {
	// Embed returns a normalized (or model-native) float32 vector for text.
	Embed(text string) ([]float32, error)
	// Name is a short identifier used in log lines and error messages
	// (e.g. "nomic-embed-text-v1.5@http://localhost:11435"). Purely for diagnostics.
	Name() string
}

// HTTPProvider talks to any endpoint that speaks the canonical
// /api/embed wire shape:
//
//	request : {"model": "<model>", "input": "<text>"}
//	response: {"embeddings": [[...float32...]]}
//
// The bundled `embedder/` Docker image serves this; so does Ollama, any
// in-house ONNX worker, etc. Picking one wire format keeps the client
// surface tiny and the failure modes obvious.
type HTTPProvider struct {
	URL    string
	Model  string
	Client *http.Client
}

// NewHTTPProvider returns a provider with a sane default HTTP timeout.
// The 30s budget matches the prior hardcoded behavior; embedding calls
// happen on memory writes and on every dev_context query, both of which
// are user-visible, so a long timeout is preferable to a hang.
func NewHTTPProvider(url, model string) *HTTPProvider {
	return &HTTPProvider{
		URL:    url,
		Model:  model,
		Client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (p *HTTPProvider) Name() string {
	return fmt.Sprintf("%s@%s", p.Model, p.URL)
}

type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

func (p *HTTPProvider) Embed(text string) ([]float32, error) {
	body, _ := json.Marshal(embedRequest{Model: p.Model, Input: text})

	resp, err := p.Client.Post(p.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed request to %s: %w", p.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed endpoint %s returned %d", p.URL, resp.StatusCode)
	}

	var result embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("embed decode from %s: %w", p.URL, err)
	}

	if len(result.Embeddings) == 0 || len(result.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("embed endpoint %s returned no vectors for model %q", p.URL, p.Model)
	}

	return result.Embeddings[0], nil
}

// --- Default-provider plumbing ----------------------------------------------

var (
	defaultProviderOnce sync.Once
	defaultProviderMu   sync.RWMutex
	defaultProvider     Provider
)

// Embed is a thin wrapper around the configured default provider, kept
// for backwards compatibility with the many call sites that just want
// "give me a vector for this text".
//
// The first call lazy-initializes the default provider from env vars
// (see initDefaultProvider). Subsequent calls reuse it. Tests can swap
// the provider with SetProvider.
func Embed(text string) ([]float32, error) {
	defaultProviderOnce.Do(initDefaultProvider)

	defaultProviderMu.RLock()
	p := defaultProvider
	defaultProviderMu.RUnlock()

	if p == nil {
		// Should be unreachable in production (initDefaultProvider always
		// assigns a non-nil HTTPProvider). Belt-and-braces against tests
		// or future code that swap nil in via SetProvider.
		return nil, fmt.Errorf("embedding provider not initialized")
	}

	vec, err := p.Embed(text)
	if err != nil {
		return nil, err
	}
	if len(vec) != EmbedDim {
		return nil, fmt.Errorf(
			"embedding provider %s returned %d-dim vector, want %d "+
				"(Redis index is built for %d; either use a %d-dim model or recreate the index with a different EmbedDim)",
			p.Name(), len(vec), EmbedDim, EmbedDim, EmbedDim)
	}
	return vec, nil
}

// SetProvider overrides the default provider. Mainly for tests and for
// programmatic configuration that doesn't go through env vars.
//
// It also marks the lazy-init Once as done so the env-var path won't
// later overwrite the provided value.
func SetProvider(p Provider) {
	defaultProviderMu.Lock()
	defaultProvider = p
	defaultProviderMu.Unlock()
	// Mark the once as already-done so initDefaultProvider is skipped.
	defaultProviderOnce.Do(func() {})
}

// initDefaultProvider builds the default HTTPProvider from env vars.
// Default points at the bundled Dockerized ONNX embedder on port 11435
// (see embedder/README.md and `make embedder-up`). The model name sent
// in the request is "nomic-embed-text-v1.5" — the ONNX embedder ignores
// the model field (it serves exactly one model per container) but the
// value is preserved for log/diagnostic clarity.
func initDefaultProvider() {
	url := os.Getenv("DEVROUTER_EMBEDDING_URL")
	if url == "" {
		url = "http://localhost:11435/api/embed"
	}
	model := os.Getenv("DEVROUTER_EMBEDDING_MODEL")
	if model == "" {
		model = "nomic-embed-text-v1.5"
	}

	defaultProviderMu.Lock()
	defaultProvider = NewHTTPProvider(url, model)
	defaultProviderMu.Unlock()

	log.Printf("[memory] embedding provider: model=%s url=%s dim=%d", model, url, EmbedDim)
}

// --- Vector serialization (unchanged) ---------------------------------------

// Float32ToBytes converts a float32 slice to little-endian bytes for Redis HSET.
func Float32ToBytes(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}
