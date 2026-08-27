/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apiserver

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// externalReg is a dedicated registry for the external API servers' metrics so
// they don't collide with the controller-runtime manager's metrics (:8443).
var externalReg = prometheus.NewRegistry()

var (
	externalRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentorca_external_requests_total",
		Help: "HTTP requests handled by the external API servers, by server/method/path/status.",
	}, []string{"server", "method", "path", "status"})

	externalRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "agentorca_external_request_duration_seconds",
		Help: "Latency of HTTP requests handled by the external API servers.",
	}, []string{"server", "path"})

	externalAuthFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentorca_external_auth_failures_total",
		Help: "Authentication/authorization rejections on the external API servers.",
	}, []string{"server"})
)

func init() {
	externalReg.MustRegister(externalRequests, externalRequestDuration, externalAuthFailures)
}

// recordingResponseWriter captures the HTTP status code so the instrumenting
// middleware can record it (the status is otherwise only known by the handler).
type recordingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *recordingResponseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *recordingResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush propagates to the underlying ResponseWriter when it implements
// http.Flusher. This is REQUIRED for SSE streaming (the run stream and the
// ACP event stream both assert w.(http.Flusher) and call Flush after each
// chunk). Without this method, wrapping a Flusher-capable writer in
// recordingResponseWriter makes the type assertion fail, causing the stream
// handler to reject the connection as "streaming unsupported" and the UI to see
// "stream connection lost".
func (w *recordingResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// probePaths are endpoints not worth writing to the audit log on every hit.
// The status page polls /api/system/status and the history tab polls
// /api/runs/history every 5s; with the UI server now instrumented, counting
// those is desirable but auditing each poll would be noisy.
var probePaths = map[string]bool{
	"/healthz":           true,
	"/readyz":            true,
	"/metrics":           true,
	"/openapi.json":      true,
	"/api/system/status": true,
	"/api/runs/history":  true,
}

// isStreamingPath reports whether a request path is a long-lived SSE stream whose
// open duration must not be counted as request latency (see instrument).
func isStreamingPath(path string) bool {
	return strings.HasSuffix(path, "/stream")
}

// instrument wraps next with Prometheus request counters/histograms, an
// audit log line, and auth-failure accounting. It is placed INSIDE the auth
// middleware for the external servers so the tenant identity (if authenticated)
// is available via TenantFromContext for the audit log.
func instrument(server string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &recordingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rw, r)

		status := strconv.Itoa(rw.status)
		path := r.URL.Path
		labels := prometheus.Labels{
			"server": server,
			"method": r.Method,
			"path":   path,
			"status": status,
		}
		externalRequests.With(labels).Inc()
		// SSE streams (run/deployment chat streams) are long-lived by design;
		// observing their full open duration would skew the p95/p99 latency
		// heatmap toward 30s+ and make healthy streaming look "slow". Count them
		// as requests but exclude them from the latency histogram.
		if !isStreamingPath(path) {
			externalRequestDuration.With(prometheus.Labels{"server": server, "path": path}).
				Observe(time.Since(start).Seconds())
		}
		if rw.status == http.StatusUnauthorized || rw.status == http.StatusForbidden {
			externalAuthFailures.With(prometheus.Labels{"server": server}).Inc()
		}

		if !probePaths[path] {
			tenant, ok := TenantFromContext(r.Context())
			tname := "unknown"
			if ok && tenant != nil {
				tname = tenant.TenantName
			}
			slog.Info("external_api_request",
				"server", server,
				"tenant", tname,
				"method", r.Method,
				"path", path,
				"status", rw.status,
				"latency_ms", time.Since(start).Milliseconds(),
			)
		}
	})
}

// metricsHandler serves the external servers' Prometheus registry.
func metricsHandler() http.Handler {
	return promhttp.HandlerFor(externalReg, promhttp.HandlerOpts{
		Registry: externalReg,
	})
}
