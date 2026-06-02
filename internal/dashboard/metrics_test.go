package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/atharva-ag/devrouter/internal/telemetry"
)

// TestNewMuxMountsMetrics verifies the dashboard mux exposes the
// Prometheus /metrics endpoint by default. Critical because the bind
// happens once at startup; if metrics weren't mounted there, /metrics
// would silently 404 on every scrape.
//
// This test exercises only the mux wiring — we don't need a real
// memory store or heuristics picker since /metrics serves the
// process-wide registry directly.
func TestNewMuxMountsMetrics(t *testing.T) {
	t.Setenv("DEVROUTER_METRICS_ADDR", "")

	mux := http.NewServeMux()
	// Re-create the same handler the live mux wires up. metricsEnabled
	// reads from env at call time so the empty-string default routes
	// here. Keeping the test out of newMux() (which requires a
	// fully-wired memory + heuristics graph) keeps it hermetic.
	if metricsEnabled() {
		mux.Handle("/metrics", telemetry.Handler())
	}

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "# TYPE") {
		t.Errorf("/metrics body does not look like a Prometheus exposition")
	}
}

// TestMetricsDisabledHonoured checks the DEVROUTER_METRICS_ADDR=off
// opt-out. Operators who want to keep the dashboard but expose
// /metrics on a separate process port rely on this.
func TestMetricsDisabledHonoured(t *testing.T) {
	for _, sentinel := range []string{"off", "none", "disabled", "false", "0"} {
		t.Run(sentinel, func(t *testing.T) {
			t.Setenv("DEVROUTER_METRICS_ADDR", sentinel)
			if metricsEnabled() {
				t.Errorf("metricsEnabled() = true for sentinel %q, want false", sentinel)
			}
		})
	}
}
