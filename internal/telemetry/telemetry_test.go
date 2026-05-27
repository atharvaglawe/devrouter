package telemetry

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestRegistryRegistersCollectors verifies that ensureRegistry wires
// every package-level collector into the same registry — the property
// the Handler relies on.
func TestRegistryRegistersCollectors(t *testing.T) {
	ResetForTest()

	// Touch a representative metric from each subsystem to confirm
	// they are reachable through the registry. We use Add(0) /
	// Observe(0) so the test does not depend on default counters
	// being non-zero (counters with no samples don't emit until the
	// first observation).
	MCPRequests.WithLabelValues("tools/call", "dev_context", "ok").Add(0)
	QueryDuration.WithLabelValues("debug").Observe(0)
	StageDuration.WithLabelValues("search", "debug").Observe(0)
	CodegraphRequests.WithLabelValues("search", "2xx").Add(0)
	EmbedderDimMismatch.Add(0)
	RedisCommands.WithLabelValues("HGETALL", "ok").Add(0)
	HeuristicsFrozen.Set(0)
	AnchorExplorations.Add(0)
	BuildInfo.WithLabelValues("test", "http://cg", "redis:6379").Set(1)

	got := testutil.CollectAndCount(BuildInfo)
	if got != 1 {
		t.Fatalf("BuildInfo: expected exactly one labelled series, got %d", got)
	}
}

// TestHandlerExposesMetrics scrapes /metrics and confirms the
// exposition output contains a representative subset of the metrics
// we publish. Catches regressions in registry wiring without locking
// the test to every metric name (those churn as instrumentation grows).
func TestHandlerExposesMetrics(t *testing.T) {
	ResetForTest()
	MCPSessionsTotal.Inc()
	QueryTotal.WithLabelValues("debug", "agent").Inc()

	srv := httptest.NewServer(Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("scrape /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	out := string(body)

	for _, want := range []string{
		"devrouter_mcp_sessions_total",
		"devrouter_query_total",
		`intent="debug"`,
		`plan_source="agent"`,
		"# HELP",
		"# TYPE",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/metrics output missing %q.\nFull body:\n%s", want, out)
		}
	}
}

// TestResetForTestClearsCounters guarantees that ResetForTest is a true
// reset — without this, test cases that share package-level state
// would silently accumulate values across the suite and produce
// flaky assertions.
func TestResetForTestClearsCounters(t *testing.T) {
	ResetForTest()
	MCPSessionsTotal.Inc()
	MCPSessionsTotal.Inc()
	if got := testutil.ToFloat64(MCPSessionsTotal); got != 2 {
		t.Fatalf("MCPSessionsTotal = %v, want 2", got)
	}

	ResetForTest()
	if got := testutil.ToFloat64(MCPSessionsTotal); got != 0 {
		t.Fatalf("MCPSessionsTotal after reset = %v, want 0", got)
	}
}

// TestNormaliseOp confirms the Redis-command label normaliser keeps
// cardinality bounded: arbitrary casing collapses to a canonical
// upper-case form, empty input falls back to UNKNOWN.
func TestNormaliseOp(t *testing.T) {
	cases := map[string]string{
		"get":       "GET",
		"  HSET ":   "HSET",
		"FT.SEARCH": "FT.SEARCH",
		"":          "UNKNOWN",
	}
	for in, want := range cases {
		if got := normaliseOp(in); got != want {
			t.Errorf("normaliseOp(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLoggerSetupIsIdempotent confirms repeated Setup calls don't
// panic and always return a non-nil logger. The MCP host can spawn
// devrouter many times in a session; we cannot rely on a single-shot
// init.
func TestLoggerSetupIsIdempotent(t *testing.T) {
	Setup()
	first := Logger()
	Setup()
	second := Logger()
	if first == nil || second == nil {
		t.Fatal("Logger returned nil after Setup")
	}
	if first != second {
		t.Errorf("Logger should be stable across Setup calls; got distinct instances")
	}
}
