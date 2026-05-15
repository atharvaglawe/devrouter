package main

// Runtime configuration for the embedder service. All knobs are env
// vars so the same binary works for any deployment. Defaults match
// what the upstream HuggingFace reference implementation for
// nomic-embed-text-v1.5 uses (8192 max length, 768 dim, mean-pool +
// L2 normalize) — change them deliberately, not on whim, since
// downstream cosine thresholds in devrouter are calibrated against
// those values.

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	// Filesystem location holding model.onnx + tokenizer.json. Inside
	// the container this is a docker volume mount; on the host it's a
	// regular directory.
	ModelDir string

	// Filename of the ONNX graph inside ModelDir. Swap to a quantized
	// variant by setting ONNX_FILENAME=model_quantized.onnx.
	ONNXFilename string

	// Path to libonnxruntime.{so,dylib}. The Go bindings load this
	// dynamically at startup — different OS / arch combos need different
	// files, so it's an explicit env var rather than hardcoded.
	ORTLibraryPath string

	// Tokenizer truncation length. nomic-embed-text-v1.5 trains at 2048
	// and serves up to 8192 via rope-scaling; we use 8192 to match the
	// canonical HF reference. Inputs longer than this are truncated
	// silently, which matches what every other embedding endpoint does.
	MaxLength int

	// Sanity-check dim. Service refuses to start if the model produces
	// a different size — catches "you pointed at the wrong ONNX file"
	// before the first /api/embed call returns nonsense.
	ExpectedDim int

	// ONNX Runtime intra-op threads. 0 = let ORT pick (= physical cores).
	// Set to a small number when co-locating with other CPU-heavy work.
	IntraOpThreads int

	// HTTP listen address. Container binds 0.0.0.0:8080 and is published
	// on host port 11435 (devrouter's default DEVROUTER_EMBEDDING_URL).
	ListenAddr string
}

func FromEnv() (Config, error) {
	cfg := Config{
		ModelDir:       getenv("MODEL_DIR", "/models/nomic-embed-text-v1.5"),
		ONNXFilename:   getenv("ONNX_FILENAME", "model.onnx"),
		ORTLibraryPath: getenv("ORT_LIBRARY_PATH", "/usr/local/lib/libonnxruntime.so"),
		ListenAddr:     getenv("LISTEN_ADDR", "0.0.0.0:8080"),
	}

	var err error
	if cfg.MaxLength, err = getenvInt("MAX_LENGTH", 8192); err != nil {
		return cfg, err
	}
	if cfg.ExpectedDim, err = getenvInt("EXPECTED_DIM", 768); err != nil {
		return cfg, err
	}
	if cfg.IntraOpThreads, err = getenvInt("INTRA_OP_THREADS", 0); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("env %s=%q is not an integer: %w", key, v, err)
	}
	return n, nil
}
