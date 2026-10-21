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
	"log/slog"
	"net/http"
	"time"
)

// The http.Server WriteTimeout (120s, see newHTTPServer in cmd/main.go) bounds
// the WHOLE response, so without intervention every SSE stream is torn down two
// minutes after it opens. The helpers below move the connection's write
// deadline via http.ResponseController (which reaches the net.Conn through
// recordingResponseWriter.Unwrap).

// sseWriteIdleTimeout bounds each write on open-ended SSE streams (UI run
// stream, external task stream) measured from the previous flush. It replaces
// the whole-response WriteTimeout with a per-event bound: a stalled peer still
// fails, but a healthy long-running stream does not. Generous because these
// streams send no heartbeats and a model call can be silent for minutes.
const sseWriteIdleTimeout = 10 * time.Minute

// extendSSEWriteDeadline moves the connection's write deadline to the
// request context's deadline (or acpStreamWindow from now when it has none).
// Used by bounded streams such as the ACP run stream, whose window is set by
// acpRequestTimeoutMiddleware. Failure is logged, not fatal: the stream still
// works up to WriteTimeout.
func extendSSEWriteDeadline(w http.ResponseWriter, r *http.Request) {
	deadline, ok := r.Context().Deadline()
	if !ok {
		deadline = time.Now().Add(acpStreamWindow)
	}
	if err := http.NewResponseController(w).SetWriteDeadline(deadline); err != nil {
		slog.Warn("failed to extend SSE write deadline", "path", r.URL.Path, "err", err)
	}
}

// rollingDeadlineFlusher wraps an SSE stream's Flusher so every flush pushes
// the write deadline idle into the future.
type rollingDeadlineFlusher struct {
	http.Flusher
	rc     *http.ResponseController
	idle   time.Duration
	path   string
	failed bool
}

// newRollingDeadlineFlusher arms the first deadline immediately and returns a
// Flusher that renews it after every flush. Drop-in for the handler's
// w.(http.Flusher) value.
func newRollingDeadlineFlusher(w http.ResponseWriter, r *http.Request, f http.Flusher, idle time.Duration) http.Flusher {
	d := &rollingDeadlineFlusher{Flusher: f, rc: http.NewResponseController(w), idle: idle, path: r.URL.Path}
	d.extend()
	return d
}

func (d *rollingDeadlineFlusher) Flush() {
	d.Flusher.Flush()
	d.extend()
}

func (d *rollingDeadlineFlusher) extend() {
	if d.failed {
		return
	}
	if err := d.rc.SetWriteDeadline(time.Now().Add(d.idle)); err != nil {
		// Unsupported writer: warn once and fall back to the server timeout.
		d.failed = true
		slog.Warn("failed to extend SSE write deadline", "path", d.path, "err", err)
	}
}
