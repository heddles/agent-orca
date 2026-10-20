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
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/acpfs"
)

// These handlers implement the workstation-facing half of the ACP filesystem
// relay (see internal/acpfs and docs/filesystem-access.md): the
// `aoctl acp serve` bridge polls for pending fs/* requests issued by the run
// it is watching and posts back whatever the ACP client (e.g. Zed) returned.
//
// Auth rides the ACP API's tenant middleware: the bridge must own the run
// (findRunInNamespaces enforces tenant ownership via the run's tenant label).
// The bridge is the enforcement point for workspace confinement and write
// approval — these endpoints transport the calls, they do not grant them.

// acpfsPollInterval re-checks the relay while long-polling.
const acpfsPollInterval = 250 * time.Millisecond

// expiredFSRequestSuffix matches the relay's expired-request error so 410
// semantics survive the Relay interface without a sentinel error type.
const expiredFSRequestSuffix = "not found (expired)"

// filesystemManifestPolicy projects Agent.spec.filesystem onto the ACP agent
// manifest. Empty Mode (pre-enum CRs) is treated as readOnly, mirroring the
// controller's conservative resolve.
func filesystemManifestPolicy(fs *agentorcav1alpha1.FilesystemAccessSpec) *ACPFilesystemPolicy {
	if fs == nil {
		return nil
	}
	mode := string(fs.Mode)
	if mode == "" {
		mode = string(agentorcav1alpha1.FilesystemReadOnly)
	}
	p := &ACPFilesystemPolicy{Mode: mode, WriteApproval: fs.WriteApproval}
	if fs.RememberApproval != nil {
		remember := *fs.RememberApproval
		p.RememberApproval = &remember
	}
	return p
}

// SetFilesystemRelay wires the fs/* relay. Call with nil to disable
// filesystem access (fs endpoints then return 501).
func (s *ACPServer) SetFilesystemRelay(relay acpfs.Relay) {
	s.fsRelay = relay
}

// handleFSPending handles GET /runs/{run_id}/fs/pending — the bridge's poll
// for requests awaiting a workstation answer. Accepts an optional
// `?wait=<seconds>` long poll.
func (s *ACPServer) handleFSPending(w http.ResponseWriter, r *http.Request, tenant *TenantIdentity, runID string) {
	if s.fsRelay == nil {
		writeACPError(w, "unavailable", acpfs.ErrUnavailable.Error(), http.StatusNotImplemented)
		return
	}
	if r.Method != http.MethodGet {
		writeACPError(w, "invalid_input", "GET required", http.StatusMethodNotAllowed)
		return
	}
	run, err := findRunInNamespaces(r.Context(), s.crdClient, tenant, runID)
	if err != nil {
		writeACPError(w, "not_found", "run not found", http.StatusNotFound)
		return
	}
	scope := acpfsScope(run.Namespace, run.Name)

	wait := fsWait(0) // no long poll unless ?wait= is passed
	if v := r.URL.Query().Get("wait"); v != "" {
		if secs, perr := strconv.ParseFloat(v, 64); perr == nil {
			wait = fsWait(secs)
		}
	}
	deadline := time.Now().Add(wait)
	for {
		reqs, err := s.fsRelay.Pending(r.Context(), scope)
		if err != nil {
			slog.Error("listing pending fs requests", "run", run.Name, "err", err)
			writeACPError(w, "server_error", "listing pending fs requests", http.StatusInternalServerError)
			return
		}
		if len(reqs) > 0 {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"requests": reqs})
			return
		}
		if time.Now().After(deadline) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"requests": []acpfs.Request{}})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(acpfsPollInterval):
		}
	}
}

// handleFSRespond handles POST /runs/{run_id}/fs/pending/{request_id}/response
// — the bridge's answer for one fs request.
func (s *ACPServer) handleFSRespond(w http.ResponseWriter, r *http.Request, tenant *TenantIdentity, runID, requestID string) {
	if s.fsRelay == nil {
		writeACPError(w, "unavailable", acpfs.ErrUnavailable.Error(), http.StatusNotImplemented)
		return
	}
	if requestID == "" {
		writeACPError(w, "invalid_input", "request_id required", http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodPost {
		writeACPError(w, "invalid_input", "POST required", http.StatusMethodNotAllowed)
		return
	}
	run, err := findRunInNamespaces(r.Context(), s.crdClient, tenant, runID)
	if err != nil {
		writeACPError(w, "not_found", "run not found", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeACPError(w, "invalid_input", "reading body", http.StatusBadRequest)
		return
	}
	var resp acpfs.Response
	if err := json.Unmarshal(body, &resp); err != nil {
		writeACPError(w, "invalid_input", "invalid JSON", http.StatusBadRequest)
		return
	}

	if err := s.fsRelay.Respond(r.Context(), acpfsScope(run.Namespace, run.Name), requestID, &resp); err != nil {
		if strings.HasSuffix(err.Error(), expiredFSRequestSuffix) {
			writeACPError(w, "not_found", err.Error(), http.StatusNotFound)
			return
		}
		slog.Error("storing fs response", "run", run.Name, "request", requestID, "err", err)
		writeACPError(w, "server_error", "storing fs response", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
