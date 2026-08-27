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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/floppyfish14/agent-orca/internal/state"
)

// Version is the operator build version. Set at link time via:
//
//	go build -ldflags "-X github.com/floppyfish14/agent-orca/internal/apiserver.Version=<v>"
//
// or left as "dev" for local builds.
var Version = "dev"

// k8sReady returns a readiness probe func for the Kubernetes API. nil k8s means
// "no check" (returns nil). Otherwise it calls ServerVersion.
func k8sReady(k8s kubernetes.Interface) func() error {
	if k8s == nil {
		return nil
	}
	return func() error {
		_, err := k8s.Discovery().ServerVersion()
		return err
	}
}

// healthzHandler is a liveness probe: always 200 once the process is up.
func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// versionHandler reports the build version.
func versionHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"version": Version})
}

// readyzHandlerBuilder returns a readiness handler that checks the Kubernetes
// API (always) and the state store (when configured).
// readyzHandlerBuilder returns a readiness handler. k8sReady returns nil if the
// Kubernetes API is reachable (and an error otherwise); pass nil to skip that
// check. The state store is pinged only when hasStore is true.
func readyzHandlerBuilder(k8sReady func() error, hasStore bool, store state.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Kubernetes API reachability.
		if k8sReady != nil {
			if err := k8sReady(); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "unready",
					"reason": fmt.Sprintf("kubernetes API unreachable: %s", err),
				})
				return
			}
		}
		// Optional state store (Redis) reachability.
		if hasStore && store != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := store.Ping(ctx); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "unready",
					"reason": fmt.Sprintf("state store unreachable: %s", err),
				})
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}
