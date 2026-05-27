// Package telemetry owns devrouter's process-level observability: a
// Prometheus registry of RED + budget + bandit + external-call metrics,
// structured slog output, and the /metrics HTTP handler that the
// dashboard mux mounts.
//
// Per-query observability already lives in `feedback:trace:{query_id}`
// Redis hashes (see internal/prompt.RetrievalTrace) and is rendered by
// internal/dashboard. This package is the orthogonal aggregate view:
// what an SRE wants to scrape, alert on, and graph over time.
//
// # Wiring
//
// cmd/router/main.go calls telemetry.Setup() once at startup. After
// that, packages emit metrics by calling the exported vars (Counters /
// Histograms / Gauges) and log via telemetry.Logger() — both are safe
// to call from any goroutine and never panic on a nil registry.
//
// # Cardinality discipline
//
// Labels are deliberately constrained to bounded sets:
//
//   - intent:      debug | explore | trace | refactor | general
//   - plan_source: agent | auto
//   - tool:        the static MCP tool names (10 of them)
//   - stage:       planner | search | graph | rerank | packing
//   - endpoint:    the static codegraph route names
//   - op:          the small set of Redis ops we wrap explicitly
//
// query_id and heuristic_profile_id are HIGH cardinality — they are
// emitted as slog fields and stored on the per-query Redis trace, never
// as Prometheus labels. The dashboard joins them on demand.
//
// # Opt-out
//
//	DEVROUTER_METRICS_ADDR=off       — don't mount /metrics on the dashboard
//	DEVROUTER_LOG_FORMAT=json        — switch slog to JSON (default: text)
//	DEVROUTER_LOG_LEVEL=info|debug|… — slog level (default: info)
package telemetry
