package telemetry

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Handler returns an http.Handler that serves the Prometheus exposition
// format from devrouter's private registry. The dashboard mux mounts it
// at /metrics by default; integrators can also mount it on a separate
// bind address by reading Registry() directly.
func Handler() http.Handler {
	reg := ensureRegistry()
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		// Continue serving on individual collector errors instead of
		// failing the whole scrape; one broken collector should never
		// black out the rest of the metrics surface.
		ErrorHandling: promhttp.ContinueOnError,
	})
}
