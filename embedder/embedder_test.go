package main

// Unit tests for the embedder. Tests are skipped if the model files
// aren't present locally — run `make embedder-deps` and
// `make embedder-fetch-model` first, or set
// DEVROUTER_EMBEDDER_TEST_MODEL_DIR to point at any directory with the
// same layout.

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	ort "github.com/yalue/onnxruntime_go"
)

// Shared embedder across tests: NewEmbedder calls ORT.InitializeEnvironment
// transitively, and that's a process-global resource. We pay the
// ~1s init cost once for the whole package, mirroring how a long-running
// HTTP service uses it. Each test runs against the same instance.
var (
	testE       *Embedder
	testEErr    error
	testEOnce   sync.Once
	testEnvInit sync.Once
)

func loadEmbedder(t *testing.T) *Embedder {
	t.Helper()
	testEOnce.Do(func() {
		modelDir := os.Getenv("DEVROUTER_EMBEDDER_TEST_MODEL_DIR")
		if modelDir == "" {
			modelDir = filepath.Join("testdata", "nomic-embed-text-v1.5")
		}
		if _, err := os.Stat(filepath.Join(modelDir, "model.onnx")); err != nil {
			testEErr = err
			return
		}

		ortPath := os.Getenv("ORT_LIBRARY_PATH")
		if ortPath == "" {
			ortPath = filepath.Join("lib", "libonnxruntime.dylib")
			if _, err := os.Stat(ortPath); err != nil {
				ortPath = filepath.Join("lib", "libonnxruntime.so")
			}
		}

		testEnvInit.Do(func() {
			ort.SetSharedLibraryPath(ortPath)
			testEErr = ort.InitializeEnvironment()
		})
		if testEErr != nil {
			return
		}

		cfg := Config{
			ModelDir:       modelDir,
			ONNXFilename:   "model.onnx",
			ORTLibraryPath: ortPath,
			MaxLength:      8192,
			ExpectedDim:    768,
		}
		testE, testEErr = NewEmbedder(cfg)
	})
	if testEErr != nil {
		t.Skipf("embedder unavailable: %v (run make embedder-deps && make embedder-fetch-model first)", testEErr)
	}
	return testE
}

func l2(v []float32) float64 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	return math.Sqrt(s)
}

func cosine(a, b []float32) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot / (l2(a) * l2(b))
}

// Each test below covers one invariant of the embedder pipeline that
// must hold regardless of model / batch size / input length. They run
// against a real ONNX session, not mocks — there's nothing meaningful
// to mock at this layer.

func TestDimMatchesExpected(t *testing.T) {
	e := loadEmbedder(t)
	v, err := e.EncodeOne("hello world")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 768 {
		t.Fatalf("got dim=%d, want 768", len(v))
	}
}

func TestL2Normalized(t *testing.T) {
	e := loadEmbedder(t)
	for _, text := range []string{"a", "hello world", strings.Repeat("foo bar ", 200)} {
		v, err := e.EncodeOne(text)
		if err != nil {
			t.Fatalf("encode %q: %v", text, err)
		}
		n := l2(v)
		if math.Abs(n-1.0) > 1e-5 {
			t.Errorf("text=%q: norm=%v, want 1.0±1e-5", text, n)
		}
	}
}

func TestReproducibleSameInput(t *testing.T) {
	e := loadEmbedder(t)
	v1, err := e.EncodeOne("same text twice")
	if err != nil {
		t.Fatal(err)
	}
	v2, err := e.EncodeOne("same text twice")
	if err != nil {
		t.Fatal(err)
	}
	// FP inference is deterministic for the CPU EP given identical
	// input — drift here would indicate non-deterministic kernel
	// selection or shared mutable state somewhere.
	if c := cosine(v1, v2); math.Abs(c-1.0) > 1e-6 {
		t.Errorf("same input produced different vectors: cosine=%v", c)
	}
}

func TestLongInputTruncatedNotErrored(t *testing.T) {
	e := loadEmbedder(t)
	// 100K chars: well past the 8192-token max. Must truncate and
	// return a valid vector, not error.
	v, err := e.EncodeOne(strings.Repeat("the quick brown fox jumped over the lazy dog ", 2500))
	if err != nil {
		t.Fatalf("long input errored: %v", err)
	}
	if len(v) != 768 {
		t.Fatalf("got dim=%d, want 768", len(v))
	}
	if n := l2(v); math.Abs(n-1.0) > 1e-5 {
		t.Errorf("long input: norm=%v, want 1.0±1e-5", n)
	}
}

func TestBatchMatchesSingleton(t *testing.T) {
	e := loadEmbedder(t)
	texts := []string{
		"cache provider configs",
		"intent classification keywords",
		"redis vector index",
	}
	batch, err := e.EncodeBatch(texts)
	if err != nil {
		t.Fatal(err)
	}
	for i, text := range texts {
		single, err := e.EncodeOne(text)
		if err != nil {
			t.Fatalf("encode %q: %v", text, err)
		}
		// Batched encoding pads inputs to the same length; padded
		// positions are masked out of the mean-pool, so the batched
		// and singleton vectors should agree up to FP-rounding noise.
		c := cosine(batch[i], single)
		if math.Abs(c-1.0) > 1e-5 {
			t.Errorf("text=%q: batch vs single cosine=%v, want 1.0±1e-5", text, c)
		}
	}
}

func TestSemanticallySimilarTextsAreClose(t *testing.T) {
	e := loadEmbedder(t)
	a, err := e.EncodeOne("redis vector index for memories")
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.EncodeOne("memory store using vector search in redis")
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.EncodeOne("recipe for chocolate chip cookies")
	if err != nil {
		t.Fatal(err)
	}
	simAB := cosine(a, b)
	simAC := cosine(a, c)
	// Paraphrases should beat unrelated topics by a clear margin. The
	// exact margin isn't worth pinning — what we want is a smoke
	// signal that semantic similarity is preserved, not regression
	// testing the model itself.
	if simAB <= simAC+0.05 {
		t.Errorf("paraphrase similarity (%.3f) should beat unrelated (%.3f) by >0.05", simAB, simAC)
	}
}

func TestEmptyBatchReturnsEmpty(t *testing.T) {
	e := loadEmbedder(t)
	out, err := e.EncodeBatch(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("got %d vectors for empty batch, want 0", len(out))
	}
}
