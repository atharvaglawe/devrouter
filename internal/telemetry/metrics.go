package telemetry

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// registryOnce guards the single process-wide registry. The dashboard's
// HTTP handler reads it; tests can replace it via ResetForTest. We do
// not use the default prometheus.DefaultRegisterer so a misbehaving
// dependency that touches the global registry can't bleed into
// devrouter's /metrics output (and vice versa).
var (
	registryOnce sync.Once
	registry     *prometheus.Registry

	// Collectors are declared up-front so call sites can reference
	// them as ordinary package-level vars. They are wired into the
	// registry on first access via ensureRegistry(). This keeps the
	// usage pattern simple (telemetry.MCPRequests.WithLabelValues(...).Inc())
	// while letting tests reset everything between cases.
	MCPRequests         *prometheus.CounterVec
	MCPRequestDuration  *prometheus.HistogramVec
	MCPSessionsTotal    prometheus.Counter
	MCPSessionDuration  prometheus.Histogram
	MCPSessionsActive   prometheus.Gauge

	QueryTotal          *prometheus.CounterVec
	QueryDuration       *prometheus.HistogramVec
	StageDuration       *prometheus.HistogramVec
	PromptTokens        *prometheus.HistogramVec
	BudgetUsedFraction  *prometheus.HistogramVec
	FilesReturned       *prometheus.HistogramVec
	TrimmedFiles        *prometheus.HistogramVec
	RelevanceGateDrops  *prometheus.CounterVec
	AutoAnchorTotal     *prometheus.CounterVec
	RepeatQueryTotal    *prometheus.CounterVec
	FallbackTotal       *prometheus.CounterVec
	SanitizePlanDrops   *prometheus.CounterVec

	FeedbackTotal           *prometheus.CounterVec
	FeedbackRawReward       *prometheus.HistogramVec
	FeedbackAdjustedReward  *prometheus.HistogramVec
	FeedbackAdditionalFiles *prometheus.HistogramVec
	FeedbackFPRecorded      *prometheus.CounterVec

	CodegraphRequests        *prometheus.CounterVec
	CodegraphRequestDuration *prometheus.HistogramVec
	CodegraphInflight        *prometheus.GaugeVec

	EmbedderRequests        *prometheus.CounterVec
	EmbedderRequestDuration prometheus.Histogram
	EmbedderDimMismatch     prometheus.Counter

	RedisCommands        *prometheus.CounterVec
	RedisCommandDuration *prometheus.HistogramVec

	HeuristicsPromotions    *prometheus.CounterVec
	HeuristicsRollbacks     *prometheus.CounterVec
	HeuristicsDiscards      *prometheus.CounterVec
	HeuristicsRewardSamples *prometheus.CounterVec
	HeuristicsFrozen        prometheus.Gauge

	AnchorExplorations  prometheus.Counter
	AnchorObservations  *prometheus.CounterVec
	AnchorProbeFailures prometheus.Counter

	BuildInfo *prometheus.GaugeVec
)

const (
	namespace = "devrouter"

	// Histogram buckets tuned for the latency profiles in
	// docs/architecture.md: codegraph+Redis dominate end-to-end
	// latency (≈50-500ms), embedder is 30-150ms, individual stages
	// span microseconds (intent) to a few hundred ms (graph).
	// Stage / external buckets share the same scheme so the
	// dashboards can read them uniformly.
)

var (
	latencyBucketsSeconds = []float64{
		0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05,
		0.1, 0.25, 0.5, 1, 2.5, 5, 10,
	}

	// sessionBucketsSeconds covers an interactive Cursor MCP session:
	// most are seconds, some are hours. Logarithmic.
	sessionBucketsSeconds = prometheus.ExponentialBuckets(1, 4, 8) // 1s .. ~16k s (~4.5h)

	tokenBuckets        = []float64{500, 1000, 2000, 4000, 8000, 16000, 32000, 64000}
	fractionBuckets     = []float64{0.05, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0}
	smallCountBuckets   = []float64{0, 1, 2, 4, 8, 16, 32, 64, 128}
	rewardBuckets       = []float64{-2, -1, -0.5, -0.25, 0, 0.25, 0.5, 1, 2}
)

// init eagerly materialises the registry the moment any package
// imports telemetry. Without this, code paths that exercise package-
// level metric vars before the dashboard mounts /metrics (which is
// the only other site that calls ensureRegistry today) would
// dereference nil pointers — every other package treats Inc() /
// Observe() as zero-overhead. The cost is one small allocation per
// process and a few microseconds at startup.
func init() {
	ensureRegistry()
}

// ensureRegistry initialises the process-wide registry and all metric
// collectors exactly once. Safe to call from any goroutine; subsequent
// calls are no-ops.
func ensureRegistry() *prometheus.Registry {
	registryOnce.Do(buildRegistry)
	return registry
}

func buildRegistry() {
	registry = prometheus.NewRegistry()

	// Go runtime + process metrics. These are the standard scrapes
	// every Prometheus dashboard expects (goroutines, GC, fds, RSS,
	// CPU). Cheap and high signal.
	registry.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{
			Namespace: namespace,
		}),
	)

	// ---- MCP server -------------------------------------------------

	MCPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "mcp",
		Name: "requests_total",
		Help: "JSON-RPC requests handled, labeled by JSON-RPC method, MCP tool (when applicable), and outcome.",
	}, []string{"method", "tool", "status"})

	MCPRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "mcp",
		Name:    "request_duration_seconds",
		Help:    "Wall-clock latency of a JSON-RPC dispatch.",
		Buckets: latencyBucketsSeconds,
	}, []string{"method", "tool"})

	MCPSessionsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "mcp",
		Name: "sessions_total",
		Help: "MCP sessions started (each devrouter stdio subprocess increments once at Run()).",
	})

	MCPSessionDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "mcp",
		Name:    "session_duration_seconds",
		Help:    "MCP session wall-clock duration from Run() start to EOF.",
		Buckets: sessionBucketsSeconds,
	})

	MCPSessionsActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "mcp",
		Name: "sessions_active",
		Help: "Number of MCP sessions currently being served (1 per devrouter process in practice).",
	})

	// ---- Router / retrieval pipeline --------------------------------

	QueryTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "query",
		Name: "total",
		Help: "dev_context calls handled, labeled by classified intent and plan source.",
	}, []string{"intent", "plan_source"})

	QueryDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "query",
		Name:    "duration_seconds",
		Help:    "End-to-end retrieval latency (matches RetrievalTrace.TotalLatencyMs).",
		Buckets: latencyBucketsSeconds,
	}, []string{"intent"})

	StageDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "query",
		Name:    "stage_duration_seconds",
		Help:    "Per-stage retrieval latency (planner | search | graph | rerank | packing).",
		Buckets: latencyBucketsSeconds,
	}, []string{"stage", "intent"})

	PromptTokens = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "query",
		Name:    "prompt_tokens",
		Help:    "Estimated token count of the DevPrompt returned to the agent.",
		Buckets: tokenBuckets,
	}, []string{"intent"})

	BudgetUsedFraction = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "query",
		Name:    "budget_used_fraction",
		Help:    "Fraction of the per-intent context budget actually consumed by the returned DevPrompt.",
		Buckets: fractionBuckets,
	}, []string{"intent"})

	FilesReturned = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "query",
		Name:    "files_returned",
		Help:    "Distinct files contributing to a returned DevPrompt.",
		Buckets: smallCountBuckets,
	}, []string{"intent"})

	TrimmedFiles = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "query",
		Name:    "trimmed_files",
		Help:    "Files dropped by the per-intent trim cap before assembly.",
		Buckets: smallCountBuckets,
	}, []string{"intent"})

	RelevanceGateDrops = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "query",
		Name: "relevance_gate_drops_total",
		Help: "Memory hits dropped by the 4-stage relevance gate, labeled by which stage filtered them.",
	}, []string{"stage"}) // floor | must | fp | rerank

	AutoAnchorTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "query",
		Name: "auto_anchor_total",
		Help: "Calls that fell through to the rarest-token auto-anchor because the agent supplied no must_terms.",
	}, []string{"result"}) // anchored | none

	RepeatQueryTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "query",
		Name: "repeat_total",
		Help: "Queries detected as semantically-similar repeats of a recent query.",
	}, []string{"intent"})

	FallbackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "query",
		Name: "fallback_total",
		Help: "Pipeline fallbacks taken when an external dependency degraded, labeled by reason.",
	}, []string{"reason"}) // codegraph | redis | embedder | memory_floor | plan_filter

	SanitizePlanDrops = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "query",
		Name: "sanitize_plan_drops_total",
		Help: "Plan entries dropped by SanitizePlan because the caller exceeded per-field caps or sent malformed input.",
	}, []string{"field"})

	// ---- Feedback ---------------------------------------------------

	FeedbackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "feedback",
		Name: "total",
		Help: "dev_feedback calls processed, labeled by join mode and intent.",
	}, []string{"intent", "joined_via"}) // explicit | lru_fallback | dropped

	FeedbackRawReward = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "feedback",
		Name:    "raw_reward",
		Help:    "Raw reward computed for an explicit dev_feedback event.",
		Buckets: rewardBuckets,
	}, []string{"intent"})

	FeedbackAdjustedReward = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "feedback",
		Name:    "adjusted_reward",
		Help:    "Variance-adjusted reward fed to the bandit.",
		Buckets: rewardBuckets,
	}, []string{"intent"})

	FeedbackAdditionalFiles = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "feedback",
		Name:    "additional_files",
		Help:    "Extra files the agent had to read beyond what dev_context returned.",
		Buckets: smallCountBuckets,
	}, []string{"intent"})

	FeedbackFPRecorded = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "feedback",
		Name: "fp_attributions_total",
		Help: "False-positive attributions persisted against returned memories.",
	}, []string{"intent"})

	// ---- Codegraph client ------------------------------------------

	CodegraphRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "codegraph",
		Name: "requests_total",
		Help: "Outbound codegraph HTTP requests, labeled by endpoint and HTTP outcome.",
	}, []string{"endpoint", "status"}) // status: 2xx | 4xx | 5xx | error

	CodegraphRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "codegraph",
		Name:    "request_duration_seconds",
		Help:    "Wall-clock latency of codegraph HTTP requests.",
		Buckets: latencyBucketsSeconds,
	}, []string{"endpoint"})

	CodegraphInflight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "codegraph",
		Name: "requests_inflight",
		Help: "Codegraph HTTP requests currently in flight.",
	}, []string{"endpoint"})

	// ---- Embedder --------------------------------------------------

	EmbedderRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "embedder",
		Name: "requests_total",
		Help: "Outbound /api/embed requests, labeled by outcome.",
	}, []string{"status"}) // ok | http_error | network | bad_response | dim_mismatch

	EmbedderRequestDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "embedder",
		Name:    "request_duration_seconds",
		Help:    "Wall-clock latency of an /api/embed call.",
		Buckets: latencyBucketsSeconds,
	})

	EmbedderDimMismatch = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "embedder",
		Name: "dim_mismatch_total",
		Help: "Embed responses whose vector length differed from memory.EmbedDim.",
	})

	// ---- Redis (selectively wrapped) -------------------------------

	RedisCommands = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "redis",
		Name: "commands_total",
		Help: "Redis commands devrouter issues on the hot paths (memory store, heuristics, FT.SEARCH).",
	}, []string{"op", "status"}) // ok | error

	RedisCommandDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "redis",
		Name:    "command_duration_seconds",
		Help:    "Wall-clock latency of wrapped Redis calls.",
		Buckets: latencyBucketsSeconds,
	}, []string{"op"})

	// ---- Heuristics ------------------------------------------------

	HeuristicsPromotions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "heuristics",
		Name: "promotions_total",
		Help: "Bandit candidate-profile promotions, labeled by intent.",
	}, []string{"intent"})

	HeuristicsRollbacks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "heuristics",
		Name: "rollbacks_total",
		Help: "Bandit rollbacks to the frozen default profile.",
	}, []string{"intent"})

	HeuristicsDiscards = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "heuristics",
		Name: "discards_total",
		Help: "Bandit candidates discarded because they showed no lift.",
	}, []string{"intent"})

	HeuristicsRewardSamples = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "heuristics",
		Name: "reward_samples_total",
		Help: "Reward rows appended by the feedback loop, labeled by intent and source.",
	}, []string{"intent", "source"}) // explicit | implicit_repeat

	HeuristicsFrozen = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "heuristics",
		Name: "frozen",
		Help: "1 when the bandit is in frozen mode (no updates), 0 when learning.",
	})

	// ---- Anchor learner -------------------------------------------

	AnchorExplorations = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "anchor",
		Name: "explorations_total",
		Help: "Times the anchor learner took an ε-greedy exploration step.",
	})

	AnchorObservations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "anchor",
		Name: "observations_total",
		Help: "Anchor-learner reward observations, labeled by outcome.",
	}, []string{"outcome"}) // rewarded | penalized | discovered

	AnchorProbeFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "anchor",
		Name: "probe_failures_total",
		Help: "Codegraph file-probe failures during anchor injection.",
	})

	// ---- Build info -----------------------------------------------

	BuildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "build_info",
		Help:      "Static info about the running devrouter binary; always 1.",
	}, []string{"version", "codegraph_url", "redis_addr"})

	registry.MustRegister(
		MCPRequests, MCPRequestDuration, MCPSessionsTotal, MCPSessionDuration, MCPSessionsActive,
		QueryTotal, QueryDuration, StageDuration, PromptTokens, BudgetUsedFraction,
		FilesReturned, TrimmedFiles, RelevanceGateDrops, AutoAnchorTotal, RepeatQueryTotal,
		FallbackTotal, SanitizePlanDrops,
		FeedbackTotal, FeedbackRawReward, FeedbackAdjustedReward, FeedbackAdditionalFiles, FeedbackFPRecorded,
		CodegraphRequests, CodegraphRequestDuration, CodegraphInflight,
		EmbedderRequests, EmbedderRequestDuration, EmbedderDimMismatch,
		RedisCommands, RedisCommandDuration,
		HeuristicsPromotions, HeuristicsRollbacks, HeuristicsDiscards, HeuristicsRewardSamples, HeuristicsFrozen,
		AnchorExplorations, AnchorObservations, AnchorProbeFailures,
		BuildInfo,
	)
}

// Registry returns the package-private Prometheus registry, lazily
// constructing it on first use. Exposed so the dashboard mux (and
// tests) can mount a promhttp handler against it.
func Registry() *prometheus.Registry {
	return ensureRegistry()
}

// ResetForTest tears down the singleton registry and rebuilds it. Tests
// that assert exact metric values use this between cases to avoid
// cross-test pollution. Not safe to call concurrently with serving
// traffic; production code must never invoke it.
func ResetForTest() {
	registryOnce = sync.Once{}
	registry = nil
	ensureRegistry()
}
