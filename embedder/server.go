package main

// HTTP wrapper around Embedder. Wire shape mirrors Ollama's /api/embed
// exactly so DEVROUTER_EMBEDDING_URL can swap between this service and
// any other compatible endpoint without code changes elsewhere.
//
// Request shape:
//   POST /api/embed   {"model": "<ignored>", "input": "<text>"|[...]}
// Response:
//   200 {"embeddings": [[...float32...]], "model": "<id>"}
//   400 on empty input (matches Ollama)
//   422 on JSON validation failure
//   500 with structured error on inference failure
//
// Why stdlib net/http rather than chi/echo/gin: zero dependencies on
// top of the CGo deps we already pull in, and three endpoints isn't
// enough surface to justify a router. Adds ~150 LOC vs ~800 LOC of
// framework imports, all of which is straightforward to read.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

type Server struct {
	embedder *Embedder
	logger   *slog.Logger
}

func NewServer(e *Embedder, logger *slog.Logger) *Server {
	return &Server{embedder: e, logger: logger}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/embed", s.handleEmbed)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/version", s.handleVersion)
	return mux
}

// embedRequest accepts both forms Ollama does:
//
//   {"model": "...", "input": "single string"}
//   {"model": "...", "input": ["batch", "of", "strings"]}
//
// We use a custom UnmarshalJSON on the input field to handle both
// without forcing the caller to know which one to send. The `model`
// field is parsed but ignored — each instance serves exactly one
// model. If you need to route between models, run multiple containers.
type embedRequest struct {
	Model string      `json:"model"`
	Input embedInputs `json:"input"`
}

type embedInputs struct {
	Texts []string
}

func (i *embedInputs) UnmarshalJSON(data []byte) error {
	// Try a single string first since that's by far the most common
	// shape for our workload (one dev_context call = one embed).
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		i.Texts = []string{s}
		return nil
	}
	var arr []string
	if err := json.Unmarshal(data, &arr); err != nil {
		return errors.New(`"input" must be a string or array of strings`)
	}
	i.Texts = arr
	return nil
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Model      string      `json:"model"`
}

type healthResponse struct {
	Status string `json:"status"`
	Model  string `json:"model"`
	Dim    int    `json:"dim"`
}

type versionResponse struct {
	Model        string `json:"model"`
	Dim          int    `json:"dim"`
	MaxLength    int    `json:"max_length"`
	ONNXFilename string `json:"onnx_filename"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (s *Server) handleEmbed(w http.ResponseWriter, r *http.Request) {
	var req embedRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("invalid request body: %w", err))
		return
	}
	if len(req.Input.Texts) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("`input` must be a non-empty string or list"))
		return
	}

	t0 := time.Now()
	vectors, err := s.embedder.EncodeBatch(req.Input.Texts)
	if err != nil {
		// Logged at error so operators can grep slow/failed embeds in
		// container logs without losing the input shape signal.
		s.logger.Error("embed failed",
			"n", len(req.Input.Texts),
			"first_len", len(req.Input.Texts[0]),
			"err", err,
		)
		writeError(w, http.StatusInternalServerError, fmt.Errorf("embed failed: %w", err))
		return
	}
	dt := time.Since(t0)

	s.logger.Info("embed",
		"n", len(req.Input.Texts),
		"dim", s.embedder.Dim(),
		"took_ms", dt.Milliseconds(),
	)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(embedResponse{
		Embeddings: vectors,
		Model:      s.embedder.ModelID(),
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(healthResponse{
		Status: "ok",
		Model:  s.embedder.ModelID(),
		Dim:    s.embedder.Dim(),
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(versionResponse{
		Model:        s.embedder.ModelID(),
		Dim:          s.embedder.Dim(),
		MaxLength:    s.embedder.cfg.MaxLength,
		ONNXFilename: s.embedder.cfg.ONNXFilename,
	})
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: err.Error()})
}
