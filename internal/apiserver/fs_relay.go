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
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/acpfs"
)

// These handlers implement the pod-facing half of the ACP filesystem relay
// (see internal/acpfs and docs/filesystem-access.md): the model-router
// sidecar turns the built-in fs_read_text_file / fs_write_text_file tool
// calls into requests here; the workstation-side ACP bridge answers them via
// the mirrored ACP API endpoints in acp_fs.go.
//
// Auth is TokenReview via handleAgentRun (pod SA token). The relay is
// capability-gated: it is only wired when the operator was started with a
// shared state store backing it; with no relay, fs requests are answered 501s.

// defaultFSPollInterval is the server-side re-check cadence while long-polling.
const defaultFSPollInterval = 250 * time.Millisecond

// maxFSLongPoll caps a single ?wait= long poll so one request stays well
// inside the internal API request timeout (60s). Callers loop around it.
const maxFSLongPoll = 25 * time.Second

// SetFilesystemRelay wires the fs/* relay. Call with nil to disable
// filesystem access (fs requests then fail fast with 501).
func (s *Server) SetFilesystemRelay(relay acpfs.Relay) {
	s.fsRelay = relay
}

// enqueueFSRequest handles POST /agentrun/{ns}/{run}/fs from the model-router.
func (s *Server) enqueueFSRequest(w http.ResponseWriter, r *http.Request, namespace, runName string) {
	if !s.fsRelayOK(w) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	// The run must exist in this namespace: it anchors the relay scope and
	// keeps unknown/forged run names out of the relay.
	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: runName, Namespace: namespace}, &run); err != nil {
		http.Error(w, fmt.Sprintf("getting AgentRun: %v", err), http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	var req acpfs.EnqueueRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	switch req.Method {
	case acpfs.MethodReadTextFile, acpfs.MethodWriteTextFile, acpfs.MethodListDir:
		// relayable
	default:
		http.Error(w, fmt.Sprintf("method %q is not a relayable fs method", req.Method), http.StatusBadRequest)
		return
	}

	created, err := s.fsRelay.Enqueue(r.Context(), acpfsScope(namespace, runName), req)
	if err != nil {
		if err == acpfs.ErrUnavailable {
			http.Error(w, err.Error(), http.StatusNotImplemented)
			return
		}
		slog.Error("enqueueing fs request", "run", runName, "ns", namespace, "err", err)
		http.Error(w, "enqueueing fs request", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(created)
}

// takeFSResponse handles GET /agentrun/{ns}/{run}/fs/{id} from the
// model-router. Returns 200 with the response once available, 204 while still
// pending (after an optional ?wait=<seconds> long poll), and 410 once the
// request has expired (or the run deleted) without an answer.
func (s *Server) takeFSResponse(w http.ResponseWriter, r *http.Request, namespace, runName, requestID string) {
	if !s.fsRelayOK(w) {
		return
	}
	wait := fsWait(0) // no long poll unless ?wait= is passed
	if v := r.URL.Query().Get("wait"); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil {
			wait = fsWait(secs)
		}
	}
	deadline := time.Now().Add(wait)
	for {
		resp, err := s.fsRelay.Take(r.Context(), acpfsScope(namespace, runName), requestID)
		switch {
		case err != nil:
			http.Error(w, err.Error(), http.StatusGone)
			return
		case resp != nil:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		if time.Now().After(deadline) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(defaultFSPollInterval):
		}
	}
}

// fsRelayOK guards both fs handlers, writing the unified unavailable response.
func (s *Server) fsRelayOK(w http.ResponseWriter) bool {
	if s.fsRelay == nil {
		http.Error(w, acpfs.ErrUnavailable.Error(), http.StatusNotImplemented)
		return false
	}
	return true
}

// fsWait clamps a requested long-poll duration to the server's cap.
func fsWait(secs float64) time.Duration {
	if secs <= 0 {
		return 0
	}
	d := time.Duration(secs * float64(time.Second))
	if d > maxFSLongPoll {
		return maxFSLongPoll
	}
	return d
}

// acpfsScope is the relay scope keying requests and responses for a run.
func acpfsScope(namespace, runName string) string {
	return namespace + ":" + runName
}
