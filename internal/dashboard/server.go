package dashboard

import (
	"context"
	_ "embed"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/atharva-ag/devrouter/internal/heuristics"
	"github.com/atharva-ag/devrouter/internal/memory"
)

//go:embed index.html
var indexHTML []byte

// serverStartTime is captured at package init so /api/version can
// return a stable "this build is running since X" value. The
// dashboard JS polls this and reloads the window when the value
// changes — saves users from having to hard-refresh after every
// rebuild during active development.
var serverStartTime = time.Now()

// Config bundles everything the dashboard needs from the host process.
// Memory and Heuristics may be nil — the corresponding tabs degrade to
// "not configured" rather than blocking server startup.
type Config struct {
	Addr       string
	Memory     *memory.Store
	Heuristics *heuristics.Picker
}

// Start binds the dashboard HTTP server. Returns immediately; the
// server runs in its own goroutine. Bind failures (port already in
// use is the common case — Cursor spawns multiple devrouter instances
// per project) are logged and swallowed so the MCP server keeps
// running. The first devrouter process to win the port serves the UI;
// the rest no-op.
func Start(cfg Config) {
	if cfg.Addr == "" {
		return
	}
	if cfg.Memory == nil || cfg.Heuristics == nil {
		log.Printf("[dashboard] skipped: memory/heuristics not initialised")
		return
	}
	// Bind synchronously so we can report the exact failure mode
	// (port in use vs permission denied vs bad address) before
	// returning. Anything past Listen is async.
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		log.Printf("[dashboard] disabled: bind %s failed: %v "+
			"(set DEVROUTER_DASHBOARD_ADDR=off to silence, or pick another port)",
			cfg.Addr, err)
		return
	}
	mux := newMux(cfg)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("[dashboard] listening on http://%s", ln.Addr())
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[dashboard] stopped: %v", err)
		}
	}()
}

func newMux(cfg Config) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	})

	mux.HandleFunc("/api/summary", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, LoadSummary(r.Context(), cfg.Heuristics.Store, cfg.Memory.RDB()))
	})

	mux.HandleFunc("/api/queries", func(w http.ResponseWriter, r *http.Request) {
		limit := queryInt(r, "limit", 50, 1, heuristics.TraceIndexCap)
		writeJSON(w, LoadRecentQueries(r.Context(), cfg.Heuristics.Store, cfg.Memory.RDB(), limit))
	})

	mux.HandleFunc("/api/heuristics", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, LoadHeuristics(r.Context(), cfg.Heuristics))
	})

	mux.HandleFunc("/api/decisions", func(w http.ResponseWriter, r *http.Request) {
		repo := r.URL.Query().Get("repo")
		writeJSON(w, LoadDecisions(r.Context(), cfg.Memory, repo))
	})

	mux.HandleFunc("/api/flows", func(w http.ResponseWriter, r *http.Request) {
		repo := r.URL.Query().Get("repo")
		writeJSON(w, LoadFlows(r.Context(), cfg.Memory.RDB(), cfg.Memory, repo))
	})

	// /api/flow_signal is the cross-flow aggregate the Heuristics tab
	// renders as the "Flow signal" panel. Walks every flow:overlay:*
	// hash on each request (cheap — bounded by total flows).
	mux.HandleFunc("/api/flow_signal", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, LoadFlowSignal(r.Context(), cfg.Memory))
	})

	mux.HandleFunc("/api/repos", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, LoadRepos(r.Context(), cfg.Memory.RDB()))
	})

	mux.HandleFunc("/api/topics", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, LoadTopics(r.Context(), cfg.Heuristics))
	})

	// /api/version lets the dashboard JS detect a restarted server
	// (start_ms changes) and auto-reload the window — otherwise a
	// long-lived tab keeps running the old JS even though /api/*
	// JSON has happily been updating, which is confusing during
	// development. Cheap, no Redis touch.
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"start_ms": serverStartTime.UnixMilli(),
		})
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := cfg.Memory.RDB().Ping(ctx).Err(); err != nil {
			http.Error(w, "redis unreachable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})

	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Pretty-print so the API is also human-friendly for ad-hoc curl
	// debugging — the gzip layer absorbs the extra bytes anyway.
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		log.Printf("[dashboard] write: %v", err)
	}
}

func queryInt(r *http.Request, name string, def, lo, hi int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}
