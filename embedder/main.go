package main

// Process entrypoint for devrouter-embedder.
//
// Lifecycle:
//   1. Load config from env.
//   2. SetSharedLibraryPath + InitializeEnvironment for ONNX Runtime.
//   3. Construct the Embedder (loads tokenizer + model, warms up).
//   4. Bring up the HTTP server.
//   5. On SIGINT/SIGTERM: graceful shutdown with a 5s drain, then
//      destroy the embedder (releases native handles), then teardown
//      the ORT environment.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	ort "github.com/yalue/onnxruntime_go"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := FromEnv()
	if err != nil {
		return err
	}
	logger.Info("starting devrouter-embedder",
		"model_dir", cfg.ModelDir,
		"onnx_filename", cfg.ONNXFilename,
		"max_length", cfg.MaxLength,
		"intra_op_threads", cfg.IntraOpThreads,
		"ort_lib", cfg.ORTLibraryPath,
		"listen", cfg.ListenAddr,
	)

	// SetSharedLibraryPath must come before InitializeEnvironment.
	// onnxruntime_go does NOT bundle the C library — it dlopen()s the
	// path here at init. If the path is wrong we want the failure to
	// come from this call (clear "no such file") rather than a cryptic
	// segfault later inside ORT.
	ort.SetSharedLibraryPath(cfg.ORTLibraryPath)
	if err := ort.InitializeEnvironment(); err != nil {
		return err
	}
	defer ort.DestroyEnvironment()

	t0 := time.Now()
	embedder, err := NewEmbedder(cfg)
	if err != nil {
		return err
	}
	defer embedder.Close()
	logger.Info("embedder ready", "dim", embedder.Dim(), "init_ms", time.Since(t0).Milliseconds())

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: NewServer(embedder, logger).Routes(),
		// Header timeout only — embeds themselves can take >1s on long
		// inputs and we don't want to time those out. Body read is
		// gated by the embedder's mutex anyway.
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Signal handling: graceful shutdown on SIGINT/SIGTERM. Inflight
	// requests get up to 5s to drain — long enough for a single in-
	// flight embed, short enough that docker stop doesn't escalate to
	// SIGKILL on us at the 10s mark.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
