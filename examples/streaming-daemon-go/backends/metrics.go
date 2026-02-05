// Metrics helpers for backends.
// These functions record Prometheus metrics for backend operations.

package backends

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Backend metrics - track API calls to upstream services.
// These are package-level variables initialized at startup, matching the
// pattern used in the main daemon file.
var (
	metricBackendRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "daemon_backend_requests_total",
		Help: "Total backend API requests made",
	}, []string{"backend"})

	metricBackendErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "daemon_backend_errors_total",
		Help: "Total backend API errors",
	}, []string{"backend"})

	metricBackendDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "daemon_backend_duration_seconds",
		Help:    "Backend request total duration",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 15), // 10ms to ~163s
	}, []string{"backend"})

	metricBackendTTFB = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "daemon_backend_ttfb_seconds",
		Help:    "Time to first byte from backend",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 15), // 1ms to ~16s
	}, []string{"backend"})

	metricChunksSent = promauto.NewCounter(prometheus.CounterOpts{
		Name: "daemon_chunks_sent_total",
		Help: "Total SSE chunks sent to clients",
	})
)

// benchmarkMode is set by the main daemon when running in benchmark mode.
// When true, Prometheus updates are skipped for better performance.
var benchmarkMode bool

// SetBenchmarkMode enables or disables benchmark mode for the backends package.
func SetBenchmarkMode(enabled bool) {
	benchmarkMode = enabled
}

// RecordBackendRequest records a successful backend API request.
func RecordBackendRequest(backendName string) {
	if benchmarkMode {
		return
	}
	metricBackendRequests.WithLabelValues(backendName).Inc()
}

// RecordBackendError records a backend API error.
func RecordBackendError(backendName string) {
	if benchmarkMode {
		return
	}
	metricBackendErrors.WithLabelValues(backendName).Inc()
}

// RecordBackendDuration records the total duration of a backend request.
func RecordBackendDuration(backendName string, seconds float64) {
	if benchmarkMode {
		return
	}
	metricBackendDuration.WithLabelValues(backendName).Observe(seconds)
}

// RecordBackendTTFB records the time to first byte from a backend.
func RecordBackendTTFB(backendName string, seconds float64) {
	if benchmarkMode {
		return
	}
	metricBackendTTFB.WithLabelValues(backendName).Observe(seconds)
}

// RecordChunkSent records an SSE chunk being sent.
func RecordChunkSent() {
	if benchmarkMode {
		return
	}
	metricChunksSent.Inc()
}
