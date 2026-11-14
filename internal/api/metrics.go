package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/processmetrics"
)

// Instrument records bounded route classes, not paths or identifiers.
func Instrument(next http.Handler, m *processmetrics.Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := "other"
		switch {
		case r.URL.Path == "/healthz" || r.URL.Path == "/readyz":
			route = "health"
		case strings.HasPrefix(r.URL.Path, "/v1/webhooks/"):
			route = "webhook"
		case strings.HasPrefix(r.URL.Path, "/v1/"):
			route = "api"
		case strings.HasPrefix(r.URL.Path, "/private/"):
			route = "private"
		}
		method := r.Method
		switch method {
		case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		default:
			method = "OTHER"
		}
		m.Inflight.WithLabelValues(route, method).Inc()
		start := time.Now()
		rec := &metricsResponse{ResponseWriter: w}
		completed := false
		defer func() {
			m.Inflight.WithLabelValues(route, method).Dec()
			status := rec.status
			if status == 0 {
				status = 200
				if !completed {
					status = 500
				}
			}
			label := strconv.Itoa(status)
			m.Requests.WithLabelValues(route, method, label).Inc()
			m.HTTPDuration.WithLabelValues(route, method, label).Observe(time.Since(start).Seconds())
		}()
		next.ServeHTTP(rec, r)
		completed = true
	})
}

type metricsResponse struct {
	http.ResponseWriter
	status int
}

func (w *metricsResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *metricsResponse) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *metricsResponse) WriteHeader(status int) {
	if status >= 200 && w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *metricsResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}
