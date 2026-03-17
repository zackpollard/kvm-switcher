package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kvm_http_requests_total",
		Help: "Total number of HTTP requests",
	}, []string{"method", "path", "status"})

	httpRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kvm_http_request_duration_seconds",
		Help:    "HTTP request duration in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})

	// SessionsActive is exported so handlers can update it.
	SessionsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "kvm_sessions_active",
		Help: "Number of currently active KVM sessions",
	})

	// BMCOnline is exported so the status poller can set it per server.
	BMCOnline = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kvm_bmc_online",
		Help: "Whether a BMC device is online (1) or offline (0)",
	}, []string{"server"})
)

// MetricsMiddleware instruments HTTP handlers with request count and duration metrics.
func MetricsMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Normalize path to avoid high cardinality
			path := normalizePath(r.URL.Path)

			wrapped := &statusRecorderMetrics{ResponseWriter: w, statusCode: http.StatusOK}
			next.ServeHTTP(wrapped, r)

			duration := time.Since(start).Seconds()
			status := strconv.Itoa(wrapped.statusCode)

			httpRequestsTotal.WithLabelValues(r.Method, path, status).Inc()
			httpRequestDuration.WithLabelValues(r.Method, path).Observe(duration)
		})
	}
}

type statusRecorderMetrics struct {
	http.ResponseWriter
	statusCode int
}

func (r *statusRecorderMetrics) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

// normalizePath reduces cardinality by replacing dynamic path segments.
func normalizePath(path string) string {
	switch {
	case len(path) > 15 && path[:14] == "/api/sessions/":
		return "/api/sessions/{id}"
	case len(path) > 19 && path[:19] == "/api/ipmi-session/":
		return "/api/ipmi-session/{name}"
	case len(path) > 8 && path[:8] == "/ws/kvm/":
		return "/ws/kvm/{id}"
	case len(path) > 7 && path[:7] == "/__bmc/":
		return "/__bmc/{name}/..."
	default:
		return path
	}
}
