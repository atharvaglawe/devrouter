package memory

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// makeFloat32Vec returns a length-n slice of float32 with deterministic
// values so equality checks in tests are easy to read.
func makeFloat32Vec(n int) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = float32(i) * 0.001
	}
	return v
}

// fakeEmbedServer returns an httptest server speaking the /api/embed wire
// shape, returning vectors of length dim. It also captures the last
// request body so tests can assert on the model field, etc.
func fakeEmbedServer(t *testing.T, dim int, status int) (*httptest.Server, *embedRequest) {
	t.Helper()
	captured := &embedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, captured)

		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		resp := embedResponse{Embeddings: [][]float32{makeFloat32Vec(dim)}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

// TestHTTPProviderEmbedSuccess covers the happy path: a 200 with the
// expected wire shape returns the vector verbatim.
func TestHTTPProviderEmbedSuccess(t *testing.T) {
	srv, captured := fakeEmbedServer(t, EmbedDim, http.StatusOK)
	p := NewHTTPProvider(srv.URL, "nomic-embed-text")

	vec, err := p.Embed("hello world")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vec) != EmbedDim {
		t.Fatalf("vec len: got %d, want %d", len(vec), EmbedDim)
	}
	if captured.Model != "nomic-embed-text" {
		t.Errorf("request model: got %q, want %q", captured.Model, "nomic-embed-text")
	}
	if captured.Input != "hello world" {
		t.Errorf("request input: got %q, want %q", captured.Input, "hello world")
	}
}

// TestHTTPProviderHTTPError ensures non-200 responses surface as errors
// (rather than silently returning a zero vector that would corrupt the
// Redis index).
func TestHTTPProviderHTTPError(t *testing.T) {
	srv, _ := fakeEmbedServer(t, EmbedDim, http.StatusInternalServerError)
	p := NewHTTPProvider(srv.URL, "nomic-embed-text")

	_, err := p.Embed("anything")
	if err == nil {
		t.Fatalf("expected error on 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status code, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error should mention URL for diagnosability, got %q", err.Error())
	}
}

// TestHTTPProviderEmptyEmbeddings ensures we reject {"embeddings": []}
// rather than handing back a length-0 vector.
func TestHTTPProviderEmptyEmbeddings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: [][]float32{}})
	}))
	t.Cleanup(srv.Close)

	p := NewHTTPProvider(srv.URL, "some-model")
	_, err := p.Embed("x")
	if err == nil {
		t.Fatalf("expected error on empty embeddings, got nil")
	}
	if !strings.Contains(err.Error(), "no vectors") {
		t.Errorf("error should explain missing vectors, got %q", err.Error())
	}
}

// withProvider swaps the global default provider for the duration of a
// test and restores the previous value via t.Cleanup. Without this, a
// later test calling Embed() could hit a torn-down httptest server or
// (worse) a nil provider that panics on the next memory write.
//
// Forces the lazy-init Once first so prev is always non-nil — otherwise
// cleanup would restore nil and break unrelated tests in the package
// (e.g. supersession_test.go) that drive Embed() through Store writes.
func withProvider(t *testing.T, p Provider) {
	t.Helper()
	defaultProviderOnce.Do(initDefaultProvider)
	defaultProviderMu.RLock()
	prev := defaultProvider
	defaultProviderMu.RUnlock()
	SetProvider(p)
	t.Cleanup(func() {
		defaultProviderMu.Lock()
		defaultProvider = prev
		defaultProviderMu.Unlock()
	})
}

// TestEmbedWrapperDimCheck is the contract test for the public Embed() —
// even if a provider returns the wrong dim, the wrapper must reject it
// with an actionable error so a misconfigured model can't slip into the
// Redis index.
func TestEmbedWrapperDimCheck(t *testing.T) {
	wrong := EmbedDim + 16
	srv, _ := fakeEmbedServer(t, wrong, http.StatusOK)
	withProvider(t, NewHTTPProvider(srv.URL, "wrong-dim-model"))

	_, err := Embed("x")
	if err == nil {
		t.Fatalf("expected dim-check error, got nil")
	}
	for _, want := range []string{"wrong-dim-model", "768", "Redis index"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("dim error should contain %q, got %q", want, err.Error())
		}
	}
}

// TestSetProviderRoundTrip verifies that SetProvider wiring works and
// that subsequent Embed() calls hit the new provider, not the lazy-init
// default. It uses a stub provider rather than HTTP to keep the test
// focused on the wiring.
func TestSetProviderRoundTrip(t *testing.T) {
	stub := &stubProvider{vec: makeFloat32Vec(EmbedDim)}
	withProvider(t, stub)

	got, err := Embed("anything")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != EmbedDim {
		t.Fatalf("vec len: got %d, want %d", len(got), EmbedDim)
	}
	if stub.calls != 1 {
		t.Errorf("stub provider call count: got %d, want 1", stub.calls)
	}
}

// stubProvider is a deterministic in-memory Provider for tests.
type stubProvider struct {
	vec   []float32
	calls int
	err   error
}

func (s *stubProvider) Embed(_ string) ([]float32, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.vec, nil
}

func (s *stubProvider) Name() string { return "stub" }
